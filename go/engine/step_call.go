package engine

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

func (r *run) call(id string, s project.Step) (string, error) {
	key := sidmesh.Key(s.Capability, s.Version)
	info, ok := r.contract(key)
	if !ok {
		return "", fmt.Errorf("capability %s is not pinned", key)
	}
	sc := r.scope(nil)
	input, err := r.exprs.resolveMap(s.Input, sc)
	if err != nil {
		return "", err
	}
	config, err := r.exprs.resolveMap(s.Config, sc)
	if err != nil {
		return "", err
	}

	stop := r.watchSLA(id, s.SLA)
	outcome, output, err := r.invoke(id, info, s, input, config)
	stop()
	if err != nil {
		return "", err
	}
	r.setStep(id, outcome, output)

	succeeded := outcome != OutcomeFailed && outcome != OutcomeUnknown
	next, gated := s.On[outcome], false
	if succeeded && info.Kind == "ai" && s.Gate != nil {
		threshold, err := r.exprs.resolve(s.Gate.Threshold, r.scope(nil))
		if err != nil {
			return "", fmt.Errorf("gate threshold: %w", err)
		}
		t, ok := toFloat(threshold)
		if !ok {
			return "", fmt.Errorf("gate threshold %v is not a number", threshold)
		}
		if c, _ := toFloat(output["confidence"]); c < t {
			next, gated = s.Gate.Below, true
		}
	}
	r.ledger(id, info, input, config, outcome, output, gated)

	if succeeded && s.Compensate != nil {
		// Resolved now, after setStep, so it can read this step's own output.
		sc := r.scope(nil)
		cin, err := r.exprs.resolveMap(s.Compensate.Input, sc)
		if err != nil {
			return "", fmt.Errorf("compensate input: %w", err)
		}
		ccfg, err := r.exprs.resolveMap(s.Compensate.Config, sc)
		if err != nil {
			return "", fmt.Errorf("compensate config: %w", err)
		}
		r.comps = append(r.comps, &compensation{
			stepID: id, capability: s.Compensate.Capability, version: s.Compensate.Version, input: cin, config: ccfg,
		})
	}
	if next == "" {
		return "", fmt.Errorf("outcome %q has no route", outcome)
	}
	return next, nil
}

type failureKind int

const (
	retryable    failureKind = iota
	beforeRun                // the handler never ran: nothing happened
	nonRetryable             // the handler ran (or may have) and must not be retried
)

func classify(err error) failureKind {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case sidmesh.ErrInputInvalid, sidmesh.ErrUnknownCapability:
			return beforeRun
		case sidmesh.ErrOutputInvalid, sidmesh.ErrFatal:
			return nonRetryable
		}
		if appErr.NonRetryable() {
			return nonRetryable
		}
	}
	return retryable
}

type retryPolicy struct {
	maxAttempts int
	initial     time.Duration
	backoff     float64
}

// defaultPolicy retries a call whose contract and step set no retry, and every call the
// interpreter makes itself.
var defaultPolicy = retryPolicy{maxAttempts: 3, initial: time.Second, backoff: 2}

// defaultTimeout bounds a call attempt whose contract and step set no timeout, and every
// call the interpreter makes itself (lookup, compensation, signal-with-start).
const defaultTimeout = 30 * time.Second

// toPolicy fills what rt leaves unset from defaultPolicy; maxAttempts is taken as given.
// A nil rt is defaultPolicy.
func toPolicy(rt *project.Retry) (retryPolicy, error) {
	if rt == nil {
		return defaultPolicy, nil
	}
	p := retryPolicy{maxAttempts: rt.MaxAttempts, initial: defaultPolicy.initial, backoff: cmp.Or(rt.Backoff, defaultPolicy.backoff)}
	if rt.InitialInterval != "" {
		d, err := parseDuration(rt.InitialInterval)
		if err != nil {
			return p, err
		}
		p.initial = d
	}
	return p, nil
}

func (p retryPolicy) delay(attempt int) time.Duration {
	return time.Duration(float64(p.initial) * math.Pow(p.backoff, float64(attempt-1)))
}

// idempotencyKey computes a write's key from the resolved input.
func (r *run) idempotencyKey(info ContractInfo, input map[string]any) (string, error) {
	values := make([]any, len(info.IdempotencyKey))
	for i, path := range info.IdempotencyKey {
		values[i] = walk(map[string]any{"input": input}, path)
	}
	return sidmesh.IdempotencyKey(r.in.Tenant, info.Capability, info.Version, values)
}

// invoke runs a call step's capability with the retry loop of SPEC §5. The loop lives in the
// workflow (each attempt is one activity) so a write can run its lookup between attempts.
func (r *run) invoke(stepID string, info ContractInfo, s project.Step, input, config map[string]any) (string, map[string]any, error) {
	timeout := defaultTimeout
	if t := info.Timeout; t != "" {
		d, err := parseDuration(t)
		if err != nil {
			return "", nil, err
		}
		timeout = d
	}
	if t, err := s.CallTimeout(); err != nil {
		return "", nil, err
	} else if t != "" {
		if timeout, err = parseDuration(t); err != nil {
			return "", nil, err
		}
	}
	policy, err := toPolicy(cmp.Or(s.Retry, info.Retry))
	if err != nil {
		return "", nil, err
	}

	meta := sidmesh.Meta{WorkflowID: r.id, StepID: stepID, Tenant: r.in.Tenant}
	write := info.Effects == "write"
	if write {
		if meta.IdempotencyKey, err = r.idempotencyKey(info, input); err != nil {
			return "", nil, err
		}
	}

	// A cancelled workflow stops here: no lookup, no route, the run ends cancelled.
	cancelled := func() bool { return r.ctx.Err() != nil && r.since(changeCancel) }
	for attempt := 1; ; attempt++ {
		meta.Attempt = attempt
		req := sidmesh.CallRequest{Capability: info.Capability, Version: info.Version, Input: input, Config: config, Meta: meta}
		res, err := r.callActivity(info.Runtime, timeout, 1, req)
		if err == nil {
			return res.Outcome, asMap(res.Output), nil
		}
		if cancelled() {
			return "", nil, r.ctx.Err()
		}
		kind := classify(err)
		workflow.GetLogger(r.ctx).Warn("call attempt failed", "step", stepID, "attempt", attempt, "error", err)
		if write && kind != beforeRun {
			outcome, output, found, lerr := r.lookup(info, req)
			if lerr != nil && cancelled() {
				return "", nil, r.ctx.Err()
			}
			if lerr != nil {
				workflow.GetLogger(r.ctx).Warn("lookup failed", "step", stepID, "error", lerr)
				return OutcomeUnknown, nil, nil
			}
			if found {
				return outcome, output, nil
			}
		}
		if kind != retryable || attempt >= policy.maxAttempts {
			return OutcomeFailed, nil, nil
		}
		if err := workflow.Sleep(r.ctx, policy.delay(attempt)); err != nil {
			return "", nil, err
		}
	}
}

// lookup asks the write's lookup capability, with the write's input and config, what an
// earlier attempt did. found is false when it answers not_found.
func (r *run) lookup(info ContractInfo, write sidmesh.CallRequest) (string, map[string]any, bool, error) {
	if info.Lookup == nil {
		return "", nil, false, fmt.Errorf("%s declares no lookup", sidmesh.Key(info.Capability, info.Version))
	}
	key := sidmesh.Key(info.Lookup.Capability, info.Lookup.Version)
	li, ok := r.contract(key)
	if !ok {
		return "", nil, false, fmt.Errorf("lookup %s is not pinned", key)
	}
	req := write
	req.Capability, req.Version = li.Capability, li.Version
	res, err := r.callActivity(li.Runtime, defaultTimeout, defaultPolicy.maxAttempts, req)
	if err != nil {
		return "", nil, false, err
	}
	if res.Outcome == "not_found" {
		return "", nil, false, nil
	}
	return res.Outcome, asMap(res.Output), true, nil
}

func (r *run) callActivity(runtime string, timeout time.Duration, attempts int, req sidmesh.CallRequest) (sidmesh.Result, error) {
	queue := r.in.Queues.Go
	if runtime == "ts" {
		queue = r.in.Queues.TS
	}
	ctx := workflow.WithActivityOptions(r.ctx, workflow.ActivityOptions{
		TaskQueue:           queue,
		StartToCloseTimeout: timeout,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    int32(attempts),
			InitialInterval:    defaultPolicy.initial,
			BackoffCoefficient: defaultPolicy.backoff,
		},
	})
	var res sidmesh.Result
	err := workflow.ExecuteActivity(ctx, CallActivityName, req).Get(ctx, &res)
	return res, err
}

func (r *run) ledger(stepID string, info ContractInfo, input, config map[string]any, outcome string, output map[string]any, gated bool) {
	rec := LedgerRecord{
		Tenant:     r.in.Tenant,
		WorkflowID: r.id,
		StepID:     stepID,
		Capability: info.Capability,
		Version:    info.Version,
		Outcome:    outcome,
		Gated:      gated,
		Time:       workflow.Now(r.ctx),
	}
	if info.Kind == "ai" {
		if c, ok := toFloat(output["confidence"]); ok {
			rec.Confidence = &c
		}
	}
	if len(info.Ledger) > 0 {
		src := map[string]any{"input": input, "config": config, "output": output}
		rec.Fields = map[string]any{}
		for _, path := range info.Ledger {
			rec.Fields[path] = walk(src, path)
		}
	}
	r.emit(SinkRecord{Ledger: &rec})
}
