package engine

import (
	"context"
	"errors"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	"go.temporal.io/sdk/client"
)

// SignalWithStartActivityName is the activity behind a signal step with "start". Workflow
// code cannot signal-with-start, so the workflow hands the request to this activity.
const SignalWithStartActivityName = "sidmesh.signal_with_start"

// SignalWithStartRequest signals WorkflowID, starting it with Start if it is not running.
type SignalWithStartRequest struct {
	WorkflowID string         `json:"workflowId"`
	Signal     string         `json:"signal"`
	Payload    map[string]any `json:"payload"`
	Start      StartInput     `json:"start"`
	Once       bool           `json:"once,omitempty"`
}

// Starter runs signal-with-start with a Temporal client.
type Starter struct {
	Client client.Client
}

// SignalWithStart is the activity; the workflow has validated the start. With Once, a
// target id that already ran and closed is skipped silently.
func (s Starter) SignalWithStart(ctx context.Context, req SignalWithStartRequest) error {
	opts := client.StartWorkflowOptions{ID: req.WorkflowID, TaskQueue: req.Start.Queues.Workflow}
	if req.Once {
		opts.WorkflowIDReusePolicy = enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
	}
	_, err := s.Client.SignalWithStartWorkflow(ctx, req.WorkflowID, req.Signal, req.Payload, opts, WorkflowName, req.Start)
	var closed *serviceerror.WorkflowExecutionAlreadyStarted
	if req.Once && errors.As(err, &closed) {
		return nil
	}
	return err
}
