# @sidmesh/core

TypeScript side of SidMesh: authoring tools (validate, impact, codegen, scaffolding) and the
TS activity runtime. The rules it enforces are in [`../spec/SPEC.md`](../spec/SPEC.md); the
meta-schemas are read from `../spec` at run time (override with `SIDMESH_SPEC_DIR`). Start
with the [root README](../README.md) for concepts and an example; releases and upgrade notes
are in [`../CHANGELOG.md`](../CHANGELOG.md).

## Commands

```bash
sidmesh validate [projectDir]                 # every SPEC §2–§3 rule; exit 1 on any issue
sidmesh impact <capability|definition> [dir]  # what a change touches, from definitions
sidmesh codegen [projectDir]                  # contracts.gen.ts / contracts_gen.go per node
sidmesh new-node <id> --runtime go|ts [dir]   # manifest, example contract, handler stub
sidmesh conformance [spec/conformance]        # run the shared fixtures
```

Issues print as `CODE  file#pointer  message`. `codegen` and `impact` refuse to run on a
project that does not validate.

## Runtime

- `createCallActivity(project, handlers)` builds the `sidmesh.call` activity (SPEC §4.1).
  `handlers` is keyed by node id, then `"<verb>@<N>"`, exactly what each node's `index.ts`
  exports. The wrapper validates input and config before the handler and the outcome and
  output after it; failures are non-retryable `INPUT_INVALID`, `OUTPUT_INVALID`, `FATAL`
  (throw `FatalError`) or `UNKNOWN_CAPABILITY`. Any other error is retried.
- `runWorker({ projectDir, handlers })` from `@sidmesh/core/worker` runs it on the
  project's `queues.ts` task queue.
- `idempotencyKey(tenant, capability, version, values)` is the SPEC §4 key, with values
  encoded per RFC 8785 so the Go runtime computes the same key.
- `connect({ projectDir })` from `@sidmesh/core/client`: `start`, `signal`, `resolve` (the
  `human` update), `result`, `cancel`, `terminate`, `list(query)`. `start` sends `pins`
  (`sha256:` of each document's RFC 8785 form, SPEC §5), not the documents.
- Configuration comes from the environment, so an app needs no glue: `TEMPORAL_ADDRESS`,
  `TEMPORAL_NAMESPACE`, `SIDMESH_CODEC_KEYS` (payload codec, §7; off when unset) and
  `SIDMESH_PG_URL` (when set, `runWorker` and `connect` publish the project's documents to
  `sidmesh_documents` at startup, insert-if-absent).
- `handleCodecRequest(codec, path, body)` is the `/encode` `/decode` codec-server logic, to
  mount behind the app's auth. `validateParams(definition, params)` checks tenant params.
- `pnpm build` output (`dist/`, which includes the spec schemas) plus production
  dependencies is all a runtime image needs.

### Building and running TS nodes (for now)

There is no build step for an app's nodes yet. Until one is decided:

1. Build this package once: `pnpm build` writes `dist/`, which is what an app imports
   (`@sidmesh/core` and `@sidmesh/core/worker`, or a relative path to `dist/` as
   `examples/e2e/worker.ts` does).
2. Write the worker as a single `worker.ts` that imports every node's `index.ts` and passes
   them to `runWorker({ projectDir, handlers })`, keyed by node id.
3. Run it with Node 24 directly (`node worker.ts`). Node strips the types, so the app needs
   no `tsc` and no bundler. Type stripping only removes annotations: no `enum`, no
   `namespace`, no parameter properties, and relative imports must name the `.ts` file.
4. Type-check separately with `tsc --noEmit` against the generated `contracts.gen.ts`
   (`sidmesh codegen`). Running never depends on it.

Rerun `pnpm build` here whenever this package changes; a stale `dist/` is the usual cause
of a worker that disagrees with `sidmesh validate`.

## End to end

`examples/e2e` is a one-node TS project (`greeter.greet`) and a `hello` definition whose
call step runs on the TS queue while B's Go interpreter drives the workflow.
`examples/e2e/run.sh` starts both workers, runs one workflow per outcome and prints the
results (`{"status":"completed","output":{"runtime":"ts",...}}` and a `refused` failure).

## Development

```bash
pnpm install && pnpm typecheck && pnpm test
```

Tests cover each rule with a one-change mutation of a valid fixture, codegen output that
must compile (`tsc --strict`, `go vet`), the runtime wrapper, and the shared vectors in
`spec/conformance` (idempotency keys, payload checks, fixture projects).
