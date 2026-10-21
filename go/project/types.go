// Package project loads a SidMesh project from disk (SPEC §1) and checks each document
// against the spec's JSON Schemas. Only fields the runtime uses are decoded; semantic rules
// (SPEC §3) belong to the TS validator.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Manifest is sidmesh.json.
type Manifest struct {
	Name   string `json:"name"`
	Spec   string `json:"spec,omitempty"`
	Queues Queues `json:"queues"`
}

// Queues are the Temporal task queues of a project.
type Queues struct {
	Workflow string `json:"workflow,omitempty"`
	Go       string `json:"go,omitempty"`
	TS       string `json:"ts,omitempty"`
}

// Node is nodes/<id>/node.json.
type Node struct {
	ID      string `json:"id"`
	Runtime string `json:"runtime"`
}

// Contract is nodes/<id>/contracts/<verb>.v<N>.json.
type Contract struct {
	Capability  string             `json:"capability"`
	Version     int                `json:"version"`
	Kind        string             `json:"kind"`
	Effects     string             `json:"effects"`
	Config      map[string]any     `json:"config,omitempty"`
	Input       map[string]any     `json:"input"`
	Outcomes    map[string]Outcome `json:"outcomes"`
	Idempotency *Idempotency       `json:"idempotency,omitempty"`
	Lookup      *CapabilityRef     `json:"lookup,omitempty"`
	Ledger      []string           `json:"ledger,omitempty"`
	Defaults    *Defaults          `json:"defaults,omitempty"`

	// Node and Runtime come from the owning node, not the file.
	Node    string `json:"-"`
	Runtime string `json:"-"`
}

// Outcome is one way a capability can finish.
type Outcome struct {
	Output map[string]any `json:"output"`
}

// Idempotency lists the input paths that identify one logical write.
type Idempotency struct {
	Key []string `json:"key"`
}

// CapabilityRef names a capability version.
type CapabilityRef struct {
	Capability string `json:"capability"`
	Version    int    `json:"version"`
}

// Retry is a retry policy.
type Retry struct {
	MaxAttempts     int     `json:"maxAttempts"`
	InitialInterval string  `json:"initialInterval,omitempty"`
	Backoff         float64 `json:"backoff,omitempty"`
}

// Defaults are a contract's execution defaults.
type Defaults struct {
	Timeout string `json:"timeout,omitempty"`
	Retry   *Retry `json:"retry,omitempty"`
}

// Definition is definitions/<id>.v<N>.json.
type Definition struct {
	ID      string             `json:"id"`
	Version int                `json:"version"`
	Input   map[string]any     `json:"input"`
	Params  map[string]Param   `json:"params,omitempty"`
	Vars    map[string]Var     `json:"vars,omitempty"`
	Signals map[string]Signal  `json:"signals,omitempty"`
	Search  map[string]Binding `json:"search,omitempty"`
	Start   string             `json:"start"`
	Steps   map[string]Step    `json:"steps"`
}

// Param is a tenant-tunable value.
type Param struct {
	Schema  map[string]any `json:"schema"`
	Default any            `json:"default"`
}

// Var is a workflow variable, changed only by set steps.
type Var = Param

// Signal is an external signal the workflow accepts.
type Signal struct {
	Schema map[string]any `json:"schema"`
	Goto   string         `json:"goto,omitempty"`
}

// Step holds every step type's fields; Type says which ones apply. The JSON Schema has
// already rejected fields that do not belong to the type.
type Step struct {
	Type string `json:"type"`

	// call
	Capability string             `json:"capability,omitempty"`
	Version    int                `json:"version,omitempty"` // call and spawn
	Input      map[string]Binding `json:"input,omitempty"`   // call, human, spawn
	Config     map[string]Binding `json:"config,omitempty"`
	Retry      *Retry             `json:"retry,omitempty"`
	Gate       *Gate              `json:"gate,omitempty"`
	Compensate *Compensation      `json:"compensate,omitempty"`
	On         map[string]string  `json:"on,omitempty"` // call, wait, human

	// Timeout is a duration string on call steps and a binding on wait steps, decoded once by
	// UnmarshalJSON (CallTimeout, WaitTimeout).
	Timeout json.RawMessage `json:"timeout,omitempty"`
	SLA     *SLA            `json:"sla,omitempty"`
	timeout stepTimeout

	// decide
	Cases []Case `json:"cases,omitempty"`
	Else  string `json:"else,omitempty"`

	// set
	Values map[string]Binding `json:"values,omitempty"`

	// wait
	Signal   string   `json:"signal,omitempty"`
	Duration *Binding `json:"duration,omitempty"`
	Collect  *Collect `json:"collect,omitempty"`

	// human
	Task     string                    `json:"task,omitempty"`
	Outcomes map[string]map[string]any `json:"outcomes,omitempty"`

	// spawn
	Definition    string             `json:"definition,omitempty"`
	Foreach       *Binding           `json:"foreach,omitempty"`
	Key           *Binding           `json:"key,omitempty"`
	Params        map[string]Binding `json:"params,omitempty"`
	Wait          string             `json:"wait,omitempty"`
	Next          string             `json:"next,omitempty"` // spawn, signal, compensate
	OnChildFailed string             `json:"onChildFailed,omitempty"`

	// signal
	To      json.RawMessage    `json:"to,omitempty"`
	Name    string             `json:"name,omitempty"`
	Payload map[string]Binding `json:"payload,omitempty"`
	Start   *SignalStart       `json:"start,omitempty"`

	// end
	Status string             `json:"status,omitempty"`
	Output map[string]Binding `json:"output,omitempty"`
	Drain  []string           `json:"drain,omitempty"` // continue as new while these have queued payloads
}

// stepTimeout is Step.Timeout decoded for the step's type. A value that does not decode is
// kept as err and reported when the step runs, not when the definition is read.
type stepTimeout struct {
	call string   // call steps
	wait *Binding // wait steps
	err  error
}

// UnmarshalJSON decodes the step and then its timeout.
func (s *Step) UnmarshalJSON(data []byte) error {
	type plain Step // without this method
	if err := json.Unmarshal(data, (*plain)(s)); err != nil {
		return err
	}
	s.timeout = stepTimeout{}
	if len(s.Timeout) == 0 {
		return nil
	}
	switch s.Type {
	case "call":
		if err := json.Unmarshal(s.Timeout, &s.timeout.call); err != nil {
			s.timeout = stepTimeout{err: fmt.Errorf("call timeout: %w", err)}
		}
	case "wait":
		var b Binding
		if err := json.Unmarshal(s.Timeout, &b); err != nil {
			s.timeout.err = fmt.Errorf("wait timeout: %w", err)
		} else {
			s.timeout.wait = &b
		}
	}
	return nil
}

// CallTimeout returns a call step's timeout duration string, or "".
func (s Step) CallTimeout() (string, error) { return s.timeout.call, s.timeout.err }

// WaitTimeout returns a wait step's timeout binding, or nil.
func (s Step) WaitTimeout() (*Binding, error) { return s.timeout.wait, s.timeout.err }

// Target is where a signal step sends: exactly one field is set.
type Target struct {
	Parent   bool
	Workflow *Binding
	Children string // spawn step id
}

// Target decodes a signal step's "to".
func (s Step) Target() (Target, error) {
	var p string
	if json.Unmarshal(s.To, &p) == nil {
		if p != "parent" {
			return Target{}, fmt.Errorf("signal to %q: expected \"parent\"", p)
		}
		return Target{Parent: true}, nil
	}
	var obj struct {
		Workflow *Binding `json:"workflow"`
		Children string   `json:"children"`
	}
	if err := json.Unmarshal(s.To, &obj); err != nil || (obj.Workflow == nil) == (obj.Children == "") {
		return Target{}, errors.New(`signal "to" must be "parent", {"workflow": binding} or {"children": stepId}`)
	}
	return Target{Workflow: obj.Workflow, Children: obj.Children}, nil
}

// Collect makes a signal wait gather several payloads. Max is an integer or a binding.
type Collect struct {
	Window Binding  `json:"window"`
	Max    *Binding `json:"max,omitempty"`
}

// UnmarshalJSON reads max as a binding; a plain integer becomes a constant.
func (c *Collect) UnmarshalJSON(data []byte) error {
	var raw struct {
		Window Binding         `json:"window"`
		Max    json.RawMessage `json:"max"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = Collect{Window: raw.Window}
	if len(raw.Max) == 0 {
		return nil
	}
	var n float64
	if json.Unmarshal(raw.Max, &n) == nil {
		c.Max = &Binding{IsConst: true, Const: n}
		return nil
	}
	c.Max = &Binding{}
	return json.Unmarshal(raw.Max, c.Max)
}

// SignalStart starts the target of a signal step if it is not running.
type SignalStart struct {
	Definition string             `json:"definition"`
	Version    int                `json:"version"`
	Input      map[string]Binding `json:"input"`
	Params     map[string]Binding `json:"params,omitempty"`
	Once       bool               `json:"once,omitempty"` // never start a closed id again
}

// Case is one branch of a decide step.
type Case struct {
	When string `json:"when"`
	Next string `json:"next"`
}

// Gate sends a low-confidence ai result somewhere else.
type Gate struct {
	Threshold Binding `json:"threshold"`
	Below     string  `json:"below"`
}

// Compensation undoes a call step's effect.
type Compensation struct {
	Capability string             `json:"capability"`
	Version    int                `json:"version"`
	Input      map[string]Binding `json:"input"`
	Config     map[string]Binding `json:"config,omitempty"` // resolved like a call's config; none is {}
}

// SLA raises an alert when a step runs too long.
type SLA struct {
	After string `json:"after"`
	Alert string `json:"alert,omitempty"`
}

// Binding is a path ("input.order_id"), a constant ({"const": v}) or a CEL expression
// ({"expr": "..."}).
type Binding struct {
	Path    string
	Const   any
	IsConst bool
	Expr    string
}

// UnmarshalJSON decodes any of the three binding forms.
func (b *Binding) UnmarshalJSON(data []byte) error {
	var path string
	if json.Unmarshal(data, &path) == nil {
		*b = Binding{Path: path}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("binding: %w", err)
	}
	if raw, ok := obj["const"]; ok {
		*b = Binding{IsConst: true}
		return json.Unmarshal(raw, &b.Const)
	}
	if raw, ok := obj["expr"]; ok {
		*b = Binding{}
		return json.Unmarshal(raw, &b.Expr)
	}
	return errors.New(`binding: expected a path, {"const": ...} or {"expr": ...}`)
}

// MarshalJSON writes the binding back in the form it was read in.
func (b Binding) MarshalJSON() ([]byte, error) {
	switch {
	case b.IsConst:
		return json.Marshal(map[string]any{"const": b.Const})
	case b.Expr != "":
		return json.Marshal(map[string]string{"expr": b.Expr})
	default:
		return json.Marshal(b.Path)
	}
}
