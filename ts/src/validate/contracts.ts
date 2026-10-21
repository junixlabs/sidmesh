import path from "node:path";
import { capabilityKey, nodeOf, verbOf } from "../capability.js";
import { pointer, type IssueList } from "../issues.js";
import { jsonEqual, resolvePath } from "../schema.js";
import type { Contract, JsonSchema } from "../types.js";
import type { Catalog, CatalogContract } from "./catalog.js";
import { checkUserSchema } from "./schemas.js";

const RESERVED = ["failed", "unknown"];

/** Capabilities named as some write capability's lookup. */
function lookupTargets(catalog: Catalog): Set<string> {
  const targets = new Set<string>();
  for (const { contract } of catalog.contracts.values()) {
    if (contract.effects === "write" && contract.lookup) {
      targets.add(capabilityKey(contract.lookup.capability, contract.lookup.version));
    }
  }
  return targets;
}

function checkSchemas(entry: CatalogContract, catalog: Catalog, issues: IssueList): void {
  const { contract, file } = entry;
  if (contract.config) {
    checkUserSchema(catalog.ajv, contract.config, file, "/config", "CON", issues);
  }
  checkUserSchema(catalog.ajv, contract.input, file, "/input", "CON", issues);
  for (const [name, outcome] of Object.entries(contract.outcomes)) {
    checkUserSchema(catalog.ajv, outcome.output, file, pointer("outcomes", name, "output"), "CON", issues);
  }
}

function checkWrite(entry: CatalogContract, catalog: Catalog, issues: IssueList): void {
  const { contract, file } = entry;
  if (contract.effects !== "write") {
    return;
  }
  if (!contract.idempotency) {
    issues.add("CON_WRITE_IDEMPOTENCY", file, "", "a write capability must declare idempotency");
  } else {
    contract.idempotency.key.forEach((keyPath, index) => {
      const segments = keyPath.split(".").slice(1);
      if (!resolvePath(contract.input, segments, contract.input, { requireRequired: true })) {
        issues.add(
          "CON_IDEMPOTENCY_PATH",
          file,
          pointer("idempotency", "key", index),
          `${keyPath} is not a required property of input`,
        );
      }
    });
  }

  if (!contract.lookup) {
    issues.add("CON_WRITE_LOOKUP", file, "", "a write capability must declare lookup");
    return;
  }
  const ref = contract.lookup;
  const target = catalog.contracts.get(capabilityKey(ref.capability, ref.version));
  const sameNode = nodeOf(ref.capability) === nodeOf(contract.capability);
  if (!target && sameNode && catalog.shapeInvalid.contracts.has(capabilityKey(ref.capability, ref.version))) {
    return; // the lookup exists but failed its shape; nothing more to check here
  }
  if (!target || !sameNode) {
    issues.add(
      "CON_LOOKUP_UNKNOWN",
      file,
      "/lookup",
      `${capabilityKey(ref.capability, ref.version)} is not a capability of this node`,
    );
    return;
  }
  const lookup = target.contract;
  if (lookup.effects === "write") {
    issues.add("CON_LOOKUP_EFFECTS", file, "/lookup", "the lookup capability must not write");
  }
  if (!jsonEqual(lookup.input, contract.input)) {
    issues.add("CON_LOOKUP_INPUT", file, "/lookup", "the lookup input schema must equal this input schema");
  }
  const expected = [...Object.keys(contract.outcomes), "not_found"].sort();
  const actual = Object.keys(lookup.outcomes).sort();
  const sameNames = jsonEqual(expected, actual);
  const sameOutputs =
    sameNames &&
    Object.entries(contract.outcomes).every(([name, outcome]) =>
      jsonEqual(outcome.output, lookup.outcomes[name]?.output),
    );
  if (!sameOutputs) {
    issues.add(
      "CON_LOOKUP_OUTCOMES",
      file,
      "/lookup",
      "the lookup must declare exactly this capability's outcomes plus not_found, with identical outputs",
    );
  }
}

function isConfidence(schema: JsonSchema): boolean {
  const props = schema.properties as Record<string, JsonSchema> | undefined;
  const confidence = props?.confidence;
  const required = Array.isArray(schema.required) ? schema.required : [];
  return (
    confidence !== undefined &&
    confidence.type === "number" &&
    confidence.minimum === 0 &&
    confidence.maximum === 1 &&
    required.includes("confidence")
  );
}

function checkAi(entry: CatalogContract, issues: IssueList): void {
  const { contract, file } = entry;
  if (contract.kind !== "ai") {
    return;
  }
  if (contract.effects === "write") {
    issues.add("CON_AI_WRITE", file, "/effects", "an ai capability must not write");
  }
  for (const [name, outcome] of Object.entries(contract.outcomes)) {
    if (!isConfidence(outcome.output)) {
      issues.add(
        "CON_AI_CONFIDENCE",
        file,
        pointer("outcomes", name, "output"),
        "ai outputs need a required confidence: number with minimum 0 and maximum 1",
      );
    }
  }
}

function checkLedger(contract: Contract, file: string, issues: IssueList): void {
  (contract.ledger ?? []).forEach((ledgerPath, index) => {
    const [root, ...segments] = ledgerPath.split(".");
    let resolves = false;
    if (root === "input") {
      resolves = resolvePath(contract.input, segments) !== undefined;
    } else if (root === "config") {
      resolves = contract.config !== undefined && resolvePath(contract.config, segments) !== undefined;
    } else if (root === "output") {
      resolves = Object.values(contract.outcomes).some(
        (outcome) => resolvePath(outcome.output, segments) !== undefined,
      );
    }
    if (!resolves) {
      issues.add("CON_LEDGER_PATH", file, pointer("ledger", index), `${ledgerPath} does not resolve`);
    }
  });
}

export function validateContracts(catalog: Catalog, issues: IssueList): void {
  const lookups = lookupTargets(catalog);
  for (const entry of [...catalog.contracts.values(), ...catalog.duplicateContracts]) {
    const { contract, file } = entry;
    const expectedName = `${verbOf(contract.capability)}.v${contract.version}.json`;
    if (path.posix.basename(file) !== expectedName) {
      issues.add("CON_FILENAME", file, "", `file must be named ${expectedName}`);
    }
    if (nodeOf(contract.capability) !== entry.nodeId) {
      issues.add(
        "CON_CAPABILITY_PREFIX",
        file,
        "/capability",
        `capability must start with "${entry.nodeId}."`,
      );
    }
    for (const name of Object.keys(contract.outcomes)) {
      const isLookup = lookups.has(capabilityKey(contract.capability, contract.version));
      if (RESERVED.includes(name) || (name === "not_found" && !isLookup)) {
        issues.add("CON_RESERVED_OUTCOME", file, pointer("outcomes", name), `outcome "${name}" is reserved`);
      }
    }
    checkSchemas(entry, catalog, issues);
    checkWrite(entry, catalog, issues);
    checkAi(entry, issues);
    checkLedger(contract, file, issues);
  }
}

