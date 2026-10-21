// Package sidmesh is what a Go node imports: the wire envelope between the interpreter and
// workers (SPEC §4.1), handler registration, and the error a handler returns to fail a step
// without retry.
package sidmesh

import "fmt"

// Error types carried by the sidmesh.call activity. All four are non-retryable; any other
// error type is a retryable handler error.
const (
	ErrInputInvalid      = "INPUT_INVALID"
	ErrOutputInvalid     = "OUTPUT_INVALID"
	ErrFatal             = "FATAL"
	ErrUnknownCapability = "UNKNOWN_CAPABILITY"
)

// Meta is what the engine tells a handler about the call it is serving.
type Meta struct {
	WorkflowID     string `json:"workflowId"`
	StepID         string `json:"stepId"`
	Attempt        int    `json:"attempt"`
	Tenant         string `json:"tenant"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// Result is how a handler finishes: one of the outcomes its contract declares, with the
// output for that outcome.
type Result struct {
	Outcome string `json:"outcome"`
	Output  any    `json:"output"`
}

// CallRequest is the input of the sidmesh.call activity.
type CallRequest struct {
	Capability string         `json:"capability"`
	Version    int            `json:"version"`
	Input      map[string]any `json:"input"`
	Config     map[string]any `json:"config"`
	Meta       Meta           `json:"meta"`
}

// Key names one capability version, e.g. "vendor.place_order@1".
func Key(capability string, version int) string {
	return fmt.Sprintf("%s@%d", capability, version)
}
