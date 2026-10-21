package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"
)

// continued returns the input of the next run when the workflow continued as new.
func continued(t *testing.T, h *harness) StartInput {
	t.Helper()
	require.True(t, h.env.IsWorkflowCompleted())
	var can *workflow.ContinueAsNewError
	require.True(t, errors.As(h.env.GetWorkflowError(), &can), "want continue-as-new, got %v", h.env.GetWorkflowError())
	require.Equal(t, WorkflowName, can.WorkflowType.Name)
	var next StartInput
	require.NoError(t, converter.GetDefaultDataConverter().FromPayloads(can.Input, &next))
	return next
}

func TestDrainContinuesAsNewCarryingQueuedSignals(t *testing.T) {
	h := newHarness(t, newFakeCall())
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow("add", map[string]any{"name": "a"})
		h.env.SignalWorkflow("note", map[string]any{"text": "n1"})
		h.env.SignalWorkflow("add", map[string]any{"name": "b"})
		h.env.SignalWorkflow("add", map[string]any{"name": "c"})
	}, time.Minute)
	in, err := Prepare(h.proj, StartRequest{Definition: "inbox", Version: 1, WorkflowID: "acme:inbox:1", Tenant: "acme",
		Input: map[string]any{"box": "x"}})
	require.NoError(t, err)
	h.env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "acme:inbox:1"})
	h.env.ExecuteWorkflow(WorkflowName, in)

	next := continued(t, h)
	require.Equal(t, in.Input, next.Input)
	require.Equal(t, in.Definition, next.Definition)
	require.Equal(t, in.Pins, next.Pins, "continue-as-new keeps the pins")
	require.Equal(t, []PendingSignal{
		{Name: "add", Payload: map[string]any{"name": "b"}},
		{Name: "add", Payload: map[string]any{"name": "c"}},
	}, next.Pending, "the drain signal's queue, in arrival order")
	require.Equal(t, map[string]any{"note": map[string]any{"text": "n1"}}, next.Latest, "other signals: latest payload only")
	require.Equal(t, map[string]int{"take": 1, "count": 1, "done": 1}, next.Executions)
	kinds := []string{}
	for _, e := range h.sink.events {
		kinds = append(kinds, e.Kind)
	}
	require.Equal(t, []string{EventStarted, EventStep, EventStep, EventContinued}, kinds)

	// The next run gets b before its first step, vars back at their defaults.
	h2 := newHarness(t, newFakeCall())
	h2.env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "acme:inbox:1"})
	h2.env.ExecuteWorkflow(WorkflowName, next)
	third := continued(t, h2)
	require.Equal(t, []PendingSignal{{Name: "add", Payload: map[string]any{"name": "c"}}}, third.Pending)
	require.Equal(t, map[string]any{"note": map[string]any{"text": "n1"}}, third.Latest, "latest carried again, not queued")
	require.Equal(t, 2, third.Executions["take"])

	h3 := newHarness(t, newFakeCall())
	h3.env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "acme:inbox:1"})
	h3.env.ExecuteWorkflow(WorkflowName, third)
	require.True(t, h3.env.IsWorkflowCompleted())
	require.NoError(t, h3.env.GetWorkflowError())
	var res Result
	require.NoError(t, h3.env.GetWorkflowResult(&res))
	require.Equal(t, "c", res.Output["name"])
	require.EqualValues(t, 1, res.Output["count"], "vars reset on every run")
	require.Equal(t, "n1", res.Output["note"], "signals.<name> keeps its value across runs")
}

func TestDrainEndsWhenNothingIsQueued(t *testing.T) {
	h := newHarness(t, newFakeCall())
	signalAt(h, time.Minute, "add", map[string]any{"name": "only"})
	res := h.run("inbox", "acme:inbox:2", map[string]any{"box": "x"})
	require.Equal(t, "only", res.Output["name"])
	last := h.sink.events[len(h.sink.events)-1]
	require.Equal(t, EventEnded, last.Kind)
	require.Equal(t, "completed", last.Status)
	require.Equal(t, "done", last.StepID)
}

func TestEventsTimeline(t *testing.T) {
	h := newHarness(t, newFakeCall())
	start := h.env.Now()
	h.run("flow", "acme:flow:20", map[string]any{"kind": "timer"})
	var got []string
	for _, e := range h.sink.events {
		require.Equal(t, "acme", e.Tenant)
		require.Equal(t, "acme:flow:20", e.WorkflowID)
		require.Equal(t, "flow", e.Definition)
		got = append(got, e.Kind+":"+e.StepID+":"+e.StepType+":"+e.Outcome+":"+e.Status)
	}
	require.Equal(t, []string{
		"started::::", "step:route:decide::", "step:nap:wait:elapsed:", "ended:woke:end::completed",
	}, got)
	nap := h.sink.events[2]
	require.True(t, nap.StartedAt.Equal(start))
	require.Equal(t, 10*time.Minute, nap.EndedAt.Sub(nap.StartedAt))
}

func TestCollectMaxBindingBelowTwoFails(t *testing.T) {
	h := newHarness(t, newFakeCall())
	signalAt(h, time.Minute, "add", map[string]any{"name": "a"})
	err := h.exec(StartRequest{Definition: "batch", Version: 1, WorkflowID: "acme:batch:4", Tenant: "acme",
		Input: map[string]any{}, Params: map[string]any{"acks_max": 1}})
	require.ErrorContains(t, err, "at least 2")
}

func TestCollectMaxFromBinding(t *testing.T) {
	h := newHarness(t, newFakeCall())
	signalAt(h, time.Minute, "add", map[string]any{"name": "a"})
	signalAt(h, time.Minute, "add", map[string]any{"name": "b"})
	signalAt(h, time.Minute, "add", map[string]any{"name": "c"})
	res := h.runReq(StartRequest{Definition: "batch", Version: 1, WorkflowID: "acme:batch:3", Tenant: "acme",
		Input: map[string]any{}, Params: map[string]any{"acks_max": 2}})
	require.Len(t, res.Output["acks"], 2, "acks collect stops at params.acks_max")
}

func TestStartedEventIsWrittenBeforeTheFirstStepEnds(t *testing.T) {
	h := newHarness(t, newFakeCall())
	var during []string
	h.env.RegisterDelayedCallback(func() { // inside the 10m nap
		h.sink.mu.Lock()
		defer h.sink.mu.Unlock()
		for _, e := range h.sink.events {
			during = append(during, e.Kind)
		}
	}, time.Minute)
	h.run("flow", "acme:flow:30", map[string]any{"kind": "timer"})
	require.Equal(t, []string{EventStarted, EventStep}, during, "started, then the decide step; nap still running")
}
