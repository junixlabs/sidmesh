package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.temporal.io/sdk/workflow"

	sidmesh "github.com/junixlabs/sidmesh/go"
	"github.com/junixlabs/sidmesh/go/project"
)

// Registry is the content-addressed document store of SPEC §6.1 (sidmesh_documents).
type Registry interface {
	Publish(ctx context.Context, docs []project.Document) error             // insert if absent
	Fetch(ctx context.Context, hashes []string) ([]project.Document, error) // the ones it has
}

// Docs resolves pins to documents: the worker's own project, plus whatever the registry
// holds. A hash always names the same bytes, so a lookup is deterministic.
type Docs struct {
	mu       sync.Mutex
	byHash   map[string]*resolved
	registry Registry
	alerts   AlertSink
	fetching map[string]bool   // registry fetch in flight
	fetched  map[string]bool   // a fetch completed (found or not)
	alerted  map[string]bool   // workflow id + hash: pin.missing written
	runtimes map[string]string // node id -> runtime, from the worker's own project
}

type resolved struct {
	def  *project.Definition
	con  *project.Contract
	node *project.Node

	schemas sync.Map // schema name -> *jsonschema.Schema compiled from this document
}

// schema compiles one of the document's schemas once per process; name identifies it within
// the document and labels its validation errors.
func (r *resolved) schema(name string, doc map[string]any) (*jsonschema.Schema, error) {
	if s, ok := r.schemas.Load(name); ok {
		return s.(*jsonschema.Schema), nil
	}
	s, err := project.Compile(name, doc)
	if err != nil {
		return nil, err
	}
	actual, _ := r.schemas.LoadOrStore(name, s)
	return actual.(*jsonschema.Schema), nil
}

// NewDocs indexes p's documents. registry and alerts may be nil.
func NewDocs(p *project.Project, registry Registry, alerts AlertSink) (*Docs, error) {
	d := &Docs{byHash: map[string]*resolved{}, registry: registry, alerts: alerts,
		fetching: map[string]bool{}, fetched: map[string]bool{}, alerted: map[string]bool{}, runtimes: map[string]string{}}
	if p != nil {
		for id, n := range p.Nodes {
			d.runtimes[id] = n.Runtime
		}
		for _, doc := range p.Documents {
			if err := d.add(doc); err != nil {
				return nil, err
			}
		}
	}
	return d, nil
}

func (d *Docs) add(doc project.Document) error {
	r := &resolved{}
	kind, _, _, err := project.ParsePinKey(doc.Key)
	switch kind {
	case project.KindDefinition:
		r.def = &project.Definition{}
		err = json.Unmarshal(doc.Doc, r.def)
	case project.KindNode:
		r.node = &project.Node{}
		err = json.Unmarshal(doc.Doc, r.node)
	case project.KindContract:
		r.con = &project.Contract{}
		err = json.Unmarshal(doc.Doc, r.con)
	}
	if err != nil {
		return fmt.Errorf("document %s: %w", doc.Key, err)
	}
	d.mu.Lock()
	d.byHash[doc.Hash] = r
	d.mu.Unlock()
	return nil
}

// Publish writes the project's documents to the registry, if there is one.
func (d *Docs) Publish(ctx context.Context, p *project.Project) error {
	if d.registry == nil {
		return nil
	}
	return d.registry.Publish(ctx, slices.Collect(maps.Values(p.Documents)))
}

// get returns the document for hash. On a miss it panics, which fails the workflow task so
// Temporal retries it; meanwhile one registry fetch of the hash runs in the background, so
// the retry usually finds it. pin.missing is written once per workflow and hash, when the
// hash is still missing after a completed fetch (or there is no registry).
func (d *Docs) get(ctx workflow.Context, tenant, key, hash string) *resolved {
	d.mu.Lock()
	if r, ok := d.byHash[hash]; ok {
		d.mu.Unlock()
		return r
	}
	d.unavailable(ctx, tenant, key, hash)
	return nil
}

// unavailable is get's miss: it starts the registry fetch, writes pin.missing when due and
// panics. Call it with d.mu held, after finding no document for hash; it releases d.mu.
func (d *Docs) unavailable(ctx workflow.Context, tenant, key, hash string) {
	id := workflow.GetInfo(ctx).WorkflowExecution.ID
	inFlight, fetched := d.fetching[hash], d.fetched[hash]
	alert := (d.registry == nil || fetched) && !inFlight && !d.alerted[id+hash]
	if alert {
		d.alerted[id+hash] = true
	}
	if d.registry != nil && !inFlight {
		d.fetching[hash] = true
		go d.fetch(hash)
	}
	d.mu.Unlock()
	if alert && d.alerts != nil {
		_ = d.alerts.WriteAlert(context.Background(), AlertRecord{
			Tenant: tenant, WorkflowID: id, Kind: AlertPinMissing, Alert: key,
			DedupeKey: "pin.missing:" + id + ":" + hash,
			Message:   "no document " + hash + " for " + key + " on this worker or in the registry", Time: time.Now(),
		})
	}
	panic(fmt.Sprintf("pinned document %s (%s) is not available on this worker", key, hash))
}

// runtime is the runtime of a pinned node; a run pinned before node pins existed falls back
// to the worker's own project. An unknown node fails the task like a missing pin.
func (d *Docs) runtime(ctx workflow.Context, in StartInput, node string) string {
	pk := project.NodePin + node
	if hash, ok := in.Pins[pk]; ok {
		return d.get(ctx, in.Tenant, pk, hash).node.Runtime
	}
	if rt, ok := d.runtimes[node]; ok {
		return rt
	}
	d.mu.Lock()
	d.unavailable(ctx, in.Tenant, pk, pk) // no pin, so the key stands in for the hash
	return ""
}

func (d *Docs) fetch(hash string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	docs, err := d.registry.Fetch(ctx, []string{hash})
	if err == nil {
		for _, doc := range docs {
			if h, herr := sidmesh.DocHash(doc.Doc); herr == nil && h == doc.Hash {
				err = d.add(doc)
			}
		}
	}
	d.mu.Lock()
	delete(d.fetching, hash)
	d.fetched[hash] = true
	d.mu.Unlock()
}
