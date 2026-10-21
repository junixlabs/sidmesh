package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

// StartRequest is what a caller supplies to start a workflow (SPEC §5 "Starting").
type StartRequest struct {
	Definition string         `json:"definition"`
	Version    int            `json:"version"`
	WorkflowID string         `json:"workflowId"`
	Tenant     string         `json:"tenant"`
	Input      map[string]any `json:"input"`
	Params     map[string]any `json:"params,omitempty"`
}

// Prepare validates a start request against the project and builds the StartInput that pins
// the definition, every definition it can start and every contract they call.
func Prepare(p *project.Project, req StartRequest) (StartInput, error) {
	key := sidmesh.Key(req.Definition, req.Version)
	def, ok := p.Definitions[key]
	if !ok {
		return StartInput{}, fmt.Errorf("definition %s not found", key)
	}
	input := req.Input
	if input == nil {
		input = map[string]any{}
	}
	if err := validateStart(*def, input, req.Params); err != nil {
		return StartInput{}, err
	}

	pins := map[string]string{}
	if err := pinDefinition(p, def, pins); err != nil {
		return StartInput{}, err
	}
	return StartInput{
		Tenant:     req.Tenant,
		Definition: DefinitionRef{Key: project.DefinitionPin + key},
		Pins:       pins,
		Queues:     p.Manifest.Queues,
		Input:      input,
		Params:     req.Params,
	}, nil
}

func nodeOf(capability string) string {
	node, _, _ := strings.Cut(capability, ".")
	return node
}

// sharedSchemas keeps schemas compiled for no pinned document (start validation, runs
// started before pinning), by name and schema JSON.
var sharedSchemas = newCache[*jsonschema.Schema](1024)

// compileShared is project.Compile through sharedSchemas.
func compileShared(name string, schema map[string]any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	key := name + "\n" + string(raw)
	if s, ok := sharedSchemas.get(key); ok {
		return s, nil
	}
	s, err := project.Compile(name, schema)
	if err != nil {
		return nil, err
	}
	sharedSchemas.put(key, s)
	return s, nil
}

// validateStart checks a start's input and param overrides against the definition.
func validateStart(def project.Definition, input, params map[string]any) error {
	key := sidmesh.Key(def.ID, def.Version)
	s, err := compileShared(key+"/input", def.Input)
	if err != nil {
		return fmt.Errorf("definition %s input schema: %w", key, err)
	}
	if err := project.Validate(s, input); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	for name, v := range params {
		param, ok := def.Params[name]
		if !ok {
			return fmt.Errorf("param %q is not declared by %s", name, key)
		}
		ps, err := compileShared(key+"/params/"+name, param.Schema)
		if err != nil {
			return fmt.Errorf("param %q schema: %w", name, err)
		}
		if err := project.Validate(ps, v); err != nil {
			return fmt.Errorf("param %q: %w", name, err)
		}
	}
	return nil
}

// pinDefinition pins def, every contract it calls (with lookups and compensations) and,
// recursively, every definition it can start (spawn, signal start).
func pinDefinition(p *project.Project, def *project.Definition, pins map[string]string) error {
	key := sidmesh.Key(def.ID, def.Version)
	pk := project.DefinitionPin + key
	if _, seen := pins[pk]; seen {
		return nil
	}
	pins[pk] = p.Documents[pk].Hash
	for _, id := range slices.Sorted(maps.Keys(def.Steps)) {
		step := def.Steps[id]
		switch {
		case step.Type == "call":
			refs := []project.CapabilityRef{{Capability: step.Capability, Version: step.Version}}
			if step.Compensate != nil {
				refs = append(refs, project.CapabilityRef{Capability: step.Compensate.Capability, Version: step.Compensate.Version})
			}
			for _, ref := range refs {
				if err := pinContract(p, ref, pins); err != nil {
					return fmt.Errorf("definition %s: %w", key, err)
				}
			}
		case step.Type == "spawn" || step.Type == "signal" && step.Start != nil:
			childKey := sidmesh.Key(step.Definition, step.Version)
			if step.Type == "signal" {
				childKey = sidmesh.Key(step.Start.Definition, step.Start.Version)
			}
			child, ok := p.Definitions[childKey]
			if !ok {
				return fmt.Errorf("definition %s step %s starts unknown %s", key, id, childKey)
			}
			if err := pinDefinition(p, child, pins); err != nil {
				return err
			}
		}
	}
	return nil
}

func pinContract(p *project.Project, ref project.CapabilityRef, pins map[string]string) error {
	key := sidmesh.Key(ref.Capability, ref.Version)
	pk := project.ContractPin + key
	if _, seen := pins[pk]; seen {
		return nil
	}
	c, ok := p.Contracts[key]
	if !ok {
		return fmt.Errorf("capability %s not found", key)
	}
	pins[pk] = p.Documents[pk].Hash
	npk := project.NodePin + c.Node
	pins[npk] = p.Documents[npk].Hash // its runtime picks the queue
	if c.Lookup != nil {
		return pinContract(p, *c.Lookup, pins)
	}
	return nil
}

// contractInfo is the part of a contract the interpreter needs.
func contractInfo(c *project.Contract) ContractInfo {
	info := ContractInfo{
		Capability: c.Capability,
		Version:    c.Version,
		Runtime:    c.Runtime,
		Kind:       c.Kind,
		Effects:    c.Effects,
		Lookup:     c.Lookup,
		Ledger:     c.Ledger,
		Outputs:    map[string]map[string]any{},
	}
	for name, o := range c.Outcomes {
		info.Outputs[name] = o.Output
	}
	if c.Idempotency != nil {
		info.IdempotencyKey = c.Idempotency.Key
	}
	if c.Defaults != nil {
		info.Timeout, info.Retry = c.Defaults.Timeout, c.Defaults.Retry
	}
	return info
}

// Start prepares the request and starts the workflow with the caller's workflow id. A second
// start with the same id is rejected, so a duplicate source event cannot start a second run.
func Start(ctx context.Context, c client.Client, p *project.Project, req StartRequest) (client.WorkflowRun, error) {
	in, err := Prepare(p, req)
	if err != nil {
		return nil, err
	}
	return c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       req.WorkflowID,
		TaskQueue:                in.Queues.Workflow,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
	}, WorkflowName, in)
}

// Resolution is the argument of the human update.
type Resolution struct {
	StepID  string         `json:"stepId"`
	Outcome string         `json:"outcome"`
	Output  map[string]any `json:"output"`
	Actor   string         `json:"actor"`
}

// ResolveTask resolves a human step through the human update and waits for it to be
// accepted. A rejected resolution (not waiting on that step, undeclared outcome, invalid
// output) comes back as the error and the task stays open.
func ResolveTask(ctx context.Context, c client.Client, workflowID string, r Resolution) error {
	h, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   workflowID,
		UpdateName:   HumanUpdate,
		Args:         []any{r},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return err
	}
	return h.Get(ctx, nil)
}
