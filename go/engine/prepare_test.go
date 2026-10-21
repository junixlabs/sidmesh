package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/junixlabs/sidmesh/go/project"
)

func TestPreparePinsEverythingTheWorkflowNeeds(t *testing.T) {
	p, err := project.Load("../testdata/project")
	require.NoError(t, err)
	in, err := Prepare(p, StartRequest{Definition: "order", Version: 1, Tenant: "acme", Input: map[string]any{"order_id": "1"}})
	require.NoError(t, err)
	require.Equal(t, "definition:order@1", in.Definition.Key)
	keys := []string{"definition:order@1", "definition:item@1"} // spawned definitions are pinned too
	for _, c := range []string{"shop.fetch_order@1", "mapper.suggest@1", "vendor.place_order@1", "vendor.find_order@1", "vendor.cancel_order@1", "vendor.find_cancel@1"} {
		keys = append(keys, "contract:"+c)
	}
	keys = append(keys, "node:shop", "node:mapper", "node:vendor")
	require.Len(t, in.Pins, len(keys))
	for _, k := range keys {
		require.Equal(t, p.Documents[k].Hash, in.Pins[k])
		require.Regexp(t, `^sha256:[0-9a-f]{64}$`, in.Pins[k])
	}
	require.Nil(t, in.Definition.Doc, "documents are not carried")
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	var shape map[string]any
	require.NoError(t, json.Unmarshal(raw, &shape))
	require.ElementsMatch(t, []string{"tenant", "definition", "pins", "queues", "input"}, keysOf(shape), "SPEC §5 start input")
	require.Equal(t, "test-go", in.Queues.Go)
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPrepareRejectsBadStarts(t *testing.T) {
	p, err := project.Load("../testdata/project")
	require.NoError(t, err)
	for name, req := range map[string]StartRequest{
		"unknown definition": {Definition: "nope", Version: 1},
		"input fails schema": {Definition: "order", Version: 1, Input: map[string]any{"order": 1}},
		"undeclared param":   {Definition: "item", Version: 1, Input: map[string]any{"order_id": "1", "sku": "A"}, Params: map[string]any{"x": 1}},
		"param fails schema": {Definition: "item", Version: 1, Input: map[string]any{"order_id": "1", "sku": "A"}, Params: map[string]any{"threshold": "high"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Prepare(p, req)
			require.Error(t, err)
		})
	}
}

func TestMissingPinFailsTheTaskWithOneAlert(t *testing.T) {
	sink := &memSink{}
	empty, err := NewDocs(nil, nil, sink) // a worker with none of the documents, no registry
	require.NoError(t, err)
	h := newHarness(t, newFakeCall(), empty)
	h.sink = sink
	err = h.exec(StartRequest{Definition: "flow", Version: 1, WorkflowID: "acme:flow:40", Tenant: "acme",
		Input: map[string]any{"kind": "other"}})
	require.ErrorContains(t, err, "is not available on this worker")
	require.Equal(t, []string{AlertPinMissing}, h.sink.alertKinds())
	require.Equal(t, "definition:flow@1", h.sink.alerts[0].Alert)
	require.Equal(t, "acme", h.sink.alerts[0].Tenant)
}

// fakeRegistry holds documents by hash.
type fakeRegistry map[string]project.Document

func (f fakeRegistry) Publish(_ context.Context, docs []project.Document) error {
	for _, d := range docs {
		if _, ok := f[d.Hash]; !ok {
			f[d.Hash] = d
		}
	}
	return nil
}

func (f fakeRegistry) Fetch(_ context.Context, hashes []string) ([]project.Document, error) {
	var out []project.Document
	for _, h := range hashes {
		if d, ok := f[h]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func TestRegistryFillsAWorkerThatLacksADocument(t *testing.T) {
	p, err := project.Load("../testdata/project")
	require.NoError(t, err)
	reg := fakeRegistry{}
	publisher, err := NewDocs(p, reg, nil)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(context.Background(), p))
	require.Len(t, reg, len(p.Documents))

	other, err := NewDocs(nil, reg, nil)
	require.NoError(t, err)
	hash := p.Documents["definition:order@1"].Hash
	other.fetch(hash)
	other.mu.Lock()
	r := other.byHash[hash]
	other.mu.Unlock()
	require.NotNil(t, r, "fetched from the registry")
	require.Equal(t, "order", r.def.ID)

	// A worker that misses a pin fetches it at once; the retried task then runs, and no
	// pin.missing is raised because the refresh found the document.
	sink := &memSink{}
	lacking, err := NewDocs(nil, reg, sink)
	require.NoError(t, err)
	h := newHarness(t, newFakeCall(), lacking)
	req := StartRequest{Definition: "flow", Version: 1, WorkflowID: "acme:flow:41", Tenant: "acme", Input: map[string]any{"kind": "other"}}
	require.ErrorContains(t, h.exec(req), "is not available")
	require.Eventually(t, func() bool {
		lacking.mu.Lock()
		defer lacking.mu.Unlock()
		return lacking.fetched[p.Documents["definition:flow@1"].Hash]
	}, 5*time.Second, 10*time.Millisecond)
	retry := newHarness(t, newFakeCall(), lacking)
	require.Equal(t, "completed", retry.run("flow", "acme:flow:41", map[string]any{"kind": "other"}).Status)
	require.Empty(t, sink.alerts, "found by the refresh: no pin.missing")

	reg[hash] = project.Document{Key: "definition:order@1", Hash: hash, Doc: []byte(`{"id":"forged"}`)}
	forged, _ := NewDocs(nil, reg, nil)
	forged.fetch(hash)
	require.Nil(t, forged.byHash[hash], "a document whose bytes do not match its hash is refused")
}
