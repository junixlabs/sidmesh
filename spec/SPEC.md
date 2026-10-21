# SidMesh spec v0.3

SidMesh builds a system out of small nodes with explicit contracts. A **node** owns
capabilities. A **contract** says what one capability version takes, the outcomes it can
finish with, and what it is allowed to touch. A **definition** wires capabilities into a
workflow graph. Where a node runs (same process, another process, a durable worker) is
deployment configuration; the contract and the definition never change because of it.

The three JSON Schemas next to this file define the shape of each document. This file
defines everything a schema cannot: project layout, semantic rules (each with the error
code a validator must report), and execution semantics shared by every implementation.

Implementations: `ts/` owns authoring tooling (validate, impact, codegen, scaffolding) and
the TypeScript activity runtime. `go/` owns the Go activity runtime and the workflow
interpreter on Temporal. Both read the same files. Fixtures in `conformance/` are the
tie-breaker when the two disagree.

## 1. Project layout

```
<project>/
  sidmesh.json                         project name and task queue names
  nodes/<nodeId>/node.json             node manifest
  nodes/<nodeId>/contracts/<verb>.v<N>.json
  nodes/<nodeId>/...                   handler code (Go package or TS module)
  definitions/<id>.v<N>.json           workflow definitions
```

Discovery is by convention; there is no list of nodes to keep in sync.

`sidmesh.json`:

| Field | Meaning |
| --- | --- |
| `name` | Project name, `^[a-z][a-z0-9-]*$` |
| `spec` | Spec version the project is written for, a string `MAJOR.MINOR` (`^[0-9]+\.[0-9]+$`), e.g. `"0.3"`. Optional in v0.3 (absent means no check); required from v1.0 |
| `queues.workflow` | Task queue the interpreter listens on. Default `<name>-workflows` |
| `queues.go` | Task queue for Go activities. Default `<name>-go` |
| `queues.ts` | Task queue for TS activities. Default `<name>-ts` |

## 2. Strict schema profile

Every object schema in a contract (`config`, `input`, each outcome `output`) and in a
definition (`input`, signal schemas, human outcome schemas) follows this profile, so code
generation is predictable in both languages:

- `type` is one of `object`, `array`, `string`, `integer`, `number`, `boolean`, or a
  two-element array `[T, "null"]` for nullable.
- An object declares `properties` and sets `additionalProperties: false`. `required` lists
  only declared properties.
- An array declares `items`.
- Allowed keywords besides those: `title`, `description`, `enum`, `const`, `minimum`,
  `maximum`, `minLength`, `maxLength`, `pattern`, `format`, `minItems`, `maxItems`,
  `default`, `$defs`, and `$ref` pointing into the same document's `$defs`.
- `format` is one of `date-time`, `date`, `email`, `uri`, `uuid`, and both runtimes assert
  it when validating payloads (TS: ajv-formats; Go: format assertion on). Any other format
  value is a strict-profile violation.
- Not allowed: `oneOf`, `anyOf`, `allOf`, `not`, `if`/`then`/`else`, `patternProperties`,
  `additionalProperties` other than `false`, remote `$ref`.

Violations: `CON_SCHEMA_STRICT` in contracts, `DEF_SCHEMA_STRICT` in definitions. A
document that is not a valid JSON Schema 2020-12 at all: `CON_SCHEMA_INVALID` /
`DEF_SCHEMA_INVALID`.

## 3. Semantic rules

A validator reports every violation it finds, each as `{code, file, pointer, message}`
where `pointer` is a JSON Pointer into the file. All rules are errors.

Pointer conventions: the offending schema node for `*_SCHEMA_STRICT`; the offending keyword
for `*_SCHEMA_INVALID`; the step for `DEF_CAPABILITY_UNKNOWN`, `DEF_SPAWN_UNKNOWN`,
`DEF_WAIT_SHAPE`, `DEF_SIGNAL_TARGET`; the `on` object for `DEF_HUMAN_UNROUTED`; the `expr`
or `when` string for `DEF_EXPR_INVALID`; the binding value for binding rules.

Cascades are suppressed: when `start` is unknown, `DEF_UNREACHABLE` is not reported; an
undeclared reference (param, signal, var, capability, definition) is reported once per
place it is used, and rules that would need the missing document are skipped there. A document that
fails its JSON Schema is still registered under its `id`/`capability` and `version` when
those fields are readable, so references to it do not add `*_UNKNOWN` errors; semantic
rules are not run inside it. `DEF_DRAIN_UNKNOWN` points at the offending array element.

### Project

| Code | Rule |
| --- | --- |
| `PRJ_MANIFEST_INVALID` | `sidmesh.json` missing or malformed (a `spec` that is not a `MAJOR.MINOR` string: pointer `/spec`) |
| `PRJ_SPEC_UNSUPPORTED` | `spec` is not supported by the toolchain (pointer `/spec`). Each toolchain implements one spec version. Before 1.0 the declared `MAJOR.MINOR` must equal it; from 1.0 the major must match and the declared minor must not be greater. The message names both versions and points to the upgrade notes in `CHANGELOG.md`. The Go loader refuses such a project too, so a worker does not run it |
| `PRJ_SHAPE_INVALID` | A document fails its JSON Schema (pointer to the failing location) |
| `PRJ_NODE_DIR_MISMATCH` | Directory name differs from `node.json` `id` |
| `PRJ_DUPLICATE_CAPABILITY` | Two contracts declare the same `capability` + `version` |
| `PRJ_DUPLICATE_DEFINITION` | Two definitions declare the same `id` + `version` |

### Contract

| Code | Rule |
| --- | --- |
| `CON_FILENAME` | File is not named `<verb>.v<version>.json` matching its own fields |
| `CON_CAPABILITY_PREFIX` | `capability` does not start with `<nodeId>.` |
| `CON_RESERVED_OUTCOME` | Declares `failed` or `unknown`; or declares `not_found` without being some write capability's lookup |
| `CON_WRITE_IDEMPOTENCY` | `effects: write` without `idempotency` |
| `CON_WRITE_LOOKUP` | `effects: write` without `lookup` |
| `CON_IDEMPOTENCY_PATH` | An idempotency key path does not resolve to a required property of `input` |
| `CON_LOOKUP_UNKNOWN` | `lookup` names a capability that does not exist, or one of another node |
| `CON_LOOKUP_EFFECTS` | The lookup capability has `effects: write` |
| `CON_LOOKUP_INPUT` | The lookup's `input` schema is not identical to the write's `input` schema |
| `CON_LOOKUP_OUTCOMES` | The lookup's outcomes are not exactly the write's outcomes plus `not_found`, with identical output schemas |
| `CON_AI_WRITE` | `kind: ai` with `effects: write` |
| `CON_AI_CONFIDENCE` | `kind: ai` and some outcome output lacks a required `confidence` of type `number` with `minimum: 0`, `maximum: 1` |
| `CON_LEDGER_PATH` | A ledger path does not resolve in `input`, `config`, or at least one outcome `output` |

"Identical" means equal after parsing JSON (key order ignored).

### Definition

Reachability and dominance are computed on the step graph, where an edge goes from a step
to every step id it names (`on` values, `next`, `else`, `cases[].next`, `gate.below`,
`onChildFailed`). Every signal `goto` target is an additional entry point.

| Code | Rule |
| --- | --- |
| `DEF_STEP_UNKNOWN` | `start`, a `goto`, or any edge target is not a step id |
| `DEF_UNREACHABLE` | A step is not reachable from `start` or any `goto` |
| `DEF_DEAD_END` | A step has no path to an `end` step |
| `DEF_CAPABILITY_UNKNOWN` | A `call` or `compensate` names a capability@version that does not exist |
| `DEF_OUTCOME_UNROUTED` | A `call` lacks `on` for a declared outcome, for `failed`, or (write capability) for `unknown` |
| `DEF_OUTCOME_EXTRA` | A `call` routes an outcome the capability does not declare (other than `failed`/`unknown`; `unknown` on a non-write capability is extra) |
| `DEF_GATE_NOT_AI` | `gate` on a capability whose kind is not `ai` |
| `DEF_INPUT_MISSING` | A required property of the target schema is not bound (call `input`/`config`, compensate `input`/`config`, spawn and signal-start `input` vs the target definition's `input`) |
| `DEF_INPUT_EXTRA` | A bound key is not a property of the target schema (the same bindings as `DEF_INPUT_MISSING`) |
| `DEF_BINDING_ROOT` | A path root is unknown, or `item` is used outside a spawn's `input`/`key` |
| `DEF_BINDING_STEP` | `steps.<id>` names a step that does not dominate the step using it, or the step itself. Exception: a call's `compensate.input` and `compensate.config` may read the call's own `steps.<self>` |
| `DEF_BINDING_PATH` | A path does not resolve in the schema it reads: definition `input`, the param's `schema`, the signal's `schema`, or the referenced step's output (any one outcome is enough) |
| `DEF_PARAM_UNKNOWN` | `params.<name>` is not declared; or a spawn/signal-start `params` key is not a param of the target definition |
| `DEF_PARAM_DEFAULT` | A param `default` does not validate against its `schema` |
| `DEF_SIGNAL_UNKNOWN` | A wait `signal` or a `signals.<name>` binding names an undeclared signal. `human.*` signals are implicit and may not be declared |
| `DEF_WAIT_SHAPE` | A wait does not have exactly one of `signal`/`duration`; or `on.received` missing with `signal`; or `on.elapsed` missing with `duration`; or `timeout` and `on.timeout` not both present or both absent; or `collect` without `signal` |
| `DEF_HUMAN_UNROUTED` | A human step's `on` does not route exactly its declared `outcomes` |
| `DEF_SPAWN_UNKNOWN` | A spawn or signal `start` names a definition@version that does not exist |
| `DEF_VAR_UNKNOWN` | `vars.<name>` or a `set` key is not declared |
| `DEF_VAR_DEFAULT` | A var `default` does not validate against its `schema` |
| `DEF_DURATION_INVALID` | A wait `duration`, `timeout` or `collect.window` given as `const` is not a duration string |
| `DEF_SIGNAL_TARGET` | A signal `start` without `to.workflow`; or `to.children` naming a step that is not a spawn |
| `DEF_DRAIN_UNKNOWN` | An end `drain` names an undeclared signal |
| `DEF_EXPR_INVALID` | A CEL expression (`expr` binding or `cases[].when`) does not parse, or uses a variable that is not one of the roots in §5. Paths inside expressions are checked like bindings. Type errors surface at run time as a failed step |

## 4. Runtime contract for handlers

The engine calls a handler with `(input, config, meta)` and gets back `{outcome, output}`.
The runtime wrapper, not the handler, enforces the contract:

1. Validate `input` and `config` before the handler runs. Invalid: non-retryable failure
   `INPUT_INVALID`.
2. Run the handler.
3. Check `outcome` is declared and `output` validates against that outcome's schema.
   Otherwise: non-retryable failure `OUTPUT_INVALID`.

`meta` carries `workflowId`, `stepId`, `attempt`, `tenant`, and for write capabilities
`idempotencyKey`. The key is the lowercase hex SHA-256 of
`<tenant>\n<capability>@<version>\n<value1>\n<value2>...` over the declared key paths, each
value encoded with RFC 8785 (JCS): no HTML escaping, U+2028 and U+2029 written raw, object
keys sorted, numbers in shortest round-trip form. Handlers pass it to the external system when it supports one.

A handler signals failure by returning an error. An error marked non-retryable (Go:
`sidmesh.Fatal(err)`, TS: `throw new FatalError(...)`) fails the step at once; any other
error is retried under the step's retry policy (step `retry` > contract `defaults.retry` >
engine default `{maxAttempts: 3, initialInterval: "1s", backoff: 2}`). Timeout: step
`timeout` > contract `defaults.timeout` > `30s`.

Handler conventions:

- **Go.** Each node is a Go package at `nodes/<nodeId>/`. A capability `<node>.<verb>` v`N`
  is a function `func <Verb>V<N>(ctx context.Context, in <Verb>V<N>Input, cfg <Verb>V<N>Config, meta sidmesh.Meta) (sidmesh.Result, error)`
  where `Verb` is the verb in PascalCase. Types are generated into `contracts_gen.go`. The
  package registers its handlers in an `init` with `sidmesh.Register`.
- **TS.** Each node is a module at `nodes/<nodeId>/index.ts` exporting `handlers`, an object
  keyed `"<verb>@<N>"`. Types are generated into `contracts.gen.ts`.

### 4.1 Wire envelope between the interpreter and workers

Both runtimes register one Temporal activity named `sidmesh.call` on their own task queue.

- Request: `{capability, version, input, config, meta}` where `meta` is
  `{workflowId, stepId, attempt, tenant, idempotencyKey?}` (`attempt` starts at 1).
- Response: `{outcome, output}`.
- Failures are Temporal application errors whose `type` is one of `INPUT_INVALID`,
  `OUTPUT_INVALID`, `FATAL` (all non-retryable), `UNKNOWN_CAPABILITY` (non-retryable), or
  any other type for retryable handler errors.

The interpreter computes `idempotencyKey`; workers only pass it through.

## 5. Execution semantics

**Starting.** A start request names `definition`, `version`, `workflowId`, `input`,
`tenant`, and optional `params` overrides. `input` validates against the definition input
schema; each override validates against its param schema. The workflow id is chosen by the
caller and should be the source system's key (`<tenant>:<source>:<id>`), so a duplicate
event cannot start a second workflow.

**Tenant params.** Effective params are, lowest first: the definition's defaults, the
tenant's stored params for that definition and version, then the start's (or spawn's, or
signal-start's) overrides. The engine reads the tenant's params once at the start of every
run (including a continue-as-new run, so a changed setting applies from the next run) through
a params source, recorded in history so replay never reads again. A tenant value that fails
its param schema, or names an undeclared param, is ignored with an alert
(`params.tenant_invalid`) and the rest still apply. Without a params source configured the
layer is empty. If the source cannot be read after its retries the run fails rather than running on defaults. Tenant params are configuration, never secrets.

**Version pinning.** A run is pinned to exact documents by content hash, not by carrying
them: the start input holds `pins`, a map from `definition:<id>@<version>` and
`contract:<capability>@<version>` to `sha256:<hex>` of the document's JCS form, for the
definition, every definition it can start, every contract they call (including lookup
and compensate), and `node:<id>` for every node that owns one of those contracts (its runtime
chooses the task queue). Children and continue-as-new runs inherit the parent's pins for what they
use. Documents live in a content-addressed registry (`sidmesh_documents`); every worker and
client publishes the documents of its project at startup (insert if absent, never update)
and holds an immutable in-memory copy keyed by hash, refreshed from the registry. A workflow
task that needs a hash the worker does not have fails and is retried by Temporal (with an
alert `pin.missing` after the first failure) until a worker that has it picks it up; it
never falls back to a different document. Without a registry configured, a worker resolves
pins only from its own project.
The interpreter's start input is `{tenant, definition, pins, queues, input, params?}`, where
`definition` is the root definition's pin key (`definition:<id>@<version>`) and `queues` maps
runtimes to task queues as before.

**Bindings.** Roots: `input`, `params` (defaults, tenant params, then overrides), `vars`,
`steps.<id>.outcome`, `steps.<id>.output`, `steps.<id>.started_at`, `steps.<id>.ended_at`,
`signals.<name>` (latest valid payload received, null if none), `item` (current foreach
element, spawn only), `workflow.id`, `workflow.parent_id`, `workflow.tenant`, `workflow.now`.
Every declared step id and signal name is in scope from the start: a signal not yet received
is `null`; a step that has not run has `outcome`, `output`, `started_at` and `ended_at` all
`null`, and one that ran without an outcome (decide, set, signal, or a step an interrupt
left) has
`outcome` and `output` `null`. So CEL reads them as `null` like a path does; selecting a
field of a `null` value is still a CEL error, so guard it (`steps.x.output != null &&
steps.x.output.n > 0`). Times are
RFC 3339 strings from workflow time, so they are deterministic; CEL can turn them into
timestamps and subtract them. A duration value (wait `duration`, `timeout`,
`collect.window`) must evaluate to a duration string like `7d`.

**Numbers in CEL.** A value enters CEL typed by the schema it comes from: `integer` becomes
`int`, `number` becomes `double`; a number with no schema (e.g. inside a `const`) becomes
`int` when it has no fractional part, else `double`. Comparisons across `int` and `double`
are allowed; arithmetic is not, so an expression mixing them converts explicitly
(`double(x)`). A result assigned to an `integer` var or field must have no fractional part. A path that
does not resolve at run time yields `null`; schema validation of the target then decides.
`{"const": v}` is `v`. `{"expr": "..."}` is CEL with those roots as `dyn` variables.

**Call.** Resolve `input` and `config`, route to the task queue of the owning node's
runtime, run with timeout and retry. Success routes `on[outcome]`. For an `ai` step with a
`gate`, if `output.confidence < threshold` the workflow goes to `gate.below` instead.
Retries exhausted or non-retryable failure routes `on.failed`.

For a **write** capability, `INPUT_INVALID` and `UNKNOWN_CAPABILITY` mean the handler never
ran: route `on.failed` without a lookup. Every other failure (timeout, retryable error,
`FATAL`, `OUTPUT_INVALID`) is ambiguous; `FATAL` and `OUTPUT_INVALID` are not retried.
Before retrying and after the last attempt, the engine runs the `lookup` capability with
the same input and the same config as the write. A lookup outcome other than `not_found` is taken as the result of the
write. `not_found` means the write did not happen: retry if attempts remain, else route
`on.failed`. If the lookup itself fails, route `on.unknown`.

A workflow cancelled (Temporal cancellation) while a call or its lookup runs stops there:
no lookup, no retry, no route; the workflow ends cancelled.

**Decide.** Evaluate `cases` in order; first true wins, else `else`.

**Set.** Resolve every value, validate each against its var schema, assign, then `next`. An
invalid value fails the workflow with `VAR_INVALID`; nothing is assigned. Vars are the way
to merge branches (both branches set the same var) and to count (`vars.n + 1`).

**Wait.** `signal`: continue at `on.received` when the signal arrives (its payload becomes
the step output). With `collect`, keep consuming that signal after the first payload until
`max` payloads or `window` has passed; output is `{items: [...]}`. `duration`: continue at
`on.elapsed` after the timer, output `{}`. `timeout`: continue at `on.timeout` if nothing
arrived in time, output `{}`. `steps.<id>.outcome` is `received`, `elapsed` or `timeout`.
Signals received before the wait began count: each signal name keeps a queue, and a wait
consumes the oldest entry. A `goto` that interrupts a `collect` puts the payloads it took back
at the front of the queue in arrival order, so a later wait or an end `drain` still gets them.

**Signal payloads.** An incoming payload is validated against the declared signal schema.
An invalid payload is dropped and an alert is raised; it never reaches a wait or `goto`.
A payload member `$sidmesh_id` (a string) is a delivery id, not data: it is removed before
validation, and a payload whose id the workflow already accepted is dropped without an alert.
The workflow remembers the newest 1000 accepted ids and carries them across continue-as-new.

**Human.** Record a task (sink interface, §6) and wait for a Temporal Update named `human`
with `{stepId, outcome, output, actor}`. The update validator rejects it synchronously if
the workflow is not waiting on that step, the outcome is not declared, or `output` does
not validate; the caller gets the error and the task stays open. The step accepts a
resolution from the moment its task is recorded. Accepted: continue at `on[outcome]`.

**Spawn.** Start one child workflow per `foreach` element (or one). Child workflow id:
`<parentId>/<stepId>/<key>` on the step's first execution, `<parentId>/<stepId>#<n>/<key>`
on its n-th (n counted like task ids, across continue-as-new), so a spawn that runs again in a
loop never reuses an id. Every key is resolved before any child starts; two elements with the
same key fail the workflow with an engine error naming the key, and no child starts. Children get the spawn's `params` overrides only; the parent's
own overrides are not inherited. `wait: all` blocks until every child ends; step output is
`{children: [{key, status, output}]}`; if any child ended `failed` and `onChildFailed` is
set, go there, else `next`. `wait: none` continues at once with output `{count}`.

**Signal step.** Send `name` with the resolved `payload`, then `next`. Targets: `parent`
(a workflow without a parent fails with `NO_PARENT`); `{workflow}`, with optional `start`
for signal-with-start (the target starts with the given definition, input and params if it
is not running; with `start.once: true` a target that already ran and closed is not started
again and the step continues at `next` with no alert, which is how a source-keyed workflow
stays single-run; this holds only while Temporal still retains the closed execution, so the
namespace retention must outlast the window in which the source can send the key again);
`{children: <spawnStepId>}`, every child of that spawn still running.
The engine adds `$sidmesh_id: "<workflowId>/<firstRunId>/<stepId>/<n>"` (n the step's
execution count) to every payload it sends, the same on every retry of that send, so a
signal-with-start retried after the server accepted it is delivered once. A receiver older
than v0.3 rejects such a payload as invalid: upgrade receivers first.
Without `start`, a target that is not running is not an error: the engine raises an alert
(`signal.target_not_running`) and continues at `next`.

**Interrupt signals.** A declared signal with `goto` is applied at the next step boundary,
or immediately while the workflow is waiting (wait, human, or a spawn with `wait: all`; the
children keep running, and the `goto` path can signal them through `to.children`). A running call is not
cancelled; its result is recorded but not routed.

**Compensate.** Run the `compensate` of every completed call step that declared one and
has not been compensated, newest first, each as a call with the engine default retry. The
compensate input may read the compensated step's own `steps.<self>.output`. A write
compensation gets its own idempotency key; it is not followed by a lookup.
A compensation's `config` bindings (optional, resolved like a call's `config` when the call
completes, and like `input` able to read `steps.<self>`) give its config; without them it is
`{}`. A compensation that fails is recorded and does not stop the others. Then `next`.

**End.** The workflow finishes with `status` and the resolved `output`. With `drain`, if any
listed signal still has queued payloads, the workflow does not finish: it continues as new
(same workflow id, definition, input, params; vars back to their defaults). The new run
receives, before its first step and in arrival order, the queued payloads of the drain
signals; for every other declared signal only the latest valid payload is carried, so
`signals.<name>` keeps its value without the queue growing across runs. `collect.max`
may be an integer or a binding that evaluates to an integer of at least 2.

**SLA.** A step with `sla` that has not finished after `after` raises one alert through the
sink, keyed on workflow id + step id + the step's execution count + `alert`, so a step that
runs again in a loop can alert again. The step keeps running.

**Errors.** A workflow that fails with an engine error (binding, `VAR_INVALID`,
`NO_PARENT`, ...) emits an `ended` event with status `failed`; a workflow cancelled through
Temporal emits one with status `cancelled`, written even though the run is cancelled.

**Search attributes.** Each `search` entry is written as a Keyword search attribute at start
and again whenever its value changes after a step; an unresolved value is written as an
empty string, so search bindings are not subject to dominance. Every attribute must be
registered on the namespace before deployment.

## 6. Sinks

The engine writes four kinds of records through interfaces with a default implementation
that writes JSON lines: **ledger** (one record per call: workflow id, step, capability,
version, outcome, `confidence` for ai, declared ledger fields, gated or not), **tasks**
(human task opened and resolved), **alerts** (SLA breaches, compensation failures, dropped signals), **events** (workflow
started, step finished with its outcome and times, workflow ended (status of its end step,
`failed` on an engine error, `cancelled` when cancelled) or continued as new;
ids, step ids, outcomes and times only, never payloads). Events are what an application
uses for timelines and outbound notifications, so it never has to read payloads back
out of Temporal history. Sink writes should not dominate workflow history: implementations
batch them per step or use local activities. A
deployment replaces the defaults with its own storage. Ledger records never include fields
the contract did not declare.

### 6.1 Postgres sink tables

The reference Postgres sink (Go writes, applications read) uses these tables. Every record
carries `tenant`. Columns not listed are not part of the contract.

| Table | Columns | Notes |
| --- | --- | --- |
| `sidmesh_events` | `id bigserial pk`, `time timestamptz`, `tenant text`, `workflow_id text`, `parent_id text null`, `definition text`, `version int`, `kind text` (`started`, `step`, `ended`, `continued`), `step_id text null`, `step_type text null`, `outcome text null`, `started_at timestamptz null`, `ended_at timestamptz null`, `status text null` | index `(workflow_id, id)`, `(tenant, id)` |
| `sidmesh_ledger` | `id bigserial pk`, `time`, `tenant`, `workflow_id`, `step_id`, `capability text`, `version int`, `outcome text`, `confidence double precision null`, `gated boolean`, `fields jsonb` | index `(capability, version, time)`, `(workflow_id)` |
| `sidmesh_tasks` | `task_id text pk` (`<workflowId>#<stepId>#<n>`, n counts openings of that step), `tenant`, `workflow_id`, `step_id`, `task text`, `status text` (`open`, `resolved`, `cancelled`), `input jsonb`, `outcomes jsonb`, `opened_at timestamptz`, `resolved_at timestamptz null`, `outcome text null`, `output jsonb null`, `actor text null` | one row per task, updated on resolve; index `(tenant, status, opened_at)` |
| `sidmesh_documents` | `hash text pk` (`sha256:<hex>`), `kind text` (`definition`, `contract`, `node`), `name text`, `version int`, `doc jsonb`, `created_at timestamptz` | content-addressed, insert-only; nodes have no version and are stored with `version` 0; index `(kind, name, version)` |
| `sidmesh_params` | `tenant text`, `definition text`, `version int`, `params jsonb`, `updated_at timestamptz` | pk `(tenant, definition, version)`; the params source; written by the application, read by the engine |
| `sidmesh_alerts` | `id bigserial pk`, `time`, `tenant`, `workflow_id`, `step_id text null`, `kind text`, `alert text null`, `dedupe_key text unique`, `message text`, `resolved_at timestamptz null` | an alert with the same `dedupe_key` is not inserted twice |

Migrations are owned by the sink implementation and are idempotent (`create ... if not
exists`). `sidmesh_documents` may also be created by any publisher (worker or client) with the
same DDL, so a client can publish before a sink has migrated.

## 7. Payload encryption

Every payload the engine and the workers exchange through Temporal (workflow input and
output, activity input and output, signals, updates, child workflow input) goes through a
payload codec, so Temporal's database and UI only hold ciphertext.

- Encoding `binary/encrypted`, algorithm AES-256-GCM, a fresh 12-byte nonce per payload,
  stored as `nonce || ciphertext || tag`. Metadata `encryption-key-id` names the key.
- Keys come from the environment: `SIDMESH_CODEC_KEYS="<id>:<base64 32 bytes>,..."`. The
  first key encrypts; any listed key decrypts. Rotation is adding a key in front.
- The original payload (metadata and data) is serialised as a Temporal `Payload` protobuf
  before encryption, so decoding restores it exactly in either language.
- Without `SIDMESH_CODEC_KEYS` the codec is off; a deployment that sets it on one side must
  set it on every worker and client.
- A codec server (Temporal's `/decode` and `/encode` HTTP contract: `{payloads: [{metadata: {<key>: <base64>}, data: <base64>}]}` in and out, as Temporal defines it) is the only way the
  Temporal UI shows plaintext; it is an application concern and must sit behind auth.

## Changes

**v0.3** (2026-10-21) — every declared step and signal is in the binding scope, `null` until
set (CEL reads unset ones as `null`, an expr search binding works before the first step); a
cancelled workflow stops at the running call (no lookup, no `on.unknown`) and its `ended`
event (status `cancelled`) is written; a `goto` during `collect` requeues what it took; spawn
child ids carry `#<n>` from the second execution and duplicate keys fail before any child
starts; signal payloads carry a `$sidmesh_id` delivery id and duplicates are dropped; a human
step accepts its resolution as soon as its task is recorded; compensate `config` (covered by
`DEF_INPUT_MISSING`/`DEF_INPUT_EXTRA`); optional `spec` in `sidmesh.json` declaring the spec
version (required from v1.0), new code `PRJ_SPEC_UNSUPPORTED`.

**v0.2** (2026-10-16) — end `drain` (continue as new carrying queued signals, code
`DEF_DRAIN_UNKNOWN`); `collect.max` may be a binding; `events` sink; payload encryption
(§7); pinning by content hash with the `sidmesh_documents` registry (start input no longer carries documents); tenant params layer and `sidmesh_params`; signal `start.once` (start a closed workflow id at most once).

**v0.1.1** (2026-10-16) — numbers enter CEL typed by their schema; interrupts also apply while a spawn waits for its children;
`workflow.tenant` root; signalling a workflow that is not running raises an alert instead of
failing.

**v0.1** (2026-10-16) — from what the first template could not express:
vars and the `set` step; wait `collect`; signal-with-start and `to.children`; `key` required
on every spawn and spawn `params`; compensate may read its own step; `workflow.now` and step
times; wait outcomes and duration values defined; human resolution through a Temporal Update;
signal payload validation; write failure classification and lookup config; JCS for
idempotency values; `title` and a fixed `format` list in the strict profile; pointer and
cascade conventions; new codes `DEF_VAR_UNKNOWN`, `DEF_VAR_DEFAULT`, `DEF_DURATION_INVALID`,
`DEF_SIGNAL_TARGET`; `DEF_SPAWN_KEY` removed (the schema now requires `key`).
