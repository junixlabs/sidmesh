import { createHash } from "node:crypto";
import { capabilityKey } from "../capability.js";
import { canonicalJson } from "../schema.js";
import { describeErrors, payloadAjv, type Ajv } from "../spec.js";
import { callsOf, startsOf } from "../steps.js";
import type { CapabilityRef, Definition } from "../types.js";
import type { Catalog } from "../validate/catalog.js";

// Mirrors the Go engine's Prepare: the start input pins, by content hash, the definition, every
// definition it can start, every contract they call (including lookup and compensate), and the
// node owning each of those contracts (SPEC §5).

export const WORKFLOW_NAME = "sidmesh.interpret";
export const HUMAN_UPDATE = "human";

export interface StartRequest {
  definition: string;
  version: number;
  workflowId: string;
  tenant: string;
  input?: Record<string, unknown>;
  params?: Record<string, unknown>;
}

/** The interpreter workflow's input. `definition` is the pin key of the root definition. */
export interface StartInput {
  tenant: string;
  definition: string;
  pins: Record<string, string>;
  queues: { workflow: string; go: string; ts: string };
  input: Record<string, unknown>;
  params?: Record<string, unknown>;
}

/** A document as the registry stores it (`sidmesh_documents`, SPEC §6.1). */
export interface PinnedDocument {
  hash: string;
  kind: "definition" | "contract" | "node";
  name: string;
  version: number;
  doc: unknown;
}

/** `sha256:<hex>` of the document's RFC 8785 form. */
export function documentHash(doc: unknown): string {
  return "sha256:" + createHash("sha256").update(canonicalJson(doc)).digest("hex");
}

export const pinKey = (kind: PinnedDocument["kind"], name: string, version: number) =>
  `${kind}:${capabilityKey(name, version)}`;
const nodePinKey = (id: string) => `node:${id}`;

interface CatalogDocuments {
  /** In publish order: definitions, contracts, nodes. */
  list: PinnedDocument[];
  byPin: Map<string, PinnedDocument>;
}

// Each catalog's documents, hashed on first use (publish at connect, or the first start) and
// reused by every start after. Catalogs from withDefinitions share their base's entry.
const catalogDocuments = new WeakMap<Catalog, CatalogDocuments>();

function documentsOf(catalog: Catalog): CatalogDocuments {
  let docs = catalogDocuments.get(catalog);
  if (!docs) {
    const defs = [...catalog.definitions.values()].map(({ definition: d }) => doc("definition", d.id, d.version, d));
    const contracts = [...catalog.contracts.values()].map(({ contract: c }) => doc("contract", c.capability, c.version, c));
    // A node has no version; the registry's version column holds 0 for it.
    const nodes = [...catalog.nodes.values()].flatMap(({ manifest: n }) => (n ? [doc("node", n.id, 0, n)] : []));
    const list = [...defs, ...contracts, ...nodes];
    const byPin = new Map<string, PinnedDocument>();
    for (const d of list) {
      byPin.set(d.kind === "node" ? nodePinKey(d.name) : pinKey(d.kind, d.name, d.version), d);
    }
    docs = { list, byPin };
    catalogDocuments.set(catalog, docs);
  }
  return docs;
}

/** The catalog's own document under `key` when it is exactly `body`; undefined for an ad-hoc body. */
function catalogDocument(catalog: Catalog, key: string, body: object): PinnedDocument | undefined {
  const known = documentsOf(catalog).byPin.get(key);
  return known?.doc === body ? known : undefined;
}

/** Hash of the document pinned under `key`: the catalog's own when it is that body, else hashed now (ad hoc). */
function hashOf(catalog: Catalog, key: string, body: object): string {
  return catalogDocument(catalog, key, body)?.hash ?? documentHash(body);
}

/** Every definition and contract of the project, as published to the registry at startup. */
export function projectDocuments(catalog: Catalog): PinnedDocument[] {
  return [...documentsOf(catalog).list];
}

/**
 * The catalog with `adHoc` definitions added (replacing any with the same id@version). It reuses
 * the base catalog's document hashes; ad-hoc definitions are hashed per start.
 */
export function withDefinitions(catalog: Catalog, adHoc: Definition[]): Catalog {
  const definitions = new Map(catalog.definitions);
  for (const d of adHoc) {
    definitions.set(capabilityKey(d.id, d.version), { file: `<ad hoc ${d.id}>`, definition: d });
  }
  const extended = { ...catalog, definitions };
  catalogDocuments.set(extended, documentsOf(catalog));
  return extended;
}

function doc(kind: PinnedDocument["kind"], name: string, version: number, body: unknown): PinnedDocument {
  return { hash: documentHash(body), kind, name, version, doc: body };
}

/** Validate a start request against the project and build the pinned start input. */
export function prepareStart(catalog: Catalog, queues: StartInput["queues"], req: StartRequest): StartInput {
  const key = capabilityKey(req.definition, req.version);
  const def = catalog.definitions.get(key)?.definition;
  if (!def) {
    throw new Error(`definition ${key} not found`);
  }
  const input = req.input ?? {};
  // Ajv caches compiled schemas by object identity, and an ad-hoc definition is a new object on
  // every start: its schemas go to a throwaway Ajv so the catalog's one does not grow forever.
  const ajv = catalogDocument(catalog, pinKey("definition", def.id, def.version), def) ? catalog.ajv : payloadAjv();
  const check = ajv.compile(def.input);
  if (!check(input)) {
    throw new Error(`input: ${describeErrors(check.errors)}`);
  }
  const result = validateParams(def, req.params ?? {}, ajv);
  if (!result.ok) {
    throw new Error(result.errors.map((e) => `param "${e.param}" ${e.message}`).join("; "));
  }

  const pins: Record<string, string> = {};
  for (const d of collectDefinitions(catalog, def, new Map()).values()) {
    const key = pinKey("definition", d.id, d.version);
    pins[key] = hashOf(catalog, key, d);
    for (const ref of callsOf(d)) {
      try {
        pinContract(catalog, ref, pins);
      } catch (err) {
        throw new Error(`definition ${capabilityKey(d.id, d.version)}: ${(err as Error).message}`);
      }
    }
  }
  return {
    tenant: req.tenant,
    definition: pinKey("definition", def.id, def.version),
    pins,
    queues,
    input,
    ...(req.params !== undefined ? { params: req.params } : {}),
  };
}

export interface ParamsResult {
  ok: boolean;
  errors: { param: string; message: string }[];
}

/**
 * Check a partial params object (start overrides, or a tenant's stored params, SPEC §5) against
 * the definition's declared param schemas. An undeclared key is an error; omitted params are fine.
 * Without `ajv` the schemas compile on a throwaway instance, so callers passing new definition
 * objects do not grow a shared cache.
 */
export function validateParams(def: Definition, params: Record<string, unknown>, ajv?: Ajv): ParamsResult {
  const compiler = ajv ?? payloadAjv();
  const errors: ParamsResult["errors"] = [];
  for (const [name, value] of Object.entries(params)) {
    const param = def.params?.[name];
    if (!param) {
      errors.push({ param: name, message: `is not declared by ${capabilityKey(def.id, def.version)}` });
      continue;
    }
    const check = compiler.compile(param.schema);
    if (!check(value)) {
      errors.push({ param: name, message: describeErrors(check.errors) });
    }
  }
  return { ok: errors.length === 0, errors };
}

/** The definition and every definition it can start (spawn, signal start), by id@version. */
function collectDefinitions(catalog: Catalog, def: Definition, into: Map<string, Definition>): Map<string, Definition> {
  const key = capabilityKey(def.id, def.version);
  if (into.has(key)) {
    return into;
  }
  into.set(key, def);
  for (const target of startsOf(def)) {
    const child = catalog.definitions.get(capabilityKey(target.definition, target.version))?.definition;
    if (!child) {
      throw new Error(`definition ${key} step ${target.stepId} starts unknown ${capabilityKey(target.definition, target.version)}`);
    }
    collectDefinitions(catalog, child, into);
  }
  return into;
}

function pinContract(catalog: Catalog, ref: CapabilityRef, pins: Record<string, string>): void {
  const key = pinKey("contract", ref.capability, ref.version);
  if (key in pins) {
    return;
  }
  const entry = catalog.contracts.get(capabilityKey(ref.capability, ref.version));
  if (!entry) {
    throw new Error(`capability ${capabilityKey(ref.capability, ref.version)} not found`);
  }
  pins[key] = hashOf(catalog, key, entry.contract);
  const node = catalog.nodes.get(entry.nodeDir)?.manifest;
  if (node) {
    const nodeKey = nodePinKey(node.id);
    pins[nodeKey] = hashOf(catalog, nodeKey, node);
  }
  if (entry.contract.lookup) {
    pinContract(catalog, entry.contract.lookup, pins);
  }
}
