# Changelog

SidMesh releases the spec, the Go module and the TypeScript package together under one
version. Before 1.0 every minor release may break: read its upgrade notes before upgrading.
Spec-level detail is in [`spec/SPEC.md`](spec/SPEC.md) under *Changes*.

Every release lists:

- **Spec**: what changed in the documents and in execution semantics.
- **Runtimes**: Go and TypeScript changes, including public API changes.
- **Upgrade notes**: what a project must do, in order, and whether running workflows are
  affected.

## v0.3.0 (2026-10-21)

### Spec

- `sidmesh.json` may declare `spec` (`"0.3"`). A toolchain refuses a project that declares
  a spec it does not implement (`PRJ_SPEC_UNSUPPORTED`). Optional now, required from 1.0.
- Every declared step and signal is in the binding scope, `null` until set. CEL reads an
  unset one as `null` instead of failing, and an `expr` search binding works before the
  first step.
- A cancelled workflow stops at the running call: no lookup, no `on.unknown`. Its `ended`
  event is written with status `cancelled`.
- A `goto` interrupt during `collect` puts back the payloads it had taken.
- Spawn: duplicate `key` values fail the step before any child starts. From the second
  execution of the same spawn step, child ids are `<parentId>/<stepId>#<n>/<key>`.
- Signal payloads carry a delivery id `$sidmesh_id`; the receiver drops a payload it has
  already accepted, so a retried signal-with-start is not delivered twice.
- A human step accepts its resolution as soon as its task is recorded.
- `compensate` takes an optional `config`, checked by `DEF_INPUT_MISSING` /
  `DEF_INPUT_EXTRA` like a call's.
- Each behaviour change is behind a `workflow.GetVersion` gate: runs started before v0.3
  replay unchanged.

### Runtimes

- `sidmesh impact` also follows `signal` steps with `start`, the same way pinning does.
- Go: CEL programs and JSON schemas are compiled once and shared (bounded caches); a spawn
  builds its binding scope once for all items; the call activity serialises input and output
  once; document publishing is one Postgres round trip; `sidmesh-run` publishes only the
  documents a start pins, after validating it.
- TS: the worker validates the project once at start; document hashes are computed once per
  catalog; ad-hoc definitions compile on a short-lived Ajv instead of growing the shared one;
  document publishing is one Postgres round trip and runs its DDL once per process.
- Go API: `project.Document` gains `Kind`, `Name`, `Version`; new `project.ParsePinKey`, the
  `Kind*` and `*Pin` constants, `project.SpecVersion`, `pgsink.EnvURL`. `engine.StartInput`
  gains `Seen` (delivery ids carried across continue-as-new).
- TS API: new `SPEC_VERSION`. Removed the unused fields `NodeEntry.contracts` and
  `CatalogContract.runtime`. `validateParams` without an `ajv` argument no longer caches
  compiled schemas.

### Upgrade notes

1. **Deploy order matters.** Upgrade every worker that receives signals (the interpreter on
   `queues.workflow`) before any worker or client that sends them. A v0.2 interpreter treats
   a payload carrying `$sidmesh_id` as invalid and drops it with an alert.
2. Add `"spec": "0.3"` to `sidmesh.json`.
3. If a definition binds `compensate` to a capability whose `config` has required fields,
   bind them in `compensate.config`; v0.2 always sent `{}`, which failed on the worker.
4. Code outside SidMesh that builds child workflow ids by hand must handle the `#<n>`
   suffix on repeated spawns.
5. Rebuild the TS package (`pnpm build`), run `sidmesh codegen` and `sidmesh validate`, and
   run the project's tests.
6. Running workflows: no action. Each new run records a `null-scope` version marker; older
   runs replay on the old paths.

## v0.2 (2026-10-16)

See `spec/SPEC.md` *Changes*: end `drain`, `collect.max` binding, `events` sink, payload
encryption, pinning by content hash with the `sidmesh_documents` registry, tenant params,
signal `start.once`. Not released as a tagged version.

## v0.1.1, v0.1 (2026-10-16)

See `spec/SPEC.md` *Changes*. Not released as tagged versions.
