package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	sidmesh "gitlab.com/sidcorp-it/sidmesh/go"
	"gitlab.com/sidcorp-it/sidmesh/go/project"
)

// fakeCall stands in for the workers' sidmesh.call activity, so each test can script how
// capabilities behave.
type fakeCall struct {
	mu       sync.Mutex
	handlers map[string]func(req sidmesh.CallRequest) (sidmesh.Result, error)
	calls    []sidmesh.CallRequest
}

func newFakeCall() *fakeCall {
	return &fakeCall{handlers: map[string]func(sidmesh.CallRequest) (sidmesh.Result, error){}}
}

func (f *fakeCall) on(key string, h func(req sidmesh.CallRequest) (sidmesh.Result, error)) *fakeCall {
	f.handlers[key] = h
	return f
}

func (f *fakeCall) Call(_ context.Context, req sidmesh.CallRequest) (sidmesh.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	h := f.handlers[sidmesh.Key(req.Capability, req.Version)]
	f.mu.Unlock()
	if h == nil {
		return sidmesh.Result{}, temporal.NewNonRetryableApplicationError("no fake for "+req.Capability, sidmesh.ErrUnknownCapability, nil)
	}
	return h(req)
}

func (f *fakeCall) callsTo(capability string) []sidmesh.CallRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sidmesh.CallRequest
	for _, c := range f.calls {
		if c.Capability == capability {
			out = append(out, c)
		}
	}
	return out
}

// memSink keeps records in memory.
type memSink struct {
	mu     sync.Mutex
	ledger []LedgerRecord
	tasks  []TaskRecord
	alerts []AlertRecord
	events []EventRecord
	onTask func(TaskRecord) // called after a task record is stored, outside the lock
}

func (m *memSink) WriteEvent(_ context.Context, r EventRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, r)
	return nil
}

func (m *memSink) WriteLedger(_ context.Context, r LedgerRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ledger = append(m.ledger, r)
	return nil
}

func (m *memSink) WriteTask(_ context.Context, r TaskRecord) error {
	m.mu.Lock()
	m.tasks = append(m.tasks, r)
	hook := m.onTask
	m.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	return nil
}

func (m *memSink) WriteAlert(_ context.Context, r AlertRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts = append(m.alerts, r)
	return nil
}

func (m *memSink) taskEvents() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, t := range m.tasks {
		out = append(out, t.Event)
	}
	return out
}

// fakeStarter records signal-with-start requests instead of reaching a server.
type fakeStarter struct {
	mu   sync.Mutex
	reqs []SignalWithStartRequest
}

func (f *fakeStarter) SignalWithStart(_ context.Context, req SignalWithStartRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return nil
}

func (m *memSink) alertKinds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, a := range m.alerts {
		out = append(out, a.Kind)
	}
	return out
}

// fakeParams is a tenant params source keyed by definition.
type fakeParams struct {
	mu    sync.Mutex
	byDef map[string]map[string]any
	reads []ParamsRequest
}

func (f *fakeParams) TenantParams(_ context.Context, tenant, definition string, version int) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, ParamsRequest{Tenant: tenant, Definition: definition, Version: version})
	return f.byDef[definition], nil
}

type harness struct {
	t      *testing.T
	env    *testsuite.TestWorkflowEnvironment
	calls  *fakeCall
	sink   *memSink
	starts *fakeStarter
	params *fakeParams
	proj   *project.Project
}

// newHarness runs the test project; docs replaces the worker's own documents.
func newHarness(t *testing.T, calls *fakeCall, docs ...*Docs) *harness {
	t.Helper()
	p, err := project.Load("../testdata/project")
	require.NoError(t, err)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	sink := &memSink{}
	if len(docs) == 0 {
		own, err := NewDocs(p, nil, sink)
		require.NoError(t, err)
		docs = append(docs, own)
	}
	env.RegisterWorkflowWithOptions(docs[0].Interpret, workflow.RegisterOptions{Name: WorkflowName})
	env.RegisterActivityWithOptions(calls.Call, activity.RegisterOptions{Name: CallActivityName})
	params := &fakeParams{byDef: map[string]map[string]any{}}
	sinks := Sinks{Ledger: sink, Tasks: sink, Alerts: sink, Events: sink, Params: params}
	env.RegisterActivityWithOptions(sinks.Activity, activity.RegisterOptions{Name: SinkActivityName})
	env.RegisterActivityWithOptions(sinks.ParamsActivity, activity.RegisterOptions{Name: ParamsActivityName})
	starts := &fakeStarter{}
	env.RegisterActivityWithOptions(starts.SignalWithStart, activity.RegisterOptions{Name: SignalWithStartActivityName})
	return &harness{t: t, env: env, calls: calls, sink: sink, starts: starts, params: params, proj: p}
}

// run starts definition@1 as workflow id and returns its result once it completes.
func (h *harness) run(definition, id string, input map[string]any) Result {
	h.t.Helper()
	return h.runReq(StartRequest{Definition: definition, Version: 1, WorkflowID: id, Tenant: "acme", Input: input})
}

func (h *harness) runReq(req StartRequest) Result {
	h.t.Helper()
	require.NoError(h.t, h.exec(req))
	var res Result
	require.NoError(h.t, h.env.GetWorkflowResult(&res))
	return res
}

// exec runs the workflow to completion and returns its error.
func (h *harness) exec(req StartRequest) error {
	h.t.Helper()
	in, err := Prepare(h.proj, req)
	require.NoError(h.t, err)
	h.env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: req.WorkflowID})
	h.env.ExecuteWorkflow(WorkflowName, in)
	require.True(h.t, h.env.IsWorkflowCompleted())
	return h.env.GetWorkflowError()
}

// execDoc runs def as a run started before pinning (definition and contracts carried in the
// start input), so a test can shape a definition in Go. defs are further definitions the run
// can start; the project's own are always available.
func (h *harness) execDoc(def project.Definition, id string, input map[string]any, defs ...project.Definition) error {
	h.t.Helper()
	contracts := map[string]ContractInfo{}
	for key, c := range h.proj.Contracts {
		contracts[key] = contractInfo(c)
	}
	all := map[string]project.Definition{}
	for key, d := range h.proj.Definitions {
		all[key] = *d
	}
	for _, d := range append(defs, def) {
		all[sidmesh.Key(d.ID, d.Version)] = d
	}
	if input == nil {
		input = map[string]any{}
	}
	in := StartInput{Tenant: "acme", Definition: DefinitionRef{Doc: &def}, Definitions: all, Contracts: contracts,
		Queues: h.proj.Manifest.Queues, Input: input}
	h.env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: id})
	h.env.ExecuteWorkflow(WorkflowName, in)
	require.True(h.t, h.env.IsWorkflowCompleted())
	return h.env.GetWorkflowError()
}

// definitionJSON decodes a definition written inline in a test.
func definitionJSON(t *testing.T, doc string) project.Definition {
	t.Helper()
	var d project.Definition
	require.NoError(t, json.Unmarshal([]byte(doc), &d))
	return d
}

func ok(outcome string, output map[string]any) func(sidmesh.CallRequest) (sidmesh.Result, error) {
	return func(sidmesh.CallRequest) (sidmesh.Result, error) {
		return sidmesh.Result{Outcome: outcome, Output: output}, nil
	}
}

func fail(err error) func(sidmesh.CallRequest) (sidmesh.Result, error) {
	return func(sidmesh.CallRequest) (sidmesh.Result, error) { return sidmesh.Result{}, err }
}

var errFlaky = errors.New("vendor timed out")

func fatal(msg string) error {
	return temporal.NewNonRetryableApplicationError(msg, sidmesh.ErrFatal, nil)
}

// itemFakes scripts a normal item: confident suggestion, vendor accepts.
func itemFakes(confidence float64) *fakeCall {
	return newFakeCall().
		on("mapper.suggest@1", func(req sidmesh.CallRequest) (sidmesh.Result, error) {
			return sidmesh.Result{Outcome: "suggested", Output: map[string]any{
				"option_set": "OS-" + req.Input["sku"].(string), "confidence": confidence,
			}}, nil
		}).
		on("vendor.place_order@1", func(req sidmesh.CallRequest) (sidmesh.Result, error) {
			return sidmesh.Result{Outcome: "accepted", Output: map[string]any{"po_id": "PO-" + req.Input["sku"].(string)}}, nil
		}).
		on("vendor.cancel_order@1", ok("cancelled", map[string]any{}))
}
