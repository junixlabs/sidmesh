# SidMesh conformance fixtures

Shared test data for every SidMesh implementation. When the Go and TypeScript
implementations disagree, these files decide. They follow `../SPEC.md`; if a fixture and
the spec disagree, the spec wins and the fixture is a bug.

## projects/

One directory per case. Each directory is a complete, minimal SidMesh project
(`sidmesh.json`, `nodes/`, `definitions/`) plus `expected.json`:

```json
{ "valid": false, "errors": [ { "code": "DEF_OUTCOME_UNROUTED", "file": "definitions/main.v1.json", "pointer": "/steps/charge/on" } ] }
```

- `valid` — whether the project must pass validation.
- `errors` — every error the validator must report, no more and no fewer.
- Matching is on the set of `(code, file)`. `file` is relative to the case directory.
  `pointer` (a JSON Pointer into that file) is compared only when the fixture gives it.

Cases named `valid-*` are baselines. Every other case is `valid-basic` with one change,
so each isolates one rule. A few rules cannot be broken alone; those cases list every
error the single change causes (see "Cases with more than one error").

### Settled by the spec (v0.1)

Pointers follow SPEC §3 "Pointer conventions" and cascades follow its suppression rule; the
fixtures were updated to match (`def-capability-unknown-compensate` now points at the step;
`def-step-unknown-start` checks that an unknown `start` reports nothing else). Idempotency
values use RFC 8785, as SPEC §4 now says. `DEF_SPAWN_KEY` no longer exists: a spawn without
`key` fails the schema, so that case became `prj-shape-spawn-key` (`PRJ_SHAPE_INVALID`).

### Readings the fixtures still rely on

Choices where SPEC.md leaves room. A validator must follow them to pass.

1. **Duplicates** are reported once, on the file that comes later in lexicographic path
   order (`PRJ_DUPLICATE_CAPABILITY`, `PRJ_DUPLICATE_DEFINITION`), pointer `/capability`
   or `/id`.
2. **Node id** for `CON_CAPABILITY_PREFIX` is the `id` in `node.json`, not the directory
   name. A directory/id mismatch reports `PRJ_NODE_DIR_MISMATCH` only.
3. **Lookup rules** (`CON_LOOKUP_*`) are reported on the write contract at `/lookup`.
   `CON_LOOKUP_UNKNOWN` stops the other lookup checks for that contract.
4. **"Is a lookup"** for `CON_RESERVED_OUTCOME` means some write contract names it in
   `lookup`, even if that reference fails another rule.
5. **Undeclared references report their own rule only:** `DEF_PARAM_UNKNOWN`,
   `DEF_SIGNAL_UNKNOWN`, `DEF_VAR_UNKNOWN`, never also `DEF_BINDING_PATH`.
6. **Dominance** is computed from a virtual root with edges to `start` and every signal
   `goto`. A step does not dominate itself, except for its own `compensate.input` and
   `compensate.config`.
7. **Shape failures** (`PRJ_SHAPE_INVALID`) are reported for the document; the fixtures
   only use shape violations that break no semantic rule.
8. **Signal `start`** with an unknown definition reports `DEF_SPAWN_UNKNOWN` at the step,
   like a spawn.
9. **`set` keys** that are not declared vars point at the value: `/steps/<id>/values/<key>`.
10. **`DEF_DRAIN_UNKNOWN`** points at the offending array item: `/steps/<id>/drain/<i>`.
11. **A definition that fails its shape still counts as existing** for references from other
    definitions (the TS validator reports `DEF_SPAWN_UNKNOWN` on a signal `start` to it).
    No fixture depends on this: `prj-shape-collect-max` breaks an unreferenced copy of
    `audit` so the case keeps one error.

### v0.2 cases

`valid-basic` ends `audit` with `drain: ["charged"]`. `valid-collect-max-binding` gives
`collect.max` as `params.max`; `def-param-unknown-collect-max` uses the same binding without
the param; `prj-shape-collect-max` sets `max: 1` (the schema minimum is 2);
`def-drain-unknown` drains a signal the definition does not declare. `valid-signal-start-once` sets `start.once: true` on the
signal-with-start to `audit`.

### v0.3 cases

A compensation may bind `config`, checked against the compensating contract's `config` like a
call's. `valid-compensate-config` gives `shop.refund` a required `config.reason` and binds it
from `steps.charge.output.charge_id` (the self-read exception); `def-input-missing-compensate-config`
adds the same config to the contract without binding it (pointer `/steps/charge/compensate/config`);
`def-input-extra-compensate-config` binds `config.region` on a contract that declares no config
(pointer `/steps/charge/compensate/config/region`).

`sidmesh.json` may declare `spec`. `valid-spec-declared` sets `"spec": "0.3"`;
`prj-spec-unsupported` sets `"0.2"` (`PRJ_SPEC_UNSUPPORTED`, pointer `/spec`);
`prj-spec-format` sets `"v0.3"` (`PRJ_MANIFEST_INVALID`, pointer `/spec`). The unsupported
case assumes a toolchain implementing 0.3.

### Cases with more than one error

| Case | Why |
| --- | --- |
| `prj-duplicate-capability` | A second file can only claim an existing capability@version under another file name, so `CON_FILENAME` comes with it |
| `con-lookup-effects` | A lookup with `effects: write` is itself a write without idempotency or lookup |

## payloads.json

Contract-wrapper vectors for the runtime check in SPEC §4.

- `contracts` — named contracts the cases refer to.
- `cases[]` — `{name, contract, input, config?, result: {outcome, output}, expect}`.
- `expect` is `ok`, `INPUT_INVALID` (input or config fails its schema; the handler never
  runs, so `result` is irrelevant) or `OUTPUT_INVALID` (undeclared outcome, a reserved
  outcome, or output that fails the outcome's schema).
- `config` absent means the caller bound no config; validate it as `{}`.
- String lengths count Unicode code points, not UTF-16 units or bytes.
- `format` (`date-time`, `date`, `email`, `uri`, `uuid`) is asserted, per SPEC §2.

## idempotency.json

Vectors for the idempotency key in SPEC §4 (12, including raw U+2028/U+2029 and unescaped `<>&`): an array of
`{note, tenant, capability, version, values, encoded, key}`.

- `values` — the values of the declared key paths, in declaration order.
- `encoded` — each value as the implementation must encode it before hashing: RFC 8785
  JSON Canonicalization (object keys sorted, no whitespace, no HTML escaping, numbers as
  ECMAScript prints them).
- `key` — lowercase hex SHA-256 of `tenant`, `capability@version` and each encoded value,
  joined with `\n`, UTF-8.

Keys were computed with Node.js `crypto` and the first was re-checked with
`shasum -a 256`.

## codec.json

Vectors for the payload codec in SPEC §7: an array of
`{note, keys, nonce?, payload?, protobuf?, encrypted, expect}`. Every byte field is
standard base64, including metadata values (Temporal metadata values are bytes).

- `keys` — the value of `SIDMESH_CODEC_KEYS`.
- `expect: "ok"` — decoding `encrypted` with `keys` must give back `payload` exactly.
  Vectors that also carry `nonce` and `protobuf` are encode vectors: encoding `payload` with
  the first key and that nonce must give `encrypted` byte for byte.
- `expect: "error"` — decoding must fail (wrong key bytes, key id not listed, altered tag).
- `protobuf` — the plaintext before encryption: the `temporal.api.common.v1.Payload`
  serialised with deterministic map order (keys sorted; Go `proto.MarshalOptions{Deterministic: true}`).
  Proto3 omits `data` when it is empty. Decoders must accept any map order; only encoding
  to the exact bytes needs the sorted order.
- `encrypted.data` is `nonce (12) || ciphertext || tag (16)`.

How they were built: `gen_codec.mjs` with Node.js `crypto` (AES-256-GCM) and a
hand-written Payload encoder (field 1 = metadata map entries `{1: key, 2: value}`, field 2 = data).
A Go program then decrypted every vector with `crypto/cipher`, unmarshalled the plaintext with
the real `go.temporal.io/api` v1.63.7 `commonpb.Payload`, compared it with `payload`, re-marshalled it
deterministically, and compared that with `protobuf`. It also confirmed that every `error` vector fails.
Keys: `k1` = bytes 0x00..0x1f, `k2` = 0x20..0x3f, the wrong key = 32 × 0xaa.

## vectors/document-hash.json

Vectors for version pinning (SPEC §5): an array of `{note, text, jcs, hash}`.

- `text` — the document exactly as a file holds it. Parse it, then serialise it with RFC 8785
  (JCS); the result must equal `jcs`, and `hash` is `sha256:` + lowercase hex SHA-256 of
  `jcs` in UTF-8.
- The document is hashed whole, `$schema` included. Whitespace and key order do not change
  the hash; any character of content does.
- Cases cover key order, keys whose UTF-16 order differs from code point order, numbers
  written as `1.0`, `0.850` or `1e2`, escapes, raw U+2028, and a contract as a project writes it.

Built with Node.js and re-checked with an independent Python JCS (UTF-16 key sort,
ECMAScript number formatting): every `jcs` and `hash` matched.
