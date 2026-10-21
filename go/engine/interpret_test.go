package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
)

func shipAfter(h *harness, d time.Duration, workflowID string) {
	h.env.RegisterDelayedCallback(func() {
		require.NoError(h.t, h.env.SignalWorkflowByID(workflowID, "shipped", map[string]any{"tracking": "TRK-" + workflowID}))
	}, d)
}

func TestOrderSpawnsItemsAndCompletes(t *testing.T) {
	calls := itemFakes(0.95).on("shop.fetch_order@1", ok("found", map[string]any{
		"items": []any{map[string]any{"sku": "A", "qty": 1}, map[string]any{"sku": "B", "qty": 2}},
	}))
	h := newHarness(t, calls)
	shipAfter(h, time.Hour, "acme:shop:1/items/A")
	shipAfter(h, 2*time.Hour, "acme:shop:1/items/B")

	res := h.run("order", "acme:shop:1", map[string]any{"order_id": "1"})

	require.Equal(t, "completed", res.Status)
	children := res.Output["children"].([]any)
	require.Len(t, children, 2)
	first := children[0].(map[string]any)
	require.Equal(t, "A", first["key"])
	require.Equal(t, "completed", first["status"])
	out := first["output"].(map[string]any)
	require.Equal(t, "PO-A", out["po_id"])
	require.Equal(t, "TRK-acme:shop:1/items/A", out["tracking"])
	require.Equal(t, "A:PO-A", out["label"])

	places := calls.callsTo("vendor.place_order")
	require.Len(t, places, 2)
	for _, p := range places {
		require.Len(t, p.Meta.IdempotencyKey, 64)
		require.Equal(t, 1, p.Meta.Attempt)
		require.Equal(t, "acme", p.Meta.Tenant)
	}
	require.NotEqual(t, places[0].Meta.IdempotencyKey, places[1].Meta.IdempotencyKey)

	require.Len(t, h.sink.ledger, 5) // fetch + 2 suggest + 2 place
	for _, l := range h.sink.ledger {
		if l.Capability == "mapper.suggest" {
			require.False(t, l.Gated)
			require.InDelta(t, 0.95, *l.Confidence, 1e-9)
			require.Contains(t, l.Fields, "input.sku")
			require.Contains(t, l.Fields, "output.option_set")
		}
	}
}

// resolve sends the human update and reports how the workflow answered.
func resolve(h *harness, res Resolution) (rejected, accepted *bool, rejectErr *error) {
	rej, acc := false, false
	var rerr error
	h.env.UpdateWorkflow(HumanUpdate, "", &testsuite.TestUpdateCallback{
		OnReject:   func(err error) { rej, rerr = true, err },
		OnAccept:   func() { acc = true },
		OnComplete: func(any, error) {},
	}, res)
	return &rej, &acc, &rerr
}

func TestGateSendsLowConfidenceToHumanAndRejectsBadResolution(t *testing.T) {
	h := newHarness(t, itemFakes(0.5))
	const id = "acme:item:1"
	var wrongStep, badOutput, good struct{ rejected, accepted *bool }
	var badErr *error
	h.env.RegisterDelayedCallback(func() {
		wrongStep.rejected, wrongStep.accepted, _ = resolve(h, Resolution{StepID: "place", Outcome: "approve", Output: map[string]any{"option_set": "x"}})
		// Missing option_set: rejected synchronously, task stays open.
		badOutput.rejected, badOutput.accepted, badErr = resolve(h, Resolution{StepID: "review", Outcome: "approve", Output: map[string]any{}, Actor: "ann"})
	}, time.Minute)
	h.env.RegisterDelayedCallback(func() {
		good.rejected, good.accepted, _ = resolve(h, Resolution{StepID: "review", Outcome: "approve", Output: map[string]any{"option_set": "OS-fixed"}, Actor: "ann"})
	}, 2*time.Minute)
	shipAfter(h, time.Hour, id)

	res := h.run("item", id, map[string]any{"order_id": "1", "sku": "A"})

	require.Equal(t, "completed", res.Status)
	require.True(t, *wrongStep.rejected)
	require.True(t, *badOutput.rejected)
	require.ErrorContains(t, *badErr, "option_set")
	require.True(t, *good.accepted)
	require.False(t, *good.rejected)
	require.Equal(t, []string{TaskOpened, TaskResolved}, h.sink.taskEvents())
	require.Equal(t, map[string]any{"sku": "A", "suggestion": "OS-A"}, h.sink.tasks[0].Input)
	require.Equal(t, "ann", h.sink.tasks[1].Actor)
	require.Equal(t, id+"#review#1", h.sink.tasks[0].TaskID)
	require.Equal(t, h.sink.tasks[0].TaskID, h.sink.tasks[1].TaskID)
	require.Equal(t, "acme", h.sink.tasks[0].Tenant)
	require.True(t, h.sink.ledger[0].Gated)
	require.Len(t, h.calls.callsTo("vendor.place_order"), 1)
}

func TestHumanUpdateBeforeTaskOpensIsRejected(t *testing.T) {
	h := newHarness(t, itemFakes(0.95)) // confident: never reaches review
	var early struct{ rejected, accepted *bool }
	h.env.RegisterDelayedCallback(func() {
		early.rejected, early.accepted, _ = resolve(h, Resolution{StepID: "review", Outcome: "approve", Output: map[string]any{"option_set": "x"}})
	}, time.Minute)
	shipAfter(h, time.Hour, "acme:item:10")
	res := h.run("item", "acme:item:10", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, "completed", res.Status)
	require.True(t, *early.rejected)
	require.Empty(t, h.sink.tasks)
}

func TestHumanRejectEndsFailed(t *testing.T) {
	h := newHarness(t, itemFakes(0.1))
	h.env.RegisterDelayedCallback(func() {
		resolve(h, Resolution{StepID: "review", Outcome: "reject", Output: map[string]any{}})
	}, time.Minute)
	res := h.run("item", "acme:item:2", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, "failed", res.Status)
	require.Empty(t, h.calls.callsTo("vendor.place_order"))
}

func TestWriteLookupFindsEarlierWrite(t *testing.T) {
	calls := itemFakes(0.9).
		on("vendor.place_order@1", fail(errFlaky)).
		on("vendor.find_order@1", ok("accepted", map[string]any{"po_id": "PO-early"}))
	h := newHarness(t, calls)
	shipAfter(h, time.Hour, "acme:item:3")

	res := h.run("item", "acme:item:3", map[string]any{"order_id": "1", "sku": "A"})

	require.Equal(t, "completed", res.Status)
	require.Equal(t, "PO-early", res.Output["po_id"])
	require.Len(t, calls.callsTo("vendor.place_order"), 1)
	lookups := calls.callsTo("vendor.find_order")
	require.Len(t, lookups, 1)
	require.Equal(t, calls.callsTo("vendor.place_order")[0].Meta.IdempotencyKey, lookups[0].Meta.IdempotencyKey)
}

func TestWriteLookupNotFoundRetriesWithSameKey(t *testing.T) {
	attempt := 0
	calls := itemFakes(0.9).
		on("vendor.place_order@1", func(req sidmesh.CallRequest) (sidmesh.Result, error) {
			attempt++
			if attempt == 1 {
				return sidmesh.Result{}, errFlaky
			}
			return sidmesh.Result{Outcome: "accepted", Output: map[string]any{"po_id": "PO-2"}}, nil
		}).
		on("vendor.find_order@1", ok("not_found", map[string]any{}))
	h := newHarness(t, calls)
	shipAfter(h, time.Hour, "acme:item:4")

	res := h.run("item", "acme:item:4", map[string]any{"order_id": "1", "sku": "A"})

	require.Equal(t, "completed", res.Status)
	places := calls.callsTo("vendor.place_order")
	require.Len(t, places, 2)
	require.Equal(t, 1, places[0].Meta.Attempt)
	require.Equal(t, 2, places[1].Meta.Attempt)
	require.Equal(t, places[0].Meta.IdempotencyKey, places[1].Meta.IdempotencyKey)
}

func TestWriteBecomesUnknownWhenLookupFails(t *testing.T) {
	calls := itemFakes(0.9).
		on("vendor.place_order@1", fail(errFlaky)).
		on("vendor.find_order@1", fail(errFlaky))
	h := newHarness(t, calls)

	res := h.run("item", "acme:item:5", map[string]any{"order_id": "1", "sku": "A"})

	require.Equal(t, "failed", res.Status)
	require.Equal(t, "write outcome unknown", res.Output["why"])
	require.Len(t, calls.callsTo("vendor.place_order"), 1)
	require.Equal(t, "unknown", h.sink.ledger[len(h.sink.ledger)-1].Outcome)
}

func TestFatalWriteIsNotRetried(t *testing.T) {
	calls := itemFakes(0.9).
		on("vendor.place_order@1", fail(fatal("sku not sold"))).
		on("vendor.find_order@1", ok("not_found", map[string]any{}))
	h := newHarness(t, calls)

	res := h.run("item", "acme:item:6", map[string]any{"order_id": "1", "sku": "A"})

	require.Equal(t, "failed", res.Status)
	require.Len(t, calls.callsTo("vendor.place_order"), 1)
	require.Len(t, calls.callsTo("vendor.find_order"), 1, "a fatal error after the handler ran is still ambiguous")
}

func TestInterruptRunsCompensation(t *testing.T) {
	calls := itemFakes(0.9)
	h := newHarness(t, calls)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow("cancel", map[string]any{"reason": "customer"})
	}, time.Hour)

	res := h.run("item", "acme:item:7", map[string]any{"order_id": "1", "sku": "A"})

	require.Equal(t, "cancelled", res.Status)
	require.Equal(t, "customer", res.Output["reason"])
	cancels := calls.callsTo("vendor.cancel_order")
	require.Len(t, cancels, 1)
	require.Equal(t, map[string]any{"po_id": "PO-A"}, cancels[0].Input)
	require.Len(t, cancels[0].Meta.IdempotencyKey, 64)
	require.Equal(t, "place", cancels[0].Meta.StepID)
}

func TestFailedCompensationRaisesAlert(t *testing.T) {
	calls := itemFakes(0.9).on("vendor.cancel_order@1", fail(fatal("already shipped")))
	h := newHarness(t, calls)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow("cancel", map[string]any{"reason": "late"})
	}, time.Hour)

	res := h.run("item", "acme:item:8", map[string]any{"order_id": "1", "sku": "A"})

	require.Equal(t, "cancelled", res.Status)
	require.Len(t, h.sink.alerts, 1)
	require.Equal(t, AlertCompensationFailed, h.sink.alerts[0].Kind)
}

func TestChildFailureRoutesOrder(t *testing.T) {
	calls := itemFakes(0.95).
		on("shop.fetch_order@1", ok("found", map[string]any{
			"items": []any{map[string]any{"sku": "A", "qty": 1}, map[string]any{"sku": "B", "qty": 1}},
		})).
		on("vendor.place_order@1", func(req sidmesh.CallRequest) (sidmesh.Result, error) {
			if req.Input["sku"] == "B" {
				return sidmesh.Result{Outcome: "rejected", Output: map[string]any{"reason": "out of stock"}}, nil
			}
			return sidmesh.Result{Outcome: "accepted", Output: map[string]any{"po_id": "PO-A"}}, nil
		})
	h := newHarness(t, calls)
	shipAfter(h, time.Hour, "acme:shop:2/items/A")

	res := h.run("order", "acme:shop:2", map[string]any{"order_id": "2"})

	require.Equal(t, "failed", res.Status)
	children := res.Output["children"].([]any)
	require.Equal(t, "completed", children[0].(map[string]any)["status"])
	require.Equal(t, "failed", children[1].(map[string]any)["status"])
}

func TestWaitTimeoutAndSLAAlert(t *testing.T) {
	h := newHarness(t, itemFakes(0.9))
	res := h.run("item", "acme:item:9", map[string]any{"order_id": "1", "sku": "A"})
	require.Equal(t, "failed", res.Status) // never shipped: 7d timeout
	require.Len(t, h.sink.alerts, 1)
	require.Equal(t, AlertSLA, h.sink.alerts[0].Kind)
	require.Equal(t, "item.ship_slow", h.sink.alerts[0].Alert)
	require.Equal(t, "ship", h.sink.alerts[0].StepID)
	require.Equal(t, "sla:acme:item:9:ship:1:item.ship_slow", h.sink.alerts[0].DedupeKey)
	require.Equal(t, "acme", h.sink.alerts[0].Tenant)
}

func TestFlowWaitSignalStepAndExpressions(t *testing.T) {
	h := newHarness(t, newFakeCall())
	h.env.OnSignalExternalWorkflow(mock.Anything, "peer-1", "", "ping",
		map[string]any{"n": int64(7), signalIDKey: "acme:flow:1//tell/1"}).Return(nil).Once() // the test env has no first run id
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow("go", map[string]any{"n": 7})
	}, 45*time.Minute)

	res := h.run("flow", "acme:flow:1", map[string]any{"kind": "wait", "peer": "peer-1"})

	require.Equal(t, "completed", res.Status)
	require.EqualValues(t, 7, res.Output["n"])
	require.EqualValues(t, 14, res.Output["doubled"])
	require.Equal(t, true, res.Output["big"])
	require.Len(t, h.sink.alerts, 1, "signal came after the 30m SLA")
	h.env.AssertExpectations(t)
}

func TestFlowSignalBeforeWaitIsQueued(t *testing.T) {
	h := newHarness(t, newFakeCall())
	h.env.OnSignalExternalWorkflow(mock.Anything, "peer-1", "", "ping", mock.Anything).Return(nil)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow("go", map[string]any{"n": 1})
		h.env.SignalWorkflow("go", map[string]any{"n": 2})
	}, 0)
	res := h.run("flow", "acme:flow:2", map[string]any{"kind": "wait", "peer": "peer-1"})
	require.EqualValues(t, 1, res.Output["n"], "a wait consumes the oldest queued signal")
}

func TestFlowWaitTimesOut(t *testing.T) {
	h := newHarness(t, newFakeCall())
	res := h.run("flow", "acme:flow:3", map[string]any{"kind": "wait", "peer": "p"})
	require.Equal(t, "failed", res.Status)
}

func TestFlowTimerAndElse(t *testing.T) {
	h := newHarness(t, newFakeCall())
	start := h.env.Now()
	res := h.run("flow", "acme:flow:4", map[string]any{"kind": "timer"})
	require.Equal(t, "completed", res.Status)
	require.GreaterOrEqual(t, h.env.Now().Sub(start), 10*time.Minute)

	h2 := newHarness(t, newFakeCall())
	res = h2.run("flow", "acme:flow:5", map[string]any{"kind": "other"})
	require.Equal(t, map[string]any{"kind": "other"}, res.Output)
}

func TestCallFailureRoutesFailedAfterRetries(t *testing.T) {
	calls := newFakeCall().on("shop.fetch_order@1", fail(errFlaky))
	h := newHarness(t, calls)
	res := h.run("order", "acme:shop:3", map[string]any{"order_id": "3"})
	require.Equal(t, "failed", res.Status)
	require.Len(t, calls.callsTo("shop.fetch_order"), 3, "engine default retry is 3 attempts")
}
