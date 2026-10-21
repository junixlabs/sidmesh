# SidMesh Go runtime

Go side of SidMesh: the workflow interpreter on Temporal, the Go worker's `sidmesh.call`
activity, and the API a Go node uses. Behaviour follows `../spec/SPEC.md`. Start with the
[root README](../README.md) for concepts and an example; releases and upgrade notes are in
[`../CHANGELOG.md`](../CHANGELOG.md).

Module: `github.com/junixlabs/sidmesh/go`

| Package | What it is for |
| --- | --- |
| `sidmesh` (module root) | What a node imports: `Register`, `Meta`, `Result`, `Fatal`, `IdempotencyKey` |
| `project` | Load a project directory and check every document against the spec schemas |
| `engine` | Interpreter workflow, call activity, sinks, workers, start / resolve helpers |
| `codec` | Payload encryption of SPEC §7 (`SIDMESH_CODEC_KEYS`) |
| `pgsink` | Postgres sink of SPEC §6.1 and the `-sink` / `-records` / `-pg-url` worker flags |
| `cmd/sidmesh-run` | Start, signal and resolve workflows from the command line |
| `examples/demo` | A runnable project: order → items, ai gate, human review, write + lookup |

## Writing a Go node

A node is a Go package under `nodes/<nodeId>/`. Each capability version is one function,
registered from `init`:

```go
func init() { sidmesh.Register("vendor.place_order", 1, PlaceOrderV1) }

func PlaceOrderV1(ctx context.Context, in PlaceOrderV1Input, cfg PlaceOrderV1Config, meta sidmesh.Meta) (sidmesh.Result, error) {
	// meta.IdempotencyKey is set for write capabilities: pass it to the vendor.
	return sidmesh.Result{Outcome: "accepted", Output: map[string]any{"po_id": po}}, nil
}
```

- Return one of the contract's outcomes with its output. The runtime validates input and
  config before the call and the output after it; a mismatch fails the step without retry.
- Return an error to retry under the step's policy. Wrap it with `sidmesh.Fatal` to fail
  the step at once.
- Input, config and output types come from codegen (`contracts_gen.go`).

## Running workers

```go
sinkFlags := pgsink.Register(flag.CommandLine) // -sink jsonl|postgres, -records, -pg-url
flag.Parse()
p, _ := project.Load("path/to/project")
sinks, closeSinks, _ := sinkFlags.Open(ctx)
defer closeSinks()
c, _ := engine.Dial("127.0.0.1:7233") // applies the payload codec when SIDMESH_CODEC_KEYS is set
engine.Run(c, engine.WorkerOptions{Project: p, Sinks: sinks})
```

`Run` starts two workers: the interpreter on `queues.workflow` (workflow, sink and
signal-with-start activities) and the Go activity worker on `queues.go`. Import your node
packages for their `init` registration. Sinks receive ledger, task, alert and event records;
JSONL is the default, `-sink postgres` writes the §6.1 tables (migrations run at start).
Records are written per step in one local activity, so a step adds one history marker.

Configuration comes from the environment, so a worker in docker compose needs no flags:
`TEMPORAL_ADDRESS`, `TEMPORAL_NAMESPACE`, `SIDMESH_CODEC_KEYS` (codec off when unset),
`SIDMESH_PG_URL` (selects the Postgres sink), `SIDMESH_SINK`, `SIDMESH_RECORDS`. Every worker
and client of a deployment must dial through `engine.Dial` with the same codec keys.

Search attributes named in a definition's `search` must exist on the namespace as Keyword:

```bash
temporal operator search-attribute create --name OrderId --type Keyword
```

## Starting and steering workflows

```go
run, err := engine.Start(ctx, c, p, engine.StartRequest{
	Definition: "order", Version: 1, WorkflowID: "acme:shop:1001", Tenant: "acme",
	Input: map[string]any{"order_id": "1001"},
})
// Resolves human step "review" through the Temporal Update "human"; a resolution the
// workflow rejects comes back as err and the task stays open.
err = engine.ResolveTask(ctx, c, "acme:shop:1001/items/B",
	engine.Resolution{StepID: "review", Outcome: "approve", Output: map[string]any{"option_set": "…"}, Actor: "ann"})
```

The workflow id is the caller's; a second start with the same id is rejected. Child ids are
`<parentId>/<stepId>/<key>`. The start input pins the definition, every definition it can
start and every contract they call by content hash (`sha256` of the JCS document); workers
and `sidmesh-run` publish their project's documents to `sidmesh_documents` when
`SIDMESH_PG_URL` is set, and a worker missing a pinned document fetches it from there (the
workflow task is retried meanwhile).

## Demo

```bash
# from go/, with a Temporal dev server on 127.0.0.1:7233 (`temporal server start-dev`)
go build -o bin/ ./cmd/sidmesh-run ./examples/demo/worker
bin/worker -project examples/demo -records /tmp/demo-records.jsonl &
bin/sidmesh-run start -project examples/demo -definition order -id demo:shop:1 -tenant demo -input '{"order_id":"1"}'
bin/sidmesh-run resolve -id demo:shop:1/items/B -step review -outcome approve -output '{"option_set":"T-shirt 2D - B - Sand"}' -actor me
bin/sidmesh-run signal -id demo:shop:1/items/A -name shipped -payload '{"tracking":"1Z-A"}'
bin/sidmesh-run signal -id demo:shop:1/items/B -name shipped -payload '{"tracking":"1Z-B"}'
bin/sidmesh-run result -id demo:shop:1
```

The demo handlers use the types in `nodes/*/contracts_gen.go`, generated from the contracts
by the TS tooling (`node dist/cli.js codegen ../go/examples/demo` from `ts/`); regenerate
them after changing a contract.

Item B goes to human review (confidence 0.41 < 0.8). `resolve` is a Temporal Update: a
resolution for another step, an undeclared outcome or an invalid output is rejected at once
with the reason, and the task stays open. Item A's first `place_order` attempt
times out after the vendor recorded it; the engine asks `find_order` and takes the earlier
result instead of ordering twice.

## Tests

```bash
scripts/remote go 'go test -race ./...'
```

`engine/testdata/histories/<release>/` hold histories recorded from the demo on a real
server, one directory per release; `TestReplayRecordedHistories` replays all of them. An
interpreter change that alters the commands a running workflow produces must sit behind a
`workflow.GetVersion` gate (named in `engine/interpret.go`, e.g. `started-flush`) so every
older release still replays; record the new release's histories next to the old ones and
drop a release only when none of its runs can still be open. When the meta-schemas change in `spec/`, run
`go generate ./project` to refresh the embedded copies.
