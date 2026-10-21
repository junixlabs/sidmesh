package engine

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	sidmesh "github.com/junixlabs/sidmesh/go"
	"github.com/junixlabs/sidmesh/go/project"
)

// HumanUpdate is the Temporal Update that resolves a human step (SPEC §5 "Human").
const HumanUpdate = "human"

// --- signals and the human update -------------------------------------------------------

// listen starts one receiver per declared signal (sorted, so coroutine order is
// deterministic) and registers the human update handler.
func (r *run) listen() error {
	for _, name := range slices.Sorted(maps.Keys(r.def.Signals)) {
		ch := workflow.GetSignalChannel(r.ctx, name)
		workflow.Go(r.ctx, func(ctx workflow.Context) {
			for {
				var payload any
				ch.Receive(ctx, &payload)
				r.onSignal(ctx, name, payload)
			}
		})
	}
	return workflow.SetUpdateHandlerWithOptions(r.ctx, HumanUpdate,
		func(ctx workflow.Context, res Resolution) error {
			if err := r.checkResolution(res); err != nil {
				return err
			}
			r.resolved = append(r.resolved, res)
			r.humanStep = "" // one resolution per wait
			return nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(ctx workflow.Context, res Resolution) error { return r.checkResolution(res) },
		})
}

// signalIDKey is the payload member in which a signal step sends its delivery id (SPEC §5
// "Signal step"). The receiver removes it before validation.
const signalIDKey = "$sidmesh_id"

// seenLimit bounds how many delivery ids a run remembers. A duplicate comes from a retry of
// the same send, minutes later at most, so the newest ids are the ones that matter.
const seenLimit = 1000

// onSignal keeps a valid payload; an invalid one is dropped with an alert, and one whose
// delivery id was already accepted is dropped silently.
func (r *run) onSignal(ctx workflow.Context, name string, payload any) {
	id := ""
	if m, ok := payload.(map[string]any); ok {
		if v, ok := m[signalIDKey]; ok {
			id, _ = v.(string)
			m = maps.Clone(m)
			delete(m, signalIDKey)
			payload = m
		}
	}
	if id != "" && r.seen[id] {
		workflow.GetLogger(ctx).Info("duplicate signal dropped", "signal", name, "id", id)
		return
	}
	schema, err := r.schema("signal/"+name, r.def.Signals[name].Schema)
	if err == nil {
		err = project.Validate(schema, payload)
	}
	if err != nil {
		r.alert(ctx, AlertRecord{Kind: AlertSignalInvalid, Alert: name, Message: err.Error()})
		return
	}
	r.arrivals++
	r.queues[name] = append(r.queues[name], queued{seq: r.arrivals, name: name, payload: payload})
	r.signals[name] = payload
	r.view.signals[name] = typed(payload, r.def.Signals[name].Schema)
	if id != "" {
		r.remember(id)
	}
	if r.def.Signals[name].Goto != "" {
		r.interrupts = append(r.interrupts, name)
	}
}

// remember records an accepted delivery id, forgetting the oldest beyond seenLimit.
func (r *run) remember(id string) {
	r.seen[id] = true
	r.seenOrder = append(r.seenOrder, id)
	if len(r.seenOrder) > seenLimit {
		delete(r.seen, r.seenOrder[0])
		r.seenOrder = r.seenOrder[1:]
	}
}

// checkResolution is the update validator: it must not change state.
func (r *run) checkResolution(res Resolution) error {
	if r.humanStep == "" || res.StepID != r.humanStep {
		return fmt.Errorf("workflow is not waiting on human step %q", res.StepID)
	}
	step := r.def.Steps[res.StepID]
	doc, ok := step.Outcomes[res.Outcome]
	if !ok {
		return fmt.Errorf("outcome %q is not one of step %s's outcomes", res.Outcome, res.StepID)
	}
	schema, err := r.schema("human/"+res.StepID+"/"+res.Outcome, doc)
	if err != nil {
		return err
	}
	output := res.Output
	if output == nil {
		output = map[string]any{}
	}
	if err := project.Validate(schema, output); err != nil {
		return fmt.Errorf("output: %w", err)
	}
	return nil
}

// interrupted reports whether a goto signal is waiting to be applied.
func (r *run) interrupted() bool { return len(r.interrupts) > 0 }

// takeInterrupt pops the oldest goto signal and returns it with the step it jumps to.
func (r *run) takeInterrupt() (signal, next string, ok bool) {
	if len(r.interrupts) == 0 {
		return "", "", false
	}
	signal = r.interrupts[0]
	r.interrupts = r.interrupts[1:]
	return signal, r.def.Signals[signal].Goto, true
}

func (r *run) popQueue(name string) queued {
	q := r.queues[name]
	r.queues[name] = q[1:]
	return q[0]
}

// takeSignals moves signals already delivered to this workflow task into the queues, so
// none is left unread when the run ends.
func (r *run) takeSignals() {
	for _, name := range slices.Sorted(maps.Keys(r.def.Signals)) {
		ch := workflow.GetSignalChannel(r.ctx, name)
		for {
			var payload any
			if !ch.ReceiveAsync(&payload) {
				break
			}
			r.onSignal(r.ctx, name, payload)
		}
	}
}

// --- wait / human ----------------------------------------------------------------------

func (r *run) wait(id string, s project.Step) (string, error) {
	stop := r.watchSLA(id, s.SLA)
	defer stop()

	if s.Duration != nil {
		d, err := r.durationOf(*s.Duration)
		if err != nil {
			return "", err
		}
		if _, err := workflow.AwaitWithTimeout(r.ctx, d, r.interrupted); err != nil {
			return "", err
		}
		if _, next, ok := r.takeInterrupt(); ok {
			return next, nil
		}
		r.setStep(id, "elapsed", map[string]any{})
		return s.On["elapsed"], nil
	}

	ready := func() bool { return len(r.queues[s.Signal]) > 0 || r.interrupted() }
	tb, err := s.WaitTimeout()
	if err != nil {
		return "", err
	}
	if tb != nil {
		d, err := r.durationOf(*tb)
		if err != nil {
			return "", err
		}
		if _, err := workflow.AwaitWithTimeout(r.ctx, d, ready); err != nil {
			return "", err
		}
	} else if err := workflow.Await(r.ctx, ready); err != nil {
		return "", err
	}
	if _, next, ok := r.takeInterrupt(); ok {
		return next, nil
	}
	if len(r.queues[s.Signal]) == 0 {
		r.setStep(id, "timeout", map[string]any{})
		return s.On["timeout"], nil
	}
	first := r.popQueue(s.Signal)
	if s.Collect == nil {
		r.setStep(id, "received", first.payload)
		return s.On["received"], nil
	}
	return r.collect(id, s, first)
}

// collect keeps consuming the wait's signal after the first payload until collect.max
// payloads or collect.window (counted from the first payload) has passed. A goto while
// collecting puts what it took back at the front of the queue, in arrival order.
func (r *run) collect(id string, s project.Step, first queued) (string, error) {
	window, err := r.durationOf(s.Collect.Window)
	if err != nil {
		return "", err
	}
	max := 0
	if s.Collect.Max != nil {
		v, err := r.exprs.resolve(*s.Collect.Max, r.scope(nil))
		if err != nil {
			return "", fmt.Errorf("collect max: %w", err)
		}
		f, ok := toFloat(v)
		if !ok || f != math.Trunc(f) || f < 2 {
			return "", fmt.Errorf("collect max %v is not an integer of at least 2", v)
		}
		max = int(f)
	}
	taken := []queued{first}
	full := func() bool { return max > 0 && len(taken) >= max }
	deadline := workflow.Now(r.ctx).Add(window)
	for !full() {
		left := deadline.Sub(workflow.Now(r.ctx))
		if left <= 0 {
			break
		}
		ok, err := workflow.AwaitWithTimeout(r.ctx, left, func() bool {
			return len(r.queues[s.Signal]) > 0 || r.interrupted()
		})
		if err != nil {
			return "", err
		}
		if _, next, ok := r.takeInterrupt(); ok {
			if r.since(changeCollectRequeue) {
				r.queues[s.Signal] = append(taken, r.queues[s.Signal]...)
			}
			return next, nil
		}
		if !ok {
			break
		}
		for len(r.queues[s.Signal]) > 0 && !full() {
			taken = append(taken, r.popQueue(s.Signal))
		}
	}
	items := make([]any, len(taken))
	for i, q := range taken {
		items[i] = q.payload
	}
	r.setStep(id, "received", map[string]any{"items": items})
	return s.On["received"], nil
}

func (r *run) human(id string, s project.Step) (string, error) {
	stop := r.watchSLA(id, s.SLA)
	defer stop()
	input, err := r.exprs.resolveMap(s.Input, r.scope(nil))
	if err != nil {
		return "", err
	}
	outcomes := slices.Sorted(maps.Keys(s.Outcomes))
	taskID := fmt.Sprintf("%s#%s#%d", r.id, id, r.executions[id])
	task := func(event string) TaskRecord {
		return TaskRecord{
			TaskID: taskID, Tenant: r.in.Tenant, WorkflowID: r.id, StepID: id, Task: s.Task,
			Event: event, Time: workflow.Now(r.ctx),
		}
	}
	opened := task(TaskOpened)
	opened.Input, opened.Outcomes = input, outcomes
	r.emit(SinkRecord{Task: &opened})
	// Waiting starts before the flush: a resolution may arrive as soon as the task is visible.
	r.humanStep, r.resolved = id, nil
	r.flush() // the task must be visible while the workflow waits on it

	err = workflow.Await(r.ctx, func() bool { return len(r.resolved) > 0 || r.interrupted() })
	r.humanStep = ""
	if err != nil {
		return "", err
	}
	if len(r.resolved) == 0 {
		sig, next, _ := r.takeInterrupt()
		rec := task(TaskInterrupted)
		rec.Reason = "signal " + sig
		r.emit(SinkRecord{Task: &rec})
		return next, nil
	}
	res := r.resolved[0]
	r.resolved = nil
	rec := task(TaskResolved)
	rec.Outcome, rec.Output, rec.Actor = res.Outcome, res.Output, res.Actor
	r.emit(SinkRecord{Task: &rec})
	r.setStep(id, res.Outcome, res.Output)
	return s.On[res.Outcome], nil
}

// --- spawn / signal / compensate -------------------------------------------------------

func (r *run) spawn(id string, s project.Step) (string, error) {
	key := sidmesh.Key(s.Definition, s.Version)
	childDef, ok := r.definition(key)
	if !ok {
		return "", fmt.Errorf("definition %s is not pinned", key)
	}
	base := r.scope(nil)
	items := []any{nil}
	if s.Foreach != nil {
		v, err := r.exprs.resolve(*s.Foreach, base)
		if err != nil {
			return "", fmt.Errorf("foreach: %w", err)
		}
		if s.Foreach.Expr != "" {
			v = typed(v, nil) // an expression result has lost its schema
		}
		if v == nil {
			items = nil
		} else if items, ok = v.([]any); !ok {
			return "", fmt.Errorf("foreach is %T, not an array", v)
		}
	}

	policy := enumspb.PARENT_CLOSE_POLICY_TERMINATE
	if s.Wait == "none" {
		policy = enumspb.PARENT_CLOSE_POLICY_ABANDON
	}
	all := make([]child, 0, len(items))
	start := func(childKey, childID string, cin StartInput) {
		cctx := workflow.WithChildOptions(r.ctx, workflow.ChildWorkflowOptions{
			WorkflowID:        childID,
			TaskQueue:         r.in.Queues.Workflow,
			ParentClosePolicy: policy,
		})
		c := child{key: childKey, id: childID, future: workflow.ExecuteChildWorkflow(cctx, WorkflowName, cin)}
		r.children[id] = append(r.children[id], c)
		all = append(all, c)
	}
	// With changeSpawnIDs every child is resolved and its key checked before any starts.
	checked := r.since(changeSpawnIDs)
	type planned struct {
		key, id string
		in      StartInput
	}
	var plan []planned
	keys := map[string]bool{}
	prefix := r.id + "/" + id
	if n := r.executions[id]; checked && n > 1 {
		prefix += fmt.Sprintf("#%d", n)
	}
	for _, item := range items {
		sc := maps.Clone(base)
		sc["item"] = item
		k, err := r.exprs.resolve(*s.Key, sc)
		if err != nil {
			return "", fmt.Errorf("key: %w", err)
		}
		childKey := toText(k)
		input, err := r.exprs.resolveMap(s.Input, sc)
		if err != nil {
			return "", err
		}
		// Only the spawn's own params: the parent's overrides are not inherited.
		params, err := r.exprs.resolveMap(s.Params, sc)
		if err != nil {
			return "", fmt.Errorf("params: %w", err)
		}
		cin := r.startInput(key, childDef, input, params)
		childID := prefix + "/" + childKey
		if !checked {
			start(childKey, childID, cin)
			continue
		}
		if keys[childKey] {
			return "", fmt.Errorf("spawn key %q is not unique: no child started", childKey)
		}
		keys[childKey] = true
		plan = append(plan, planned{key: childKey, id: childID, in: cin})
	}
	for _, p := range plan {
		start(p.key, p.id, p.in)
	}

	if s.Wait == "none" {
		for _, c := range all {
			if err := c.future.GetChildWorkflowExecution().Get(r.ctx, nil); err != nil {
				return "", fmt.Errorf("start child %s: %w", c.key, err)
			}
		}
		r.setStep(id, "started", map[string]any{"count": float64(len(all))})
		return s.Next, nil
	}

	// Interrupts apply while waiting; the children keep running.
	err := workflow.Await(r.ctx, func() bool {
		if r.interrupted() {
			return true
		}
		for _, c := range all {
			if !c.future.IsReady() {
				return false
			}
		}
		return true
	})
	if err != nil {
		return "", err
	}
	if _, next, ok := r.takeInterrupt(); ok {
		return next, nil
	}
	results := make([]any, 0, len(all))
	anyFailed := false
	for _, c := range all {
		var res Result
		status := "failed"
		var output any
		if err := c.future.Get(r.ctx, &res); err == nil {
			status, output = res.Status, res.Output
		} else {
			workflow.GetLogger(r.ctx).Warn("child workflow failed", "child", c.key, "error", err)
		}
		if status == "failed" {
			anyFailed = true
		}
		results = append(results, map[string]any{"key": c.key, "status": status, "output": output})
	}
	r.setStep(id, "ended", map[string]any{"children": results})
	if anyFailed && s.OnChildFailed != "" {
		return s.OnChildFailed, nil
	}
	return s.Next, nil
}

func (r *run) signal(s project.Step) (string, error) {
	to, err := s.Target()
	if err != nil {
		return "", err
	}
	sc := r.scope(nil)
	payload, err := r.exprs.resolveMap(s.Payload, sc)
	if err != nil {
		return "", err
	}
	if r.since(changeSignalIDs) {
		// Deterministic per send, so a retried signal-with-start repeats the same id and the
		// receiver drops the copy.
		info := workflow.GetInfo(r.ctx)
		payload[signalIDKey] = fmt.Sprintf("%s/%s/%s/%d", r.id, info.FirstRunID, r.stepID, r.executions[r.stepID])
	}
	switch {
	case to.Parent:
		if r.parentID == "" {
			return "", temporal.NewNonRetryableApplicationError("signal to parent from a workflow without a parent", ErrNoParent, nil)
		}
		return s.Next, r.signalExternal(r.parentID, s.Name, payload)

	case to.Children != "":
		for _, c := range r.children[to.Children] {
			if c.future.IsReady() {
				continue // ended
			}
			// A child may end between the check and the signal.
			err := c.future.SignalChildWorkflow(r.ctx, s.Name, payload).Get(r.ctx, nil)
			if err := r.delivered(c.id, s.Name, err); err != nil {
				return "", err
			}
		}
		return s.Next, nil
	}

	v, err := r.exprs.resolve(*to.Workflow, sc)
	if err != nil {
		return "", err
	}
	target := toText(v)
	if s.Start == nil {
		return s.Next, r.signalExternal(target, s.Name, payload)
	}
	key := sidmesh.Key(s.Start.Definition, s.Start.Version)
	def, ok := r.definition(key)
	if !ok {
		return "", fmt.Errorf("definition %s is not pinned", key)
	}
	input, err := r.exprs.resolveMap(s.Start.Input, sc)
	if err != nil {
		return "", fmt.Errorf("start input: %w", err)
	}
	params, err := r.exprs.resolveMap(s.Start.Params, sc)
	if err != nil {
		return "", fmt.Errorf("start params: %w", err)
	}
	if err := validateStart(def, input, params); err != nil {
		return "", temporal.NewNonRetryableApplicationError(err.Error(), ErrStartInvalid, nil)
	}
	req := SignalWithStartRequest{
		WorkflowID: target,
		Signal:     s.Name,
		Payload:    payload,
		Once:       s.Start.Once,
		Start:      r.startInput(key, def, input, params),
	}
	ctx := workflow.WithActivityOptions(r.ctx, workflow.ActivityOptions{
		TaskQueue:           r.in.Queues.Workflow,
		StartToCloseTimeout: defaultTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: int32(defaultPolicy.maxAttempts)},
	})
	if err := workflow.ExecuteActivity(ctx, SignalWithStartActivityName, req).Get(ctx, nil); err != nil {
		return "", fmt.Errorf("signal-with-start %s to %s: %w", s.Name, target, err)
	}
	return s.Next, nil
}

func (r *run) signalExternal(target, name string, payload map[string]any) error {
	err := workflow.SignalExternalWorkflow(r.ctx, target, "", name, payload).Get(r.ctx, nil)
	return r.delivered(target, name, err)
}

// delivered turns a signal's error into the step's error. A target that is not running is
// not an error: it raises an alert and the step continues.
func (r *run) delivered(target, name string, err error) error {
	var gone *temporal.UnknownExternalWorkflowExecutionError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &gone):
		r.alert(r.ctx, AlertRecord{
			StepID: r.stepID, Kind: AlertTargetNotRunning, Alert: name,
			Message: "signal " + name + " to " + target + ": target is not running",
		})
		return nil
	default:
		return fmt.Errorf("signal %s to %s: %w", name, target, err)
	}
}

func (r *run) compensate(id string, s project.Step) (string, error) {
	done, failed := 0, 0
	for i := len(r.comps) - 1; i >= 0; i-- {
		c := r.comps[i]
		if c.done {
			continue
		}
		c.done = true
		info, ok := r.contract(sidmesh.Key(c.capability, c.version))
		if !ok {
			return "", fmt.Errorf("compensation %s is not pinned", sidmesh.Key(c.capability, c.version))
		}
		meta := sidmesh.Meta{WorkflowID: r.id, StepID: c.stepID, Attempt: 1, Tenant: r.in.Tenant}
		if info.Effects == "write" {
			key, err := r.idempotencyKey(info, c.input)
			if err != nil {
				return "", err
			}
			meta.IdempotencyKey = key
		}
		req := sidmesh.CallRequest{Capability: c.capability, Version: c.version, Input: c.input, Config: c.config, Meta: meta}
		if _, err := r.callActivity(info.Runtime, defaultTimeout, defaultPolicy.maxAttempts, req); err != nil {
			failed++
			r.alert(r.ctx, AlertRecord{StepID: c.stepID, Kind: AlertCompensationFailed, Message: err.Error()})
			continue
		}
		done++
	}
	r.setStep(id, "done", map[string]any{"compensated": float64(done), "failed": float64(failed)})
	return s.Next, nil
}

// --- SLA, search attributes, sinks -----------------------------------------------------

// watchSLA raises one alert if the step is still running after sla.after. The returned
// function stops the watch; call it when the step finishes.
func (r *run) watchSLA(stepID string, sla *project.SLA) func() {
	if sla == nil {
		return func() {}
	}
	d, err := parseDuration(sla.After)
	if err != nil {
		workflow.GetLogger(r.ctx).Error("bad sla duration", "step", stepID, "error", err)
		return func() {}
	}
	run := r.executions[stepID]
	ctx, cancel := workflow.WithCancel(r.ctx)
	workflow.Go(ctx, func(ctx workflow.Context) {
		if err := workflow.Sleep(ctx, d); err != nil {
			return // the step finished first
		}
		// Disconnected so that the step finishing does not cancel an alert being written.
		alertCtx, _ := workflow.NewDisconnectedContext(ctx)
		r.alert(alertCtx, AlertRecord{
			StepID: stepID, Kind: AlertSLA, Alert: sla.Alert,
			Message:   "step still running after " + sla.After,
			DedupeKey: fmt.Sprintf("sla:%s:%s:%d:%s", r.id, stepID, run, sla.Alert), // SPEC §5 "SLA"
		})
	})
	return cancel
}

// upsertSearch writes every search attribute whose value changed. An unresolved value is
// written as "".
func (r *run) upsertSearch() error {
	if len(r.def.Search) == 0 {
		return nil
	}
	names := slices.Sorted(maps.Keys(r.def.Search))
	sc := r.scope(nil)
	var updates []temporal.SearchAttributeUpdate
	for _, name := range names {
		v, err := r.exprs.resolve(r.def.Search[name], sc)
		if err != nil {
			return fmt.Errorf("search attribute %s: %w", name, err)
		}
		text := ""
		if v != nil {
			text = toText(v)
		}
		if old, ok := r.search[name]; ok && old == text {
			continue
		}
		r.search[name] = text
		updates = append(updates, temporal.NewSearchAttributeKeyKeyword(name).ValueSet(text))
	}
	if len(updates) == 0 {
		return nil
	}
	return workflow.UpsertTypedSearchAttributes(r.ctx, updates...)
}

// alert fills the common fields and writes the alert. Without a DedupeKey the alert is
// unique to this run: kind, workflow, run and a per-run counter.
func (r *run) alert(ctx workflow.Context, a AlertRecord) {
	a.Tenant, a.WorkflowID, a.Time = r.in.Tenant, r.id, workflow.Now(ctx)
	if a.DedupeKey == "" {
		r.alerts++
		a.DedupeKey = fmt.Sprintf("%s:%s:%s:%d", a.Kind, r.id, workflow.GetInfo(ctx).WorkflowExecution.RunID, r.alerts)
	}
	if ctx == r.ctx {
		r.emit(SinkRecord{Alert: &a})
		return
	}
	r.write(ctx, []SinkRecord{{Alert: &a}}) // from a coroutine: write now
}

// emit queues a record for the current step's batch.
func (r *run) emit(rec SinkRecord) { r.buffer = append(r.buffer, rec) }

// flush writes the queued records in one local activity, so sinks add one marker per step
// to the history instead of an activity per record.
func (r *run) flush() {
	if len(r.buffer) > 0 {
		r.write(r.ctx, r.buffer)
		r.buffer = nil
	}
}

// write runs the sink local activity. A sink that keeps failing is logged; it never fails
// the workflow.
func (r *run) write(ctx workflow.Context, recs []SinkRecord) {
	ctx = workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 5},
	})
	if err := workflow.ExecuteLocalActivity(ctx, SinkActivityName, recs).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("sink write failed", "error", err)
	}
}
