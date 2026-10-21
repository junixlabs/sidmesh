package engine

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	sidmesh "github.com/junixlabs/sidmesh/go"
	"github.com/junixlabs/sidmesh/go/project"
)

// Tests for the behaviour changes of SPEC v0.3.

func TestCELReadsUnsetStepsAndSignalsAsNull(t *testing.T) {
	def := definitionJSON(t, `{
	  "id": "nulls", "version": 1,
	  "input": {"type": "object", "properties": {}, "additionalProperties": false},
	  "signals": {"note": {"schema": {"type": "object", "properties": {"text": {"type": "string"}}, "additionalProperties": false}}},
	  "search": {"Stage": {"expr": "steps.nap.outcome == null ? 'start' : steps.nap.outcome"}},
	  "start": "check",
	  "steps": {
	    "check": {"type": "decide", "cases": [{"when": "signals.note == null && steps.nap.output == null", "next": "nap"}], "else": "bad"},
	    "nap": {"type": "wait", "duration": {"const": "1m"}, "on": {"elapsed": "done"}},
	    "done": {"type": "end", "status": "completed", "output": {"decided": {"expr": "steps.check.outcome == null && steps.check.ended_at != null"}}},
	    "bad": {"type": "end", "status": "failed"}
	  }
	}`)
	h := newHarness(t, newFakeCall())
	var stages []string
	h.env.OnUpsertTypedSearchAttributes(mock.Anything).Run(func(args mock.Arguments) {
		sa := args.Get(0).(temporal.SearchAttributes)
		if v, ok := sa.GetKeyword(temporal.NewSearchAttributeKeyKeyword("Stage")); ok {
			stages = append(stages, v)
		}
	}).Return(nil)
	require.NoError(t, h.execDoc(def, "acme:nulls:1", nil), "an expr search binding before the first step")
	var res Result
	require.NoError(t, h.env.GetWorkflowResult(&res))
	require.Equal(t, "completed", res.Status)
	require.Equal(t, true, res.Output["decided"], "a decide step has a null outcome, not a missing one")
	require.Equal(t, []string{"start", "elapsed"}, stages)
}

func TestCancelDuringWriteEndsCancelled(t *testing.T) {
	calls := itemFakes(0.95)
	h := newHarness(t, calls)
	h.env.OnActivity(CallActivityName, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, req sidmesh.CallRequest) (sidmesh.Result, error) {
			if req.Capability != "vendor.place_order" {
				return calls.Call(ctx, req)
			}
			h.env.CancelWorkflow()
			select { // the workflow cancels the attempt
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return sidmesh.Result{}, errFlaky
		})
	err := h.exec(StartRequest{Definition: "item", Version: 1, WorkflowID: "acme:item:30", Tenant: "acme",
		Input: map[string]any{"order_id": "1", "sku": "A"}})
	require.True(t, temporal.IsCanceledError(err), "want cancelled, got %v", err)
	require.Empty(t, calls.callsTo("vendor.find_order"), "no lookup")
	last := h.sink.events[len(h.sink.events)-1]
	require.Equal(t, EventEnded, last.Kind)
	require.Equal(t, "cancelled", last.Status)
	require.Equal(t, "place", last.StepID, "not routed on.unknown")
}

func TestCancelWhileWaitingWritesEndedEvent(t *testing.T) {
	h := newHarness(t, itemFakes(0.95))
	h.env.RegisterDelayedCallback(h.env.CancelWorkflow, time.Hour) // inside the ship wait
	err := h.exec(StartRequest{Definition: "item", Version: 1, WorkflowID: "acme:item:31", Tenant: "acme",
		Input: map[string]any{"order_id": "1", "sku": "A"}})
	require.True(t, temporal.IsCanceledError(err), "want cancelled, got %v", err)
	last := h.sink.events[len(h.sink.events)-1]
	require.Equal(t, EventEnded, last.Kind, "written although the run's context is cancelled")
	require.Equal(t, "cancelled", last.Status)
	require.Equal(t, "ship", last.StepID)
}

func TestGotoDuringCollectKeepsTakenPayloads(t *testing.T) {
	def := definitionJSON(t, `{
	  "id": "regather", "version": 1,
	  "input": {"type": "object", "properties": {}, "additionalProperties": false},
	  "signals": {
	    "add": {"schema": {"type": "object", "properties": {"name": {"type": "string"}}, "additionalProperties": false}},
	    "stop": {"schema": {"type": "object", "properties": {}, "additionalProperties": false}, "goto": "again"}
	  },
	  "start": "first",
	  "steps": {
	    "first": {"type": "wait", "signal": "add", "collect": {"window": {"const": "1h"}}, "on": {"received": "lost"}},
	    "lost": {"type": "end", "status": "failed"},
	    "again": {"type": "wait", "signal": "add", "collect": {"window": {"const": "1m"}, "max": 5},
	              "timeout": {"const": "1m"}, "on": {"received": "done", "timeout": "empty"}},
	    "done": {"type": "end", "status": "completed", "output": {"items": "steps.again.output.items"}},
	    "empty": {"type": "end", "status": "failed"}
	  }
	}`)
	h := newHarness(t, newFakeCall())
	signalAt(h, time.Minute, "add", map[string]any{"name": "a"})
	signalAt(h, 2*time.Minute, "add", map[string]any{"name": "b"})
	signalAt(h, 3*time.Minute, "stop", map[string]any{})
	signalAt(h, 3*time.Minute+time.Second, "add", map[string]any{"name": "c"})
	require.NoError(t, h.execDoc(def, "acme:regather:1", nil))
	var res Result
	require.NoError(t, h.env.GetWorkflowResult(&res))
	require.Equal(t, "completed", res.Status)
	require.Equal(t, []any{
		map[string]any{"name": "a"}, map[string]any{"name": "b"}, map[string]any{"name": "c"},
	}, res.Output["items"], "what the interrupted collect took comes first, in arrival order")
}

// spawnDef spawns worker children from input.names, wait none, rounds times in a loop.
const spawnDef = `{
  "id": "respawn", "version": 1,
  "input": {"type": "object", "properties": {"names": {"type": "array", "items": {"type": "string"}}, "rounds": {"type": "integer"}}, "additionalProperties": false},
  "vars": {"n": {"schema": {"type": "integer"}, "default": 0}},
  "start": "kids",
  "steps": {
    "kids": {"type": "spawn", "definition": "worker", "version": 1, "foreach": "input.names", "key": "item",
             "input": {"name": "item"}, "wait": "none", "next": "count"},
    "count": {"type": "set", "values": {"n": {"expr": "vars.n + 1"}}, "next": "loop"},
    "loop": {"type": "decide", "cases": [{"when": "vars.n < input.rounds", "next": "kids"}], "else": "done"},
    "done": {"type": "end", "status": "completed"}
  }
}`

// childIDs lists the workflow ids of the children that started, in start order.
func childIDs(h *harness) []string {
	var ids []string
	for _, e := range h.sink.events {
		if e.Kind == EventStarted && e.ParentID != "" {
			ids = append(ids, e.WorkflowID)
		}
	}
	return ids
}

func TestSpawnDuplicateKeyFailsBeforeAnyChild(t *testing.T) {
	h := newHarness(t, newFakeCall())
	err := h.execDoc(definitionJSON(t, spawnDef), "acme:respawn:1",
		map[string]any{"names": []any{"a", "b", "a"}, "rounds": 1})
	require.ErrorContains(t, err, `spawn key "a" is not unique`)
	require.Empty(t, childIDs(h), "no child started")
}

func TestSpawnAgainInALoopGetsNewChildIDs(t *testing.T) {
	h := newHarness(t, newFakeCall())
	require.NoError(t, h.execDoc(definitionJSON(t, spawnDef), "acme:respawn:2",
		map[string]any{"names": []any{"a"}, "rounds": 3}))
	require.ElementsMatch(t, []string{
		"acme:respawn:2/kids/a", "acme:respawn:2/kids#2/a", "acme:respawn:2/kids#3/a",
	}, childIDs(h), "the first execution keeps the plain id")
}

func TestDuplicateSignalDeliveryIsDropped(t *testing.T) {
	h := newHarness(t, newFakeCall())
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow("add", map[string]any{"name": "a", signalIDKey: "sender/r1/send/1"})
		h.env.SignalWorkflow("add", map[string]any{"name": "a", signalIDKey: "sender/r1/send/1"}) // a retry
	}, time.Minute)
	res := h.run("inbox", "acme:inbox:30", map[string]any{"box": "x"})
	require.Equal(t, "completed", res.Status, "nothing left to drain: the copy was dropped")
	require.Equal(t, "a", res.Output["name"])
	require.Empty(t, h.sink.alerts, "the id is not part of the payload the schema sees")
}

func TestSeenSignalIDsCarryAcrossContinueAsNew(t *testing.T) {
	h := newHarness(t, newFakeCall())
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow("add", map[string]any{"name": "a", signalIDKey: "s/1"})
		h.env.SignalWorkflow("add", map[string]any{"name": "b", signalIDKey: "s/2"})
	}, time.Minute)
	in, err := Prepare(h.proj, StartRequest{Definition: "inbox", Version: 1, WorkflowID: "acme:inbox:31", Tenant: "acme",
		Input: map[string]any{"box": "x"}})
	require.NoError(t, err)
	h.env.ExecuteWorkflow(WorkflowName, in)
	next := continued(t, h)
	require.Equal(t, []string{"s/1", "s/2"}, next.Seen)
	require.Equal(t, []PendingSignal{{Name: "add", Payload: map[string]any{"name": "b"}}}, next.Pending,
		"carried payloads lose the id")
}

func TestRememberIsBounded(t *testing.T) {
	r := &run{seen: map[string]bool{}}
	for i := range seenLimit + 5 {
		r.remember(time.Duration(i).String())
	}
	require.Len(t, r.seenOrder, seenLimit)
	require.Len(t, r.seen, seenLimit)
	require.False(t, r.seen[time.Duration(4).String()], "the oldest are forgotten")
	require.True(t, r.seen[time.Duration(5).String()])
}

func TestHumanResolutionDuringTaskFlushIsAccepted(t *testing.T) {
	h := newHarness(t, itemFakes(0.5)) // gated: review
	var rejected, accepted *bool
	h.sink.onTask = func(rec TaskRecord) {
		if rec.Event == TaskOpened { // the task is visible; the flush has not returned yet
			rejected, accepted, _ = resolve(h, Resolution{StepID: "review", Outcome: "reject", Output: map[string]any{}})
		}
	}
	res := h.run("item", "acme:item:32", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, "failed", res.Status)
	require.False(t, *rejected)
	require.True(t, *accepted)
	require.Equal(t, []string{TaskOpened, TaskResolved}, h.sink.taskEvents())
}

func TestCompensateConfig(t *testing.T) {
	calls := itemFakes(0.9)
	h := newHarness(t, calls)
	def := *h.proj.Definitions["item@1"]
	def.Steps = maps.Clone(def.Steps)
	place := def.Steps["place"]
	comp := *place.Compensate
	comp.Config = map[string]project.Binding{"reason": {Expr: "'undo ' + steps.place.output.po_id"}}
	place.Compensate = &comp
	def.Steps["place"] = place
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow("cancel", map[string]any{"reason": "customer"})
	}, time.Hour)
	require.NoError(t, h.execDoc(def, "acme:item:33", map[string]any{"order_id": "1", "sku": "A"}))
	cancels := calls.callsTo("vendor.cancel_order")
	require.Len(t, cancels, 1)
	require.Equal(t, map[string]any{"reason": "undo PO-A"}, cancels[0].Config)

	// Without a config binding the compensation still gets {}.
	calls2 := itemFakes(0.9)
	h2 := newHarness(t, calls2)
	h2.env.RegisterDelayedCallback(func() {
		h2.env.SignalWorkflow("cancel", map[string]any{"reason": "customer"})
	}, time.Hour)
	h2.run("item", "acme:item:34", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, map[string]any{}, calls2.callsTo("vendor.cancel_order")[0].Config)
}
