package engine

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	sidmesh "github.com/junixlabs/sidmesh/go"
	"github.com/junixlabs/sidmesh/go/project"
)

// Interpreter changes that alter the commands a running workflow produces ship behind a
// workflow.GetVersion gate named here, so runs started before the change replay unchanged.
// Remove a gate only when no run older than it can still be open.
const (
	changeStartedFlush = "started-flush" // 2026-10-16: write the started event before the first step
	// 2026-10-21: every declared step id and signal name is in the binding scope, null (or a
	// record of nulls) until it has a value, so CEL reads them as null instead of failing.
	changeNullScope = "null-scope"
	// 2026-10-21: a cancelled workflow stops at the call it is in (no lookup, no routing) and
	// ends cancelled; the ended event (status cancelled) is written on a disconnected context.
	changeCancel = "cancel"
	// 2026-10-21: a goto during a wait collect puts the payloads it took back on the queue.
	changeCollectRequeue = "collect-requeue"
	// 2026-10-21: duplicate spawn keys fail the step before any child starts; executions after
	// the first add #<n> to the step id in child ids.
	changeSpawnIDs = "spawn-ids"
	// 2026-10-21: signal steps attach a delivery id to the payload (signalIDKey).
	changeSignalIDs = "signal-ids"
)

// since reports whether this run has the given change, recording it the first time it is
// asked (workflow.GetVersion): a run whose history predates the change keeps the old path.
func (r *run) since(change string) bool {
	return workflow.GetVersion(r.ctx, change, workflow.DefaultVersion, 1) >= 1
}

// Workflow failure types the interpreter raises itself.
const (
	ErrVarInvalid = "VAR_INVALID"
	ErrNoParent   = "NO_PARENT"
	// ErrStartInvalid: a signal step's start input or params do not validate.
	ErrStartInvalid = "START_INVALID"
)

// Interpret is the interpreter workflow (SPEC §5), resolving pins through d. Child workflows
// run the same function.
func (d *Docs) Interpret(ctx workflow.Context, in StartInput) (Result, error) {
	r, err := newRun(ctx, in, d)
	if err != nil {
		return Result{}, err
	}
	if r.since(changeNullScope) {
		r.nullScope = true
		for name := range r.def.Signals {
			if _, ok := r.view.signals[name]; !ok {
				r.view.signals[name] = nil
			}
		}
		for id := range r.def.Steps {
			r.steps[id] = map[string]any{"outcome": nil, "output": nil, "started_at": nil, "ended_at": nil}
		}
	}
	if err := r.listen(); err != nil {
		return Result{}, err
	}
	if err := r.loadTenantParams(); err != nil {
		return Result{}, err
	}
	// Payloads carried over by end drain arrive before the first step, in arrival order.
	for _, p := range in.Pending {
		r.onSignal(ctx, p.Name, p.Payload)
	}
	if err := r.upsertSearch(); err != nil {
		return Result{}, err
	}
	r.event(EventStarted, "", nil)
	if workflow.GetVersion(ctx, changeStartedFlush, workflow.DefaultVersion, 1) >= 1 {
		r.flush() // a run that waits before its first step ends is visible at once
	}
	res, err := r.loop()
	if err != nil && !workflow.IsContinueAsNewError(err) {
		status := "failed"
		if temporal.IsCanceledError(err) && r.since(changeCancel) {
			status = "cancelled"
		}
		r.event(EventEnded, r.stepID, func(e *EventRecord) { e.Status = status })
		if status == "cancelled" {
			// The run's own context is cancelled; the ended record must still be written.
			dctx, _ := workflow.NewDisconnectedContext(r.ctx)
			r.write(dctx, r.buffer)
			r.buffer = nil
		}
	}
	r.flush()
	return res, err
}

func (r *run) loop() (Result, error) {
	cur := r.def.Start
	for {
		if _, next, ok := r.takeInterrupt(); ok {
			cur = next
		}
		step, ok := r.def.Steps[cur]
		if !ok {
			return Result{}, fmt.Errorf("step %q is not defined", cur)
		}
		r.stepID, r.started, r.touched = cur, workflow.Now(r.ctx), false
		r.executions[cur]++
		next, end, err := r.exec(cur, step)
		if err != nil {
			// Application errors (VAR_INVALID, NO_PARENT) keep their type for the caller.
			var appErr *temporal.ApplicationError
			if errors.As(err, &appErr) {
				return Result{}, appErr
			}
			return Result{}, fmt.Errorf("step %s: %w", cur, err)
		}
		if end != nil {
			return r.finish(cur, step, *end)
		}
		r.stamp(cur)
		r.event(EventStep, cur, func(e *EventRecord) {
			e.StepType = step.Type
			e.Outcome, _ = r.steps[cur]["outcome"].(string)
			e.StartedAt, e.EndedAt = r.started, workflow.Now(r.ctx)
		})
		r.flush()
		if err := r.upsertSearch(); err != nil {
			return Result{}, err
		}
		cur = next
	}
}

// finish ends the run at end step id, or continues as new when a drain signal still has
// queued payloads (SPEC §5 "End").
func (r *run) finish(id string, s project.Step, res Result) (Result, error) {
	r.takeSignals()
	drain := false
	for _, name := range s.Drain {
		if len(r.queues[name]) > 0 {
			drain = true
		}
	}
	if !drain {
		r.event(EventEnded, id, func(e *EventRecord) { e.StepType, e.Status = s.Type, res.Status })
		return res, nil
	}
	// Drain signals carry their queues; every other signal only its latest payload.
	var pending []queued
	for _, name := range s.Drain {
		pending = append(pending, r.queues[name]...)
	}
	slices.SortFunc(pending, func(a, b queued) int { return a.seq - b.seq })
	next := r.in
	next.Pending = make([]PendingSignal, len(pending))
	for i, q := range pending {
		next.Pending[i] = PendingSignal{Name: q.name, Payload: q.payload}
	}
	next.Latest = map[string]any{}
	for name, v := range r.signals {
		if !slices.Contains(s.Drain, name) {
			next.Latest[name] = v
		}
	}
	next.Executions = r.executions
	next.Seen = r.seenOrder
	r.event(EventContinued, id, func(e *EventRecord) { e.StepType, e.Status = s.Type, res.Status })
	return Result{}, workflow.NewContinueAsNewError(r.ctx, WorkflowName, next)
}

// run is the state of one workflow execution.
type run struct {
	ctx      workflow.Context
	in       StartInput
	src      docSource
	def      project.Definition
	params   map[string]any
	vars     map[string]any
	steps    map[string]map[string]any // step id -> {outcome, output (typed), started_at, ended_at}; never mutated, replaced
	signals  map[string]any            // signal name -> latest valid payload
	queues   map[string][]queued       // signal name -> valid payloads not yet consumed
	schemas  map[string]*jsonschema.Schema
	children map[string][]child // spawn step id -> every child it started
	comps    []*compensation    // in completion order
	search   map[string]string
	exprs    *exprs
	id       string
	parentID string

	// view holds the binding roots typed by their schemas (SPEC §5 "Numbers in CEL"), updated
	// where the run changes them, so scope only assembles. Values in it are never mutated.
	view struct {
		input, params, vars, signals map[string]any
	}
	nullScope bool                    // changeNullScope
	contracts map[string]ContractInfo // resolved contracts by capability@version

	seen      map[string]bool // delivery ids of accepted signal payloads (signalIDKey)
	seenOrder []string        // the same ids, oldest first, at most seenLimit

	arrivals   int            // signals received so far, orders queued payloads across names
	executions map[string]int // step id -> times it started, across continue-as-new
	alerts     int            // alerts raised by this run, part of their dedupe keys
	buffer     []SinkRecord   // records of the current step, written together by flush

	interrupts []string     // goto signals not yet applied
	humanStep  string       // human step currently waiting, "" if none
	resolved   []Resolution // accepted human updates not yet consumed

	stepID  string    // the current step
	started time.Time // workflow time the current step began
	touched bool      // the current step recorded an outcome
}

type queued struct {
	seq     int
	name    string
	payload any
}

type child struct {
	key    string // the spawn's key binding
	id     string
	future workflow.ChildWorkflowFuture
}

type compensation struct {
	stepID     string
	capability string
	version    int
	input      map[string]any
	config     map[string]any // {} when the compensate binds none
	done       bool
}

func newRun(ctx workflow.Context, in StartInput, docs *Docs) (*run, error) {
	e, err := sharedExprs()
	if err != nil {
		return nil, err
	}
	info := workflow.GetInfo(ctx)
	parent := ""
	if info.ParentWorkflowExecution != nil {
		parent = info.ParentWorkflowExecution.ID
	}
	if in.Input == nil {
		in.Input = map[string]any{}
	}
	r := &run{
		ctx:        ctx,
		in:         in,
		steps:      map[string]map[string]any{},
		signals:    map[string]any{},
		queues:     map[string][]queued{},
		executions: map[string]int{},
		schemas:    map[string]*jsonschema.Schema{},
		children:   map[string][]child{},
		search:     map[string]string{},
		contracts:  map[string]ContractInfo{},
		seen:       map[string]bool{},
		exprs:      e,
		id:         info.WorkflowExecution.ID,
		parentID:   parent,
	}
	if in.Pins == nil {
		r.src = carriedDocs{in: in}
	} else {
		r.src = &pinnedDocs{ctx: ctx, docs: docs, in: in}
	}
	if r.def, err = r.src.root(); err != nil {
		return nil, err
	}
	r.vars = map[string]any{}
	for name, v := range r.def.Vars {
		r.vars[name] = v.Default
	}
	for id, n := range in.Executions {
		r.executions[id] = n
	}
	for name, v := range in.Latest {
		r.signals[name] = v
	}
	for _, id := range in.Seen {
		r.remember(id)
	}
	r.view.input = typed(in.Input, r.def.Input).(map[string]any)
	r.view.vars = typedEach(r.vars, r.def.Vars, func(v project.Var) map[string]any { return v.Schema })
	r.view.signals = typedEach(r.signals, r.def.Signals, func(s project.Signal) map[string]any { return s.Schema })
	return r, nil
}

// mergeParams layers the effective params: defaults < tenant params < overrides.
func mergeParams(def project.Definition, tenant, overrides map[string]any) map[string]any {
	out := map[string]any{}
	for name, p := range def.Params {
		out[name] = p.Default
	}
	for _, layer := range []map[string]any{tenant, overrides} {
		for name, v := range layer {
			out[name] = v
		}
	}
	return out
}

// loadTenantParams reads the tenant's stored params once per run through the params local
// activity (recorded in history). Undeclared or invalid values are dropped with an alert.
func (r *run) loadTenantParams() error {
	ctx := workflow.WithLocalActivityOptions(r.ctx, workflow.LocalActivityOptions{
		ScheduleToCloseTimeout: time.Minute,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 5},
	})
	var stored map[string]any
	req := ParamsRequest{Tenant: r.in.Tenant, Definition: r.def.ID, Version: r.def.Version}
	if err := workflow.ExecuteLocalActivity(ctx, ParamsActivityName, req).Get(ctx, &stored); err != nil {
		return fmt.Errorf("tenant params: %w", err)
	}
	valid := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(stored)) {
		decl, ok := r.def.Params[name]
		err := fmt.Errorf("param %q is not declared", name)
		if ok {
			var schema *jsonschema.Schema
			if schema, err = r.schema("param/"+name, decl.Schema); err == nil {
				err = project.Validate(schema, stored[name])
			}
		}
		if err != nil {
			r.alert(r.ctx, AlertRecord{Kind: AlertTenantParamsInvalid, Alert: name, Message: err.Error()})
			continue
		}
		valid[name] = stored[name]
	}
	r.params = mergeParams(r.def, valid, r.in.Params)
	r.view.params = typedEach(r.params, r.def.Params, func(p project.Param) map[string]any { return p.Schema })
	return nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// event writes one events-sink record; fill sets the kind-specific fields.
func (r *run) event(kind, stepID string, fill func(*EventRecord)) {
	e := EventRecord{
		Time: workflow.Now(r.ctx), Tenant: r.in.Tenant, WorkflowID: r.id, ParentID: r.parentID,
		Definition: r.def.ID, Version: r.def.Version, Kind: kind, StepID: stepID,
	}
	if fill != nil {
		fill(&e)
	}
	r.emit(SinkRecord{Event: &e})
}

// scope assembles the binding roots of SPEC §5 from the typed view: shallow copies of the
// maps the run keeps changing, so a resolved value never changes after the fact.
// workflow.now is the current workflow time. item is already typed by the caller.
func (r *run) scope(item any) map[string]any {
	steps := make(map[string]any, len(r.steps))
	for id, m := range r.steps {
		steps[id] = m
	}
	return map[string]any{
		"input":   r.view.input,
		"params":  r.view.params,
		"vars":    maps.Clone(r.view.vars),
		"steps":   steps,
		"signals": maps.Clone(r.view.signals),
		"item":    item,
		"workflow": map[string]any{
			"id": r.id, "parent_id": r.parentID, "tenant": r.in.Tenant, "now": formatTime(workflow.Now(r.ctx)),
		},
	}
}

// typedEach types each value by the schema of its declaration in decls.
func typedEach[D any](values map[string]any, decls map[string]D, schema func(D) map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for name, v := range values {
		out[name] = typed(v, schema(decls[name]))
	}
	return out
}

// docSource resolves the documents of a run: pinned by content hash, or carried in the start
// input by a run started before pinning. newRun picks one.
type docSource interface {
	root() (project.Definition, error)                // the run's own definition
	definition(key string) (project.Definition, bool) // id@version
	contract(key string) (ContractInfo, bool)         // capability@version
	// childInput sets the documents of in, the input of a run this one starts with def.
	childInput(in *StartInput, key string, def project.Definition)
	// schema compiles a schema of the root definition; name labels its validation errors.
	schema(name string, doc map[string]any) (*jsonschema.Schema, error)
}

// pinnedDocs resolves the start input's pins through Docs; a run it starts gets the same pins.
type pinnedDocs struct {
	ctx     workflow.Context
	docs    *Docs
	in      StartInput
	rootDoc *resolved
}

func (p *pinnedDocs) root() (project.Definition, error) {
	doc, ok := p.resolve(strings.TrimPrefix(p.in.Definition.Key, project.DefinitionPin))
	if !ok {
		return project.Definition{}, fmt.Errorf("root definition %s is not pinned", p.in.Definition.Key)
	}
	p.rootDoc = doc
	return *doc.def, nil
}

func (p *pinnedDocs) resolve(key string) (*resolved, bool) {
	pk := project.DefinitionPin + key
	hash, ok := p.in.Pins[pk]
	if !ok {
		return nil, false
	}
	return p.docs.get(p.ctx, p.in.Tenant, pk, hash), true
}

func (p *pinnedDocs) definition(key string) (project.Definition, bool) {
	doc, ok := p.resolve(key)
	if !ok {
		return project.Definition{}, false
	}
	return *doc.def, true
}

func (p *pinnedDocs) contract(key string) (ContractInfo, bool) {
	pk := project.ContractPin + key
	hash, ok := p.in.Pins[pk]
	if !ok {
		return ContractInfo{}, false
	}
	info := contractInfo(p.docs.get(p.ctx, p.in.Tenant, pk, hash).con)
	info.Runtime = p.docs.runtime(p.ctx, p.in, nodeOf(info.Capability))
	return info, true
}

func (p *pinnedDocs) childInput(in *StartInput, key string, _ project.Definition) {
	in.Definition.Key, in.Pins = project.DefinitionPin+key, p.in.Pins
}

// schema compiles once per process: the compiled schema lives with the pinned document.
func (p *pinnedDocs) schema(name string, doc map[string]any) (*jsonschema.Schema, error) {
	return p.rootDoc.schema(name, doc)
}

// carriedDocs reads the documents a pre-pinning run carries; a run it starts carries the same.
type carriedDocs struct{ in StartInput }

func (c carriedDocs) root() (project.Definition, error) { return *c.in.Definition.Doc, nil }

func (c carriedDocs) definition(key string) (project.Definition, bool) {
	d, ok := c.in.Definitions[key]
	return d, ok
}

func (c carriedDocs) contract(key string) (ContractInfo, bool) {
	info, ok := c.in.Contracts[key]
	return info, ok
}

func (c carriedDocs) childInput(in *StartInput, _ string, def project.Definition) {
	in.Definition.Doc, in.Definitions, in.Contracts = &def, c.in.Definitions, c.in.Contracts
}

func (c carriedDocs) schema(name string, doc map[string]any) (*jsonschema.Schema, error) {
	return compileShared(name, doc)
}

// definition returns definition id@version.
func (r *run) definition(key string) (project.Definition, bool) { return r.src.definition(key) }

// contract returns what the interpreter needs of capability@version, resolved once per run.
func (r *run) contract(key string) (ContractInfo, bool) {
	if c, ok := r.contracts[key]; ok {
		return c, true
	}
	info, ok := r.src.contract(key)
	if !ok {
		return ContractInfo{}, false
	}
	r.contracts[key] = info
	return info, true
}

// startInput is the input of a run this one starts (child or signal-start): the same pins,
// or, for a pre-pinning run, the same carried documents.
func (r *run) startInput(key string, def project.Definition, input, params map[string]any) StartInput {
	in := StartInput{Tenant: r.in.Tenant, Queues: r.in.Queues, Input: input, Params: params}
	r.src.childInput(&in, key, def)
	return in
}

// outputSchema is the schema of a step's output for the outcome it recorded, or nil.
func (r *run) outputSchema(id, outcome string) map[string]any {
	s := r.def.Steps[id]
	switch s.Type {
	case "call":
		info, _ := r.contract(sidmesh.Key(s.Capability, s.Version))
		return info.Outputs[outcome]
	case "human":
		return s.Outcomes[outcome]
	case "wait":
		if s.Signal == "" || outcome != "received" {
			return nil
		}
		sig := r.def.Signals[s.Signal].Schema
		if s.Collect != nil {
			return map[string]any{"type": "object", "properties": map[string]any{
				"items": map[string]any{"type": "array", "items": sig},
			}}
		}
		return sig
	}
	return nil
}

// setStep records the current step's outcome and output, typed by its schema, with its times.
func (r *run) setStep(id, outcome string, output any) {
	r.steps[id] = map[string]any{
		"outcome":    outcome,
		"output":     typed(output, r.outputSchema(id, outcome)),
		"started_at": formatTime(r.started),
		"ended_at":   formatTime(workflow.Now(r.ctx)),
	}
	r.touched = true
}

// stamp finishes the step record after the step ran. A step that recorded no outcome this
// time (decide, set, signal, an interrupted wait) gets times only.
func (r *run) stamp(id string) {
	now := formatTime(workflow.Now(r.ctx))
	if rec, ok := r.steps[id]; ok && r.touched {
		rec = maps.Clone(rec)
		rec["ended_at"] = now
		r.steps[id] = rec
		return
	}
	rec := map[string]any{"started_at": formatTime(r.started), "ended_at": now}
	if r.nullScope {
		rec["outcome"], rec["output"] = nil, nil
	}
	r.steps[id] = rec
}

func (r *run) exec(id string, s project.Step) (string, *Result, error) {
	var next string
	var err error
	switch s.Type {
	case "call":
		next, err = r.call(id, s)
	case "decide":
		next, err = r.decide(s)
	case "set":
		next, err = r.set(s)
	case "wait":
		next, err = r.wait(id, s)
	case "human":
		next, err = r.human(id, s)
	case "spawn":
		next, err = r.spawn(id, s)
	case "signal":
		next, err = r.signal(s)
	case "compensate":
		next, err = r.compensate(id, s)
	case "end":
		out, err := r.exprs.resolveMap(s.Output, r.scope(nil))
		if err != nil {
			return "", nil, err
		}
		return "", &Result{Status: s.Status, Output: out}, nil
	default:
		err = fmt.Errorf("unknown step type %q", s.Type)
	}
	return next, nil, err
}

func (r *run) decide(s project.Step) (string, error) {
	sc := r.scope(nil)
	for _, c := range s.Cases {
		ok, err := r.exprs.bool(c.When, sc)
		if err != nil {
			return "", err
		}
		if ok {
			return c.Next, nil
		}
	}
	return s.Else, nil
}

// set resolves every value, validates all of them, then assigns them together.
func (r *run) set(s project.Step) (string, error) {
	names := slices.Sorted(maps.Keys(s.Values))
	sc := r.scope(nil)
	values := make(map[string]any, len(names))
	for _, name := range names {
		decl, ok := r.def.Vars[name]
		if !ok {
			return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf("var %q is not declared", name), ErrVarInvalid, nil)
		}
		v, err := r.exprs.resolve(s.Values[name], sc)
		if err != nil {
			return "", err
		}
		schema, err := r.schema("var/"+name, decl.Schema)
		if err != nil {
			return "", err
		}
		if err := project.Validate(schema, v); err != nil {
			return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf("var %s: %v", name, err), ErrVarInvalid, nil)
		}
		values[name] = v
	}
	for name, v := range values {
		r.vars[name] = v
		r.view.vars[name] = typed(v, r.def.Vars[name].Schema)
	}
	return s.Next, nil
}

// schema compiles a schema of the run's definition, kept by the run and its document source.
func (r *run) schema(key string, doc map[string]any) (*jsonschema.Schema, error) {
	if s, ok := r.schemas[key]; ok {
		return s, nil
	}
	s, err := r.src.schema(key, doc)
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", key, err)
	}
	r.schemas[key] = s
	return s, nil
}

func (r *run) durationOf(b project.Binding) (time.Duration, error) {
	v, err := r.exprs.resolve(b, r.scope(nil))
	if err != nil {
		return 0, err
	}
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("duration %v is not a string", v)
	}
	return parseDuration(s)
}
