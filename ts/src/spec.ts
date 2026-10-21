import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import Ajv2020Module from "ajv/dist/2020.js";
import addFormatsModule from "ajv-formats";
import type { ErrorObject, ValidateFunction } from "ajv";

// ajv and ajv-formats are CommonJS; under NodeNext the default import is module.exports.
type AjvClass = typeof Ajv2020Module.default;
export type Ajv = InstanceType<AjvClass>;
const Ajv2020 = ((Ajv2020Module as unknown as { default?: AjvClass }).default ??
  Ajv2020Module) as AjvClass;
const addFormats = ((addFormatsModule as unknown as { default?: unknown }).default ??
  addFormatsModule) as unknown as (ajv: Ajv) => Ajv;

/**
 * The spec directory is the single source of truth for the meta-schemas. `pnpm build` copies
 * the schemas into dist/spec so a built package needs nothing beside it; from source it is
 * sidmesh/spec beside sidmesh/ts. SIDMESH_SPEC_DIR overrides both.
 */
export function specDir(): string {
  if (process.env.SIDMESH_SPEC_DIR) {
    return process.env.SIDMESH_SPEC_DIR;
  }
  const here = path.dirname(fileURLToPath(import.meta.url));
  const bundled = path.join(here, "spec");
  return existsSync(path.join(bundled, "definition.schema.json")) ? bundled : path.resolve(here, "../../spec");
}

export interface MetaValidators {
  node: ValidateFunction;
  contract: ValidateFunction;
  definition: ValidateFunction;
}

let cached: MetaValidators | undefined;

export function metaValidators(): MetaValidators {
  if (cached) {
    return cached;
  }
  const ajv = new Ajv2020({ strict: true, allErrors: true });
  const load = (name: string) =>
    JSON.parse(readFileSync(path.join(specDir(), `${name}.schema.json`), "utf8")) as { $id: string };
  // contract first: definition.schema.json references its $defs by relative $id.
  const docs = { contract: load("contract"), node: load("node"), definition: load("definition") };
  for (const doc of Object.values(docs)) {
    ajv.addSchema(doc);
  }
  // Look schemas up by their own $id, so a spec version bump needs no code change here.
  const get = (name: keyof typeof docs) => {
    const validate = ajv.getSchema(docs[name].$id);
    if (!validate) {
      throw new Error(`spec schema ${name} failed to load from ${specDir()}`);
    }
    return validate;
  };
  cached = { node: get("node"), contract: get("contract"), definition: get("definition") };
  return cached;
}

/** A fresh Ajv for user-authored schemas (contract payloads, params, signals). */
export function payloadAjv(): Ajv {
  const ajv = new Ajv2020({ strict: false, allErrors: true });
  addFormats(ajv);
  return ajv;
}

export function describeErrors(errors: ErrorObject[] | null | undefined): string {
  if (!errors || errors.length === 0) {
    return "invalid";
  }
  return errors
    .slice(0, 5)
    .map((e) => `${e.instancePath || "/"} ${e.message ?? e.keyword}`)
    .join("; ");
}
