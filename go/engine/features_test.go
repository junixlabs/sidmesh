package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/temporal"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

func signalAt(h *harness, d time.Duration, name string, payload any) {
	h.env.RegisterDelayedCallback(func() { h.env.SignalWorkflow(name, payload) }, d)
}

func TestBatchVarsCollectSpawnParamsAndChildren(t *testing.T) {
	h := newHarness(t, newFakeCall())
	const id = "acme:batch:1"
	start := h.env.Now()
	signalAt(h, time.Minute, "add", map[string]any{"nope": 1}) // invalid: dropped with an alert
	signalAt(h, time.Minute, "add", map[string]any{"name": "a"})
	signalAt(h, time.Minute, "add", map[string]any{"name": "b"})
	signalAt(h, 30*time.Minute, "add", map[string]any{"name": "c"}) // third: max reached
	signalAt(h, 40*time.Minute, "add", map[string]any{"name": "late"})

	res := h.runReq(StartRequest{Definition: "batch", Version: 1, WorkflowID: id, Tenant: "acme",
		Input: map[string]any{}, Params: map[string]any{"mode": "fast"}})

	require.Equal(t, "completed", res.Status)
	require.EqualValues(t, 4, res.Output["count"], "init set 1, tally added 3")
	require.Equal(t, formatTime(start), res.Output["started"], "workflow.now at the set step")
	require.Equal(t, formatTime(start), res.Output["init_ended"])
	require.Equal(t, formatTime(start), res.Output["gather_started"])
	require.Equal(t, "1800s", res.Output["waited"], "collect ended when max was reached at 30m")
	require.Equal(t, []any{
		map[string]any{"name": "a"}, map[string]any{"name": "b"}, map[string]any{"name": "c"},
	}, res.Output["items"])
	require.EqualValues(t, 3, res.Output["spawned"])

	acks := res.Output["acks"].([]any)
	require.Len(t, acks, 3)
	names := map[string]bool{}
	for _, a := range acks {
		ack := a.(map[string]any)
		names[ack["name"].(string)] = true
		require.Equal(t, "default", ack["mode"], "the parent's own override is not inherited")
		require.Equal(t, "batch", ack["label"], "spawn params reach the child")
		require.Equal(t, id, ack["from"], "the to.children signal reached the child")
		require.Equal(t, "acme", ack["tenant"])
	}
	require.Equal(t, map[string]bool{"a": true, "b": true, "c": true}, names)
	require.Equal(t, []string{AlertSignalInvalid}, h.sink.alertKinds())
	require.Equal(t, "add", h.sink.alerts[0].Alert)
}

func TestCollectWindowFromParamEnds(t *testing.T) {
	h := newHarness(t, newFakeCall())
	signalAt(h, time.Minute, "add", map[string]any{"name": "solo"})
	res := h.runReq(StartRequest{Definition: "batch", Version: 1, WorkflowID: "acme:batch:2", Tenant: "acme",
		Input: map[string]any{}, Params: map[string]any{"window": "2h"}})
	require.Equal(t, "completed", res.Status)
	require.Equal(t, []any{map[string]any{"name": "solo"}}, res.Output["items"])
	require.Equal(t, "7260s", res.Output["waited"], "first payload at 1m, then a 2h window")
	require.Len(t, res.Output["acks"], 1)
}

func TestSetInvalidValueFailsWithVarInvalid(t *testing.T) {
	h := newHarness(t, newFakeCall())
	err := h.exec(StartRequest{Definition: "flow", Version: 1, WorkflowID: "acme:flow:6", Tenant: "acme",
		Input: map[string]any{"kind": "badvar"}})
	requireAppError(t, err, ErrVarInvalid, true)
}

func TestSignalToParentWithoutParentFails(t *testing.T) {
	h := newHarness(t, newFakeCall())
	err := h.exec(StartRequest{Definition: "flow", Version: 1, WorkflowID: "acme:flow:7", Tenant: "acme",
		Input: map[string]any{"kind": "orphan"}})
	requireAppError(t, err, ErrNoParent, true)
}

func TestSignalWithStart(t *testing.T) {
	h := newHarness(t, newFakeCall())
	res := h.run("flow", "acme:flow:8", map[string]any{"kind": "start", "peer": "acme:worker:9"})
	require.Equal(t, "completed", res.Status)
	require.Len(t, h.starts.reqs, 1)
	req := h.starts.reqs[0]
	require.Equal(t, "acme:worker:9", req.WorkflowID)
	require.Equal(t, "poke", req.Signal)
	require.Equal(t, map[string]any{"from": "acme:flow:8", signalIDKey: "acme:flow:8//start_peer/1"}, req.Payload,
		"a delivery id per send, the same on every retry of the activity")
	require.Equal(t, "definition:worker@1", req.Start.Definition.Key)
	require.Equal(t, map[string]any{"name": "acme:worker:9"}, req.Start.Input)
	require.Equal(t, map[string]any{"mode": "started"}, req.Start.Params)
	require.Equal(t, "acme", req.Start.Tenant)
	require.Contains(t, req.Start.Pins, "definition:worker@1", "Prepare pins definitions a signal step can start")
	require.Nil(t, req.Start.Definition.Doc, "a pinned run starts others with pins, not documents")
}

func TestSignalToStoppedWorkflowAlertsAndContinues(t *testing.T) {
	h := newHarness(t, newFakeCall())
	h.env.OnSignalExternalWorkflow(mock.Anything, "gone-1", "", "ping", mock.Anything).
		Return(&temporal.UnknownExternalWorkflowExecutionError{})
	start := h.env.Now()
	res := h.run("flow", "acme:flow:9", map[string]any{"kind": "gone", "peer": "gone-1"})
	require.Equal(t, "completed", res.Status)
	require.Equal(t, "acme", res.Output["tenant"])
	require.Equal(t, formatTime(start), res.Output["now"])
	require.Equal(t, []string{AlertTargetNotRunning}, h.sink.alertKinds())
	require.Equal(t, "to_gone", h.sink.alerts[0].StepID)
}

func TestInterruptWhileSpawnWaitsForAll(t *testing.T) {
	h := newHarness(t, newFakeCall())
	signalAt(h, time.Hour, "abort", map[string]any{})
	res := h.run("fanout", "acme:fanout:1", map[string]any{"names": []any{"a", "b"}})
	require.Equal(t, "cancelled", res.Status)
	acks := res.Output["acks"].([]any)
	require.Len(t, acks, 2, "children kept running and the goto path signalled them")
	for _, a := range acks {
		require.Equal(t, "stop", a.(map[string]any)["from"])
	}
}

func TestSearchAttributesWriteUnresolvedAsEmpty(t *testing.T) {
	calls := itemFakes(0.95).on("shop.fetch_order@1", ok("missing", map[string]any{}))
	h := newHarness(t, calls)
	var stages []string
	h.env.OnUpsertTypedSearchAttributes(mock.Anything).Run(func(args mock.Arguments) {
		sa := args.Get(0).(temporal.SearchAttributes)
		if v, ok := sa.GetKeyword(temporal.NewSearchAttributeKeyKeyword("OrderStage")); ok {
			stages = append(stages, v)
		}
	}).Return(nil)
	res := h.run("order", "acme:shop:9", map[string]any{"order_id": "9"})
	require.Equal(t, "failed", res.Status)
	require.Equal(t, []string{"", "missing"}, stages)
}

func TestWriteInputInvalidSkipsLookup(t *testing.T) {
	calls := itemFakes(0.9).
		on("vendor.place_order@1", fail(temporal.NewNonRetryableApplicationError("bad sku", sidmesh.ErrInputInvalid, nil))).
		on("vendor.find_order@1", ok("accepted", map[string]any{"po_id": "PO-x"}))
	h := newHarness(t, calls)
	res := h.run("item", "acme:item:11", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, "failed", res.Status)
	require.Len(t, calls.callsTo("vendor.place_order"), 1)
	require.Empty(t, calls.callsTo("vendor.find_order"), "the handler never ran")
}

func TestWriteOutputInvalidLooksUpWithoutRetry(t *testing.T) {
	calls := itemFakes(0.9).
		on("vendor.place_order@1", fail(temporal.NewNonRetryableApplicationError("bad output", sidmesh.ErrOutputInvalid, nil))).
		on("vendor.find_order@1", ok("not_found", map[string]any{}))
	h := newHarness(t, calls)
	res := h.run("item", "acme:item:12", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, "failed", res.Status)
	require.Len(t, calls.callsTo("vendor.place_order"), 1, "OUTPUT_INVALID is not retried")
	lookups := calls.callsTo("vendor.find_order")
	require.Len(t, lookups, 1)
	place := calls.callsTo("vendor.place_order")[0]
	require.Equal(t, place.Input, lookups[0].Input)
	require.Equal(t, place.Config, lookups[0].Config, "the lookup gets the write's config")
}

func TestNumbersAreTypedBySchema(t *testing.T) {
	// total is an integer var: const 5, then vars.total + 1 (int + int). score is a number,
	// limit a number param whose default 1 is integer-valued; vars.total / 2 is int division.
	h := newHarness(t, newFakeCall())
	res := h.run("flow", "acme:flow:10", map[string]any{"kind": "numbers", "score": 0.5})
	require.Equal(t, "completed", res.Status)
	require.Equal(t, "low", res.Output["band"])
	require.EqualValues(t, 6, res.Output["total"])
	require.EqualValues(t, 3, res.Output["half"])

	h2 := newHarness(t, newFakeCall())
	res = h2.run("flow", "acme:flow:11", map[string]any{"kind": "numbers", "score": 1.0})
	require.Equal(t, "high", res.Output["band"], "1.0 < 1.0 is false")
}

func TestTyped(t *testing.T) {
	integer := map[string]any{"type": "integer"}
	number := map[string]any{"type": "number"}
	require.Equal(t, int64(3), typed(float64(3), integer))
	require.Equal(t, 3.5, typed(3.5, integer), "a fractional value stays double; validation decides")
	require.Equal(t, 1.0, typed(1.0, number))
	require.Equal(t, int64(2), typed(2.0, nil), "schema-less integral numbers are int")
	require.Equal(t, 2.5, typed(2.5, nil))
	obj := map[string]any{"type": "object", "properties": map[string]any{
		"n": integer, "list": map[string]any{"type": "array", "items": number},
	}}
	require.Equal(t, map[string]any{"n": int64(4), "list": []any{1.0, 2.5}, "free": int64(7)},
		typed(map[string]any{"n": 4.0, "list": []any{1.0, 2.5}, "free": 7.0}, obj))
	require.Equal(t, int64(3), typed(3.0, map[string]any{"type": []any{"integer", "null"}}))
}

func TestSignalWithStartOncePassesThrough(t *testing.T) {
	h := newHarness(t, newFakeCall())
	h.run("flow", "acme:flow:12", map[string]any{"kind": "start_once", "peer": "acme:worker:12"})
	require.Len(t, h.starts.reqs, 1)
	require.True(t, h.starts.reqs[0].Once)

	h2 := newHarness(t, newFakeCall())
	h2.run("flow", "acme:flow:13", map[string]any{"kind": "start", "peer": "acme:worker:13"})
	require.False(t, h2.starts.reqs[0].Once)
}

func TestStarterOnceSkipsClosedTarget(t *testing.T) {
	p, err := project.Load("../testdata/project")
	require.NoError(t, err)
	in, err := Prepare(p, StartRequest{Definition: "worker", Version: 1, WorkflowID: "w", Tenant: "acme", Input: map[string]any{"name": "w"}})
	require.NoError(t, err)
	closed := serviceerror.NewWorkflowExecutionAlreadyStarted("closed", "", "")
	for _, once := range []bool{true, false} {
		c := &mocks.Client{}
		c.On("SignalWithStartWorkflow", mock.Anything, "w", "poke", mock.Anything,
			mock.MatchedBy(func(o client.StartWorkflowOptions) bool {
				return (o.WorkflowIDReusePolicy == enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE) == once
			}), WorkflowName, mock.Anything).Return(nil, closed)
		err := Starter{Client: c}.SignalWithStart(context.Background(),
			SignalWithStartRequest{WorkflowID: "w", Signal: "poke", Start: in, Once: once})
		if once {
			require.NoError(t, err, "once: a closed target is skipped without failing")
		} else {
			require.Error(t, err)
		}
		c.AssertExpectations(t)
	}
}

func TestTenantParamsLayer(t *testing.T) {
	// item's threshold defaults to 0.8; confidence 0.95 passes the gate.
	h := newHarness(t, itemFakes(0.95))
	h.params.byDef["item"] = map[string]any{"threshold": 0.99} // tenant raises it: gated
	h.env.RegisterDelayedCallback(func() {
		resolve(h, Resolution{StepID: "review", Outcome: "reject", Output: map[string]any{}})
	}, time.Minute)
	res := h.run("item", "acme:item:20", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, "failed", res.Status, "the tenant value gated the call")
	require.Equal(t, []ParamsRequest{{Tenant: "acme", Definition: "item", Version: 1}}, h.params.reads)

	h2 := newHarness(t, itemFakes(0.95))
	h2.params.byDef["item"] = map[string]any{"threshold": 0.99}
	shipAfter(h2, time.Hour, "acme:item:21")
	res = h2.runReq(StartRequest{Definition: "item", Version: 1, WorkflowID: "acme:item:21", Tenant: "acme",
		Input: map[string]any{"order_id": "1", "sku": "A"}, Params: map[string]any{"threshold": 0.5}})
	require.Equal(t, "completed", res.Status, "a start override beats the tenant value")

	h3 := newHarness(t, itemFakes(0.95))
	h3.params.byDef["item"] = map[string]any{"threshold": "high", "bogus": 1}
	shipAfter(h3, time.Hour, "acme:item:22")
	res = h3.run("item", "acme:item:22", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, "completed", res.Status, "invalid tenant values are ignored: the default applies")
	require.Equal(t, []string{AlertTenantParamsInvalid, AlertTenantParamsInvalid}, h3.sink.alertKinds())
	require.Equal(t, "bogus", h3.sink.alerts[0].Alert)
	require.Equal(t, "threshold", h3.sink.alerts[1].Alert)
}
