package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.temporal.io/sdk/temporal"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

// CallActivity is the Go worker's sidmesh.call activity (SPEC §4, §4.1). It enforces the
// contract around every handler: input and config before, outcome and output after.
type CallActivity struct {
	contracts map[string]*compiledContract
	handlers  *sidmesh.Registry
}

type compiledContract struct {
	input    *jsonschema.Schema
	config   *jsonschema.Schema // nil when the contract declares no config
	outcomes map[string]*jsonschema.Schema
}

// NewCallActivity compiles the schemas of every Go contract in the project.
func NewCallActivity(p *project.Project, handlers *sidmesh.Registry) (*CallActivity, error) {
	a := &CallActivity{contracts: map[string]*compiledContract{}, handlers: handlers}
	for key, c := range p.Contracts {
		if c.Runtime != "go" {
			continue
		}
		cc := &compiledContract{outcomes: map[string]*jsonschema.Schema{}}
		var err error
		if cc.input, err = project.Compile(key+"/input", c.Input); err != nil {
			return nil, fmt.Errorf("%s input: %w", key, err)
		}
		if c.Config != nil {
			if cc.config, err = project.Compile(key+"/config", c.Config); err != nil {
				return nil, fmt.Errorf("%s config: %w", key, err)
			}
		}
		for name, o := range c.Outcomes {
			if cc.outcomes[name], err = project.Compile(key+"/outcomes/"+name, o.Output); err != nil {
				return nil, fmt.Errorf("%s outcome %s: %w", key, name, err)
			}
		}
		a.contracts[key] = cc
	}
	return a, nil
}

// Call runs one capability. Input, config and output are plain decoded JSON when validated,
// which project.Validate reads without encoding them again: input is encoded once (by the
// handler's Wrap), output once (by plainJSON).
func (a *CallActivity) Call(ctx context.Context, req sidmesh.CallRequest) (sidmesh.Result, error) {
	key := sidmesh.Key(req.Capability, req.Version)
	cc, known := a.contracts[key]
	h, registered := a.handlers.Lookup(key)
	if !known || !registered {
		return sidmesh.Result{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("no Go handler for %s", key), sidmesh.ErrUnknownCapability, nil)
	}
	input, config := req.Input, req.Config
	if input == nil {
		input = map[string]any{}
	}
	if config == nil {
		config = map[string]any{}
	}
	if err := project.Validate(cc.input, input); err != nil {
		return sidmesh.Result{}, temporal.NewNonRetryableApplicationError("input: "+err.Error(), sidmesh.ErrInputInvalid, nil)
	}
	if cc.config != nil {
		if err := project.Validate(cc.config, config); err != nil {
			return sidmesh.Result{}, temporal.NewNonRetryableApplicationError("config: "+err.Error(), sidmesh.ErrInputInvalid, nil)
		}
	}

	res, err := h(ctx, input, config, req.Meta)
	if err != nil {
		if sidmesh.IsFatal(err) {
			return sidmesh.Result{}, temporal.NewNonRetryableApplicationError(err.Error(), sidmesh.ErrFatal, err)
		}
		return sidmesh.Result{}, temporal.NewApplicationErrorWithCause(err.Error(), "HANDLER_ERROR", err)
	}

	schema, declared := cc.outcomes[res.Outcome]
	if !declared {
		return sidmesh.Result{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("outcome %q is not declared by %s", res.Outcome, key), sidmesh.ErrOutputInvalid, nil)
	}
	output, err := plainJSON(res.Output)
	if err == nil {
		err = project.Validate(schema, output)
	}
	if err != nil {
		return sidmesh.Result{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("outcome %s output: %v", res.Outcome, err), sidmesh.ErrOutputInvalid, nil)
	}
	return sidmesh.Result{Outcome: res.Outcome, Output: output}, nil
}

// plainJSON turns a handler's typed output into maps, slices and float64s.
func plainJSON(v any) (any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(raw, &out)
	return out, err
}
