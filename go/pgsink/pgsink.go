// Package pgsink is the reference Postgres sink of SPEC §6.1.
package pgsink

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/sidcorp-it/sidmesh/go/engine"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

//go:embed schema.sql
var schema string

// Sink writes every record kind to Postgres.
type Sink struct {
	pool *pgxpool.Pool
}

// Open connects to url and runs the migrations.
func Open(ctx context.Context, url string) (*Sink, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	s, err := New(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// New runs the migrations on pool and returns the sink.
func New(ctx context.Context, pool *pgxpool.Pool) (*Sink, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("pgsink migrations: %w", err)
	}
	return &Sink{pool: pool}, nil
}

// Close closes the pool.
func (s *Sink) Close() { s.pool.Close() }

// Sinks returns a Sinks that writes every kind here.
func (s *Sink) Sinks() engine.Sinks {
	return engine.Sinks{Ledger: s, Tasks: s, Alerts: s, Events: s, Params: s, Registry: s}
}

func recordKey(ctx context.Context) *string { return null(engine.RecordKey(ctx)) }

func null(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func jsonb(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func (s *Sink) WriteEvent(ctx context.Context, r engine.EventRecord) error {
	_, err := s.pool.Exec(ctx, `insert into sidmesh_events
		(time, tenant, workflow_id, parent_id, definition, version, kind, step_id, step_type, outcome, started_at, ended_at, status, record_key)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		on conflict (record_key) do nothing`,
		r.Time, r.Tenant, r.WorkflowID, null(r.ParentID), r.Definition, r.Version, r.Kind,
		null(r.StepID), null(r.StepType), null(r.Outcome), nullTime(r.StartedAt), nullTime(r.EndedAt),
		null(r.Status), recordKey(ctx))
	return err
}

func (s *Sink) WriteLedger(ctx context.Context, r engine.LedgerRecord) error {
	fields := r.Fields
	if fields == nil {
		fields = map[string]any{}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `insert into sidmesh_ledger
		(time, tenant, workflow_id, step_id, capability, version, outcome, confidence, gated, fields, record_key)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		on conflict (record_key) do nothing`,
		r.Time, r.Tenant, r.WorkflowID, r.StepID, r.Capability, r.Version, r.Outcome, r.Confidence,
		r.Gated, raw, recordKey(ctx))
	return err
}

// WriteTask inserts the row when the task opens and updates it when it is resolved or
// cancelled by an interrupt.
func (s *Sink) WriteTask(ctx context.Context, r engine.TaskRecord) error {
	switch r.Event {
	case engine.TaskOpened:
		input, err := jsonb(r.Input)
		if err != nil {
			return err
		}
		outcomes, err := jsonb(r.Outcomes)
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `insert into sidmesh_tasks
			(task_id, tenant, workflow_id, step_id, task, status, input, outcomes, opened_at)
			values ($1,$2,$3,$4,$5,'open',$6,$7,$8)
			on conflict (task_id) do nothing`,
			r.TaskID, r.Tenant, r.WorkflowID, r.StepID, r.Task, input, outcomes, r.Time)
		return err
	case engine.TaskResolved:
		output, err := jsonb(r.Output)
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `update sidmesh_tasks
			set status = 'resolved', resolved_at = $2, outcome = $3, output = $4, actor = $5
			where task_id = $1`,
			r.TaskID, r.Time, r.Outcome, output, null(r.Actor))
		return err
	case engine.TaskInterrupted:
		_, err := s.pool.Exec(ctx, `update sidmesh_tasks set status = 'cancelled', resolved_at = $2
			where task_id = $1 and status = 'open'`, r.TaskID, r.Time)
		return err
	}
	return fmt.Errorf("unknown task event %q", r.Event)
}

// WriteAlert stores an alert once per dedupe key.
func (s *Sink) WriteAlert(ctx context.Context, r engine.AlertRecord) error {
	_, err := s.pool.Exec(ctx, `insert into sidmesh_alerts
		(time, tenant, workflow_id, step_id, kind, alert, dedupe_key, message)
		values ($1,$2,$3,$4,$5,$6,$7,$8)
		on conflict (dedupe_key) do nothing`,
		r.Time, r.Tenant, r.WorkflowID, null(r.StepID), r.Kind, null(r.Alert), r.DedupeKey, r.Message)
	return err
}

// TenantParams reads sidmesh_params; no row is an empty layer.
func (s *Sink) TenantParams(ctx context.Context, tenant, definition string, version int) (map[string]any, error) {
	var params map[string]any
	err := s.pool.QueryRow(ctx, `select params from sidmesh_params
		where tenant = $1 and definition = $2 and version = $3`, tenant, definition, version).Scan(&params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return params, err
}

// Publish inserts documents into sidmesh_documents in one statement; an existing hash is
// left alone.
func (s *Sink) Publish(ctx context.Context, docs []project.Document) error {
	if len(docs) == 0 {
		return nil
	}
	hashes, kinds, names := make([]string, len(docs)), make([]string, len(docs)), make([]string, len(docs))
	versions, bodies := make([]int, len(docs)), make([]string, len(docs))
	for i, d := range docs {
		hashes[i], kinds[i], names[i], versions[i], bodies[i] = d.Hash, d.Kind, d.Name, d.Version, string(d.Doc)
	}
	_, err := s.pool.Exec(ctx, `insert into sidmesh_documents (hash, kind, name, version, doc)
		select hash, kind, name, version, doc::jsonb
		from unnest($1::text[], $2::text[], $3::text[], $4::int[], $5::text[]) as d (hash, kind, name, version, doc)
		on conflict (hash) do nothing`, hashes, kinds, names, versions, bodies)
	return err
}

// Fetch returns the documents the registry has among hashes.
func (s *Sink) Fetch(ctx context.Context, hashes []string) ([]project.Document, error) {
	rows, err := s.pool.Query(ctx, `select hash, kind, name, version, doc::text
		from sidmesh_documents where hash = any($1)`, hashes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []project.Document
	for rows.Next() {
		var d project.Document
		var doc string
		if err := rows.Scan(&d.Hash, &d.Kind, &d.Name, &d.Version, &doc); err != nil {
			return nil, err
		}
		d.Key, d.Doc = project.PinKey(d.Kind, d.Name, d.Version), []byte(doc)
		out = append(out, d)
	}
	return out, rows.Err()
}
