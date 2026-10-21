// Package engine runs SidMesh definitions on Temporal: the interpreter workflow, the Go side
// of the sidmesh.call activity, sinks, and helpers to start and steer workflows.
package engine

import (
	"encoding/json"

	"github.com/junixlabs/sidmesh/go/project"
)

// Names registered with Temporal.
const (
	WorkflowName     = "sidmesh.interpret"
	CallActivityName = "sidmesh.call"
	SinkActivityName = "sidmesh.sink"
	// ParamsActivityName reads a tenant's stored params (local activity, once per run).
	ParamsActivityName = "sidmesh.params"
)

// StartInput is the interpreter workflow's input, {tenant, definition, pins, queues, input,
// params?} (SPEC §5 "Version pinning"): the run's documents are pinned by content hash, so a
// running workflow never sees later edits.
type StartInput struct {
	Tenant     string            `json:"tenant"`
	Definition DefinitionRef     `json:"definition"`
	Pins       map[string]string `json:"pins,omitempty"` // pin key -> sha256:<hex>
	Queues     project.Queues    `json:"queues"`

	// Runs started before pinning carry the documents (Definition.Doc and these).
	Definitions map[string]project.Definition `json:"definitions,omitempty"` // by id@version
	Contracts   map[string]ContractInfo       `json:"contracts,omitempty"`   // by capability@version

	Input  map[string]any `json:"input"`
	Params map[string]any `json:"params,omitempty"` // overrides of defaults

	// Set by end drain on the next run: queued drain-signal payloads to deliver before the
	// first step (arrival order), the latest payload of every other signal, and how often
	// each step has run so far (task ids and SLA keys stay unique across runs).
	Pending    []PendingSignal `json:"pending,omitempty"`
	Latest     map[string]any  `json:"latest,omitempty"`
	Executions map[string]int  `json:"executions,omitempty"`
	// Delivery ids of signal payloads already accepted, oldest first, so a payload delivered
	// again after continue-as-new is still dropped.
	Seen []string `json:"seen,omitempty"`
}

// PendingSignal is a signal payload carried across continue-as-new.
type PendingSignal struct {
	Name    string `json:"name"`
	Payload any    `json:"payload"`
}

// ContractInfo is the part of a contract the interpreter needs. Input and config schemas
// stay with the workers, which validate.
type ContractInfo struct {
	Capability     string                 `json:"capability"`
	Version        int                    `json:"version"`
	Runtime        string                 `json:"runtime"`
	Kind           string                 `json:"kind"`
	Effects        string                 `json:"effects"`
	Lookup         *project.CapabilityRef `json:"lookup,omitempty"`
	IdempotencyKey []string               `json:"idempotencyKey,omitempty"`
	Ledger         []string               `json:"ledger,omitempty"`
	Timeout        string                 `json:"timeout,omitempty"`
	Retry          *project.Retry         `json:"retry,omitempty"`
	// Outputs maps each declared outcome to its output schema, which types numbers in CEL.
	Outputs map[string]map[string]any `json:"outputs,omitempty"`
}

// Result is what the workflow returns when it reaches an end step.
type Result struct {
	Status string         `json:"status"`
	Output map[string]any `json:"output,omitempty"`
}

// Outcomes the engine itself produces on call steps.
const (
	OutcomeFailed  = "failed"
	OutcomeUnknown = "unknown"
)

// DefinitionRef is the start input's "definition": the root definition's pin key, or the
// definition document itself in a run started before pinning.
type DefinitionRef struct {
	Key string
	Doc *project.Definition
}

func (d DefinitionRef) MarshalJSON() ([]byte, error) {
	if d.Doc != nil {
		return json.Marshal(d.Doc)
	}
	return json.Marshal(d.Key)
}

func (d *DefinitionRef) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &d.Key)
	}
	d.Doc = &project.Definition{}
	return json.Unmarshal(data, d.Doc)
}
