import type { IssueList } from "../issues.js";
import { strictViolations } from "../schema.js";
import type { Ajv } from "../spec.js";
import type { JsonSchema } from "../types.js";

/**
 * Check one user-authored schema: valid JSON Schema 2020-12 (…_SCHEMA_INVALID), then the
 * strict profile (…_SCHEMA_STRICT).
 */
export function checkUserSchema(
  ajv: Ajv,
  schema: JsonSchema,
  file: string,
  at: string,
  prefix: "CON" | "DEF",
  issues: IssueList,
): void {
  if (!(ajv.validateSchema(schema) as boolean)) {
    // One issue per failing location, pointing at the offending keyword inside the schema.
    const seen = new Set<string>();
    for (const error of ajv.errors ?? []) {
      if (!seen.has(error.instancePath)) {
        seen.add(error.instancePath);
        issues.add(`${prefix}_SCHEMA_INVALID`, file, at + error.instancePath, `not a valid JSON Schema: ${error.message ?? error.keyword}`);
      }
    }
    return;
  }
  try {
    ajv.compile(schema);
  } catch (error) {
    issues.add(`${prefix}_SCHEMA_INVALID`, file, at, `not a valid JSON Schema: ${(error as Error).message}`);
    return;
  }
  for (const violation of strictViolations(schema)) {
    issues.add(`${prefix}_SCHEMA_STRICT`, file, at + violation.pointer, violation.message);
  }
}
