package pgsink

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
	"gitlab.com/sidcorp-it/sidmesh/go/engine"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

// open connects to SIDMESH_TEST_PG_URL (a throwaway database) in a fresh schema.
func open(t *testing.T) *Sink {
	t.Helper()
	url := os.Getenv("SIDMESH_TEST_PG_URL")
	if url == "" {
		t.Skip("SIDMESH_TEST_PG_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	schemaName := fmt.Sprintf("t%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "create schema "+schemaName)
	require.NoError(t, err)
	admin.Close()
	cfg, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schemaName
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	s, err := New(ctx, pool)
	require.NoError(t, err)
	_, err = New(ctx, pool) // migrations are idempotent
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

func TestSinkTables(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)

	require.NoError(t, s.WriteEvent(ctx, engine.EventRecord{Time: now, Tenant: "acme", WorkflowID: "w1",
		Definition: "order", Version: 1, Kind: engine.EventStarted}))
	require.NoError(t, s.WriteEvent(ctx, engine.EventRecord{Time: now, Tenant: "acme", WorkflowID: "w1", ParentID: "p",
		Definition: "order", Version: 1, Kind: engine.EventStep, StepID: "fetch", StepType: "call",
		Outcome: "found", StartedAt: now, EndedAt: now.Add(time.Second)}))
	var kind, outcome string
	var parent *string
	var started, ended *time.Time
	require.NoError(t, s.pool.QueryRow(ctx, `select kind, outcome, parent_id, started_at, ended_at from sidmesh_events
		where workflow_id = 'w1' and kind = 'step'`).Scan(&kind, &outcome, &parent, &started, &ended))
	require.Equal(t, "found", outcome)
	require.Equal(t, "p", *parent)
	require.Equal(t, time.Second, ended.Sub(*started))

	c := 0.7
	require.NoError(t, s.WriteLedger(ctx, engine.LedgerRecord{Time: now, Tenant: "acme", WorkflowID: "w1", StepID: "map",
		Capability: "variant.map", Version: 1, Outcome: "mapped", Confidence: &c, Gated: true,
		Fields: map[string]any{"input.sku": "A"}}))
	var conf float64
	var fields map[string]any
	require.NoError(t, s.pool.QueryRow(ctx, `select confidence, fields from sidmesh_ledger where capability = 'variant.map'`).Scan(&conf, &fields))
	require.Equal(t, 0.7, conf)
	require.Equal(t, map[string]any{"input.sku": "A"}, fields)

	task := engine.TaskRecord{TaskID: "w1#review#1", Tenant: "acme", WorkflowID: "w1", StepID: "review", Task: "variant.review",
		Event: engine.TaskOpened, Input: map[string]any{"sku": "A"}, Outcomes: []string{"approve", "reject"}, Time: now}
	require.NoError(t, s.WriteTask(ctx, task))
	require.NoError(t, s.WriteTask(ctx, task)) // a retried open does not duplicate
	task.Event, task.Outcome, task.Output, task.Actor, task.Time = engine.TaskResolved, "approve", map[string]any{"option_set": "X"}, "ann", now.Add(time.Minute)
	require.NoError(t, s.WriteTask(ctx, task))
	var status, actor string
	var output map[string]any
	var n int
	require.NoError(t, s.pool.QueryRow(ctx, `select count(*) over (), status, actor, output from sidmesh_tasks`).Scan(&n, &status, &actor, &output))
	require.Equal(t, 1, n)
	require.Equal(t, "resolved", status)
	require.Equal(t, "ann", actor)
	require.Equal(t, map[string]any{"option_set": "X"}, output)

	cancelled := engine.TaskRecord{TaskID: "w1#review#2", Tenant: "acme", WorkflowID: "w1", StepID: "review", Task: "variant.review",
		Event: engine.TaskOpened, Time: now}
	require.NoError(t, s.WriteTask(ctx, cancelled))
	cancelled.Event = engine.TaskInterrupted
	require.NoError(t, s.WriteTask(ctx, cancelled))
	require.NoError(t, s.pool.QueryRow(ctx, `select status from sidmesh_tasks where task_id = 'w1#review#2'`).Scan(&status))
	require.Equal(t, "cancelled", status)

	alert := engine.AlertRecord{Time: now, Tenant: "acme", WorkflowID: "w1", StepID: "ship", Kind: engine.AlertSLA,
		Alert: "slow", DedupeKey: "sla:w1:ship:slow", Message: "late"}
	require.NoError(t, s.WriteAlert(ctx, alert))
	require.NoError(t, s.WriteAlert(ctx, alert))
	require.NoError(t, s.pool.QueryRow(ctx, `select count(*) from sidmesh_alerts`).Scan(&n))
	require.Equal(t, 1, n, "same dedupe_key is stored once")
}

func TestTenantParams(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	got, err := s.TenantParams(ctx, "acme", "item", 1)
	require.NoError(t, err)
	require.Nil(t, got, "no row is an empty layer")
	_, err = s.pool.Exec(ctx, `insert into sidmesh_params (tenant, definition, version, params)
		values ('acme', 'item', 1, '{"threshold": 0.9}')`)
	require.NoError(t, err)
	got, err = s.TenantParams(ctx, "acme", "item", 1)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"threshold": 0.9}, got)
}

func TestDocumentRegistry(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	p, err := project.Load("../testdata/project")
	require.NoError(t, err)
	docs := make([]project.Document, 0, len(p.Documents))
	for _, d := range p.Documents {
		docs = append(docs, d)
	}
	require.NoError(t, s.Publish(ctx, docs))
	require.NoError(t, s.Publish(ctx, docs), "insert if absent")
	for _, key := range []string{"contract:vendor.place_order@1", "node:vendor"} {
		want := p.Documents[key]
		got, err := s.Fetch(ctx, []string{want.Hash, "sha256:none"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, want.Key, got[0].Key)
		h, err := sidmesh.DocHash(got[0].Doc)
		require.NoError(t, err)
		require.Equal(t, want.Hash, h, "jsonb round trip keeps the JCS hash")
	}
	var version int
	require.NoError(t, s.pool.QueryRow(ctx, `select version from sidmesh_documents where kind = 'node' and name = 'vendor'`).Scan(&version))
	require.Zero(t, version, "nodes are stored with version 0")
}
