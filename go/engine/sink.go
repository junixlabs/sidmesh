package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
)

// LedgerRecord is written once per call step (SPEC §6). Fields holds only the paths the
// contract declares in "ledger".
type LedgerRecord struct {
	Tenant     string         `json:"tenant"`
	WorkflowID string         `json:"workflowId"`
	StepID     string         `json:"stepId"`
	Capability string         `json:"capability"`
	Version    int            `json:"version"`
	Outcome    string         `json:"outcome"`
	Confidence *float64       `json:"confidence,omitempty"`
	Gated      bool           `json:"gated"`
	Fields     map[string]any `json:"fields,omitempty"`
	Time       time.Time      `json:"time"`
}

// Task events.
const (
	TaskOpened      = "opened"
	TaskResolved    = "resolved"
	TaskInterrupted = "interrupted" // an interrupt signal cancelled the task
)

// TaskRecord tracks a human task through its life. TaskID is "<workflowId>#<stepId>#<n>",
// n counting the openings of that step (SPEC §6.1).
type TaskRecord struct {
	TaskID     string         `json:"taskId"`
	Tenant     string         `json:"tenant"`
	WorkflowID string         `json:"workflowId"`
	StepID     string         `json:"stepId"`
	Task       string         `json:"task"`
	Event      string         `json:"event"`
	Input      map[string]any `json:"input,omitempty"`
	Outcomes   []string       `json:"outcomes,omitempty"`
	Outcome    string         `json:"outcome,omitempty"`
	Output     map[string]any `json:"output,omitempty"`
	Actor      string         `json:"actor,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Time       time.Time      `json:"time"`
}

// Alert kinds.
const (
	AlertSLA                 = "sla"
	AlertCompensationFailed  = "compensation_failed"
	AlertSignalInvalid       = "signal.payload_invalid"    // Alert holds the signal name
	AlertTargetNotRunning    = "signal.target_not_running" // Alert holds the signal name
	AlertTenantParamsInvalid = "params.tenant_invalid"     // Alert holds the param name
	AlertPinMissing          = "pin.missing"               // Alert holds the pin key
)

// AlertRecord is an SLA breach, a failed compensation, a dropped signal payload or a
// signal whose target is not running. A sink stores one alert per DedupeKey.
type AlertRecord struct {
	Tenant     string    `json:"tenant"`
	WorkflowID string    `json:"workflowId"`
	StepID     string    `json:"stepId"`
	Kind       string    `json:"kind"`
	Alert      string    `json:"alert,omitempty"`
	DedupeKey  string    `json:"dedupeKey"`
	Message    string    `json:"message,omitempty"`
	Time       time.Time `json:"time"`
}

// Event kinds.
const (
	EventStarted   = "started"
	EventStep      = "step"
	EventEnded     = "ended"
	EventContinued = "continued"
)

// EventRecord is one entry of a workflow's timeline: ids, step ids, outcomes and times,
// never payloads.
type EventRecord struct {
	Time       time.Time `json:"time"`
	Tenant     string    `json:"tenant"`
	WorkflowID string    `json:"workflowId"`
	ParentID   string    `json:"parentId,omitempty"`
	Definition string    `json:"definition"`
	Version    int       `json:"version"`
	Kind       string    `json:"kind"`
	StepID     string    `json:"stepId,omitempty"`
	StepType   string    `json:"stepType,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	EndedAt    time.Time `json:"endedAt,omitzero"`
	Status     string    `json:"status,omitempty"`
}

// LedgerSink stores ledger records.
type LedgerSink interface {
	WriteLedger(ctx context.Context, r LedgerRecord) error
}

// TaskSink stores human task events.
type TaskSink interface {
	WriteTask(ctx context.Context, r TaskRecord) error
}

// AlertSink stores alerts.
type AlertSink interface {
	WriteAlert(ctx context.Context, r AlertRecord) error
}

// EventSink stores timeline events.
type EventSink interface {
	WriteEvent(ctx context.Context, r EventRecord) error
}

// ParamsSource returns a tenant's stored params for one definition version (SPEC §5
// "Tenant params"); nil when it has none.
type ParamsSource interface {
	TenantParams(ctx context.Context, tenant, definition string, version int) (map[string]any, error)
}

// ParamsRequest is the input of the params local activity.
type ParamsRequest struct {
	Tenant     string `json:"tenant"`
	Definition string `json:"definition"`
	Version    int    `json:"version"`
}

// Sinks is what the workflow worker writes records to, and the stores it reads tenant
// params and pinned documents from. A nil sink drops its records; a nil Params is an empty
// layer; without a Registry pins resolve from the worker's own project only.
type Sinks struct {
	Ledger   LedgerSink
	Tasks    TaskSink
	Alerts   AlertSink
	Events   EventSink
	Params   ParamsSource
	Registry Registry
}

// ParamsActivity is the sidmesh.params local activity.
func (s Sinks) ParamsActivity(ctx context.Context, req ParamsRequest) (map[string]any, error) {
	if s.Params == nil {
		return nil, nil
	}
	return s.Params.TenantParams(ctx, req.Tenant, req.Definition, req.Version)
}

// SinkRecord carries exactly one record from the workflow to the sink activity.
type SinkRecord struct {
	Ledger *LedgerRecord `json:"ledger,omitempty"`
	Task   *TaskRecord   `json:"task,omitempty"`
	Alert  *AlertRecord  `json:"alert,omitempty"`
	Event  *EventRecord  `json:"event,omitempty"`
}

// Activity is the sidmesh.sink local activity: it hands each record of a batch to the
// matching sink.
func (s Sinks) Activity(ctx context.Context, recs []SinkRecord) error {
	for i, r := range recs {
		if err := s.write(context.WithValue(ctx, recordIndex{}, i), r); err != nil {
			return err
		}
	}
	return nil
}

type recordIndex struct{}

// RecordKey identifies a record written by the sink activity, the same across its retries,
// so a sink can make writes idempotent. It is "" outside the sink activity.
func RecordKey(ctx context.Context) string {
	i, ok := ctx.Value(recordIndex{}).(int)
	if !ok || !activity.IsActivity(ctx) {
		return ""
	}
	info := activity.GetInfo(ctx)
	return fmt.Sprintf("%s/%s/%s/%d", info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, i)
}

func (s Sinks) write(ctx context.Context, r SinkRecord) error {
	switch {
	case r.Ledger != nil:
		if s.Ledger != nil {
			return s.Ledger.WriteLedger(ctx, *r.Ledger)
		}
	case r.Task != nil:
		if s.Tasks != nil {
			return s.Tasks.WriteTask(ctx, *r.Task)
		}
	case r.Alert != nil:
		if s.Alerts != nil {
			return s.Alerts.WriteAlert(ctx, *r.Alert)
		}
	case r.Event != nil:
		if s.Events != nil {
			return s.Events.WriteEvent(ctx, *r.Event)
		}
	default:
		return errors.New("empty sink record")
	}
	return nil
}

// JSONL is the default sink: one JSON object per line, tagged with its kind.
type JSONL struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONL writes every record to w.
func NewJSONL(w io.Writer) *JSONL { return &JSONL{w: w} }

// Sinks returns a Sinks that sends every kind to this writer.
func (j *JSONL) Sinks() Sinks { return Sinks{Ledger: j, Tasks: j, Alerts: j, Events: j} }

func (j *JSONL) WriteLedger(_ context.Context, r LedgerRecord) error { return j.write("ledger", r) }
func (j *JSONL) WriteTask(_ context.Context, r TaskRecord) error     { return j.write("task", r) }
func (j *JSONL) WriteAlert(_ context.Context, r AlertRecord) error   { return j.write("alert", r) }
func (j *JSONL) WriteEvent(_ context.Context, r EventRecord) error   { return j.write("event", r) }

func (j *JSONL) write(kind string, record any) error {
	raw, err := json.Marshal(struct {
		Kind   string `json:"kind"`
		Record any    `json:"record"`
	}{kind, record})
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_, err = j.w.Write(append(raw, '\n'))
	return err
}
