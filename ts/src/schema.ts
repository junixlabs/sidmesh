import { pointer } from "./issues.js";
import type { JsonSchema } from "./types.js";

const SCALAR_TYPES = new Set(["object", "array", "string", "integer", "number", "boolean"]);

const ALLOWED_KEYWORDS = new Set([
  "type",
  "properties",
  "additionalProperties",
  "required",
  "items",
  "title",
  "description",
  "enum",
  "const",
  "minimum",
  "maximum",
  "minLength",
  "maxLength",
  "pattern",
  "format",
  "minItems",
  "maxItems",
  "default",
  "$defs",
  "$ref",
]);

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** The base type of a schema node, unwrapping the [T, "null"] nullable form. */
export function baseType(schema: JsonSchema): string | undefined {
  const type = schema.type;
  if (typeof type === "string") {
    return type;
  }
  if (Array.isArray(type)) {
    const nonNull = type.filter((t) => t !== "null");
    return nonNull.length === 1 && typeof nonNull[0] === "string" ? nonNull[0] : undefined;
  }
  return undefined;
}

export function isNullable(schema: JsonSchema): boolean {
  return Array.isArray(schema.type) && schema.type.includes("null");
}

/** Follow a local "#/$defs/<name>" reference against the document root. */
export function deref(schema: JsonSchema, root: JsonSchema): JsonSchema | undefined {
  let current: JsonSchema | undefined = schema;
  for (let hops = 0; current && typeof current.$ref === "string" && hops < 32; hops++) {
    const match = /^#\/\$defs\/([^/]+)$/.exec(current.$ref);
    const defs = root.$defs;
    if (!match || !isRecord(defs)) {
      return undefined;
    }
    const next = defs[match[1] as string];
    current = isRecord(next) ? next : undefined;
  }
  return current;
}

/** The only format values allowed (SPEC §2); both runtimes assert them. */
const ALLOWED_FORMATS = new Set(["date-time", "date", "email", "uri", "uuid"]);

export interface StrictViolation {
  /** JSON Pointer, relative to the schema root, of the schema node that breaks the profile. */
  pointer: string;
  message: string;
}

/**
 * Check a schema against the strict profile in SPEC §2. Returns every violation with a
 * pointer relative to the schema root.
 */
export function strictViolations(root: JsonSchema): StrictViolation[] {
  const found: StrictViolation[] = [];

  const visit = (node: unknown, at: string): void => {
    if (!isRecord(node)) {
      found.push({ pointer: at, message: "schema must be an object" });
      return;
    }
    for (const key of Object.keys(node)) {
      if (!ALLOWED_KEYWORDS.has(key)) {
        found.push({ pointer: at, message: `keyword "${key}" is not allowed` });
      }
    }

    if (node.$defs !== undefined) {
      if (!isRecord(node.$defs)) {
        found.push({ pointer: at, message: "$defs must be an object" });
      } else {
        for (const [name, def] of Object.entries(node.$defs)) {
          visit(def, at + pointer("$defs", name));
        }
      }
    }

    if (node.format !== undefined && (typeof node.format !== "string" || !ALLOWED_FORMATS.has(node.format))) {
      found.push({ pointer: at, message: `format must be one of ${[...ALLOWED_FORMATS].join(", ")}` });
    }

    if (node.$ref !== undefined) {
      if (typeof node.$ref !== "string" || !/^#\/\$defs\/[^/]+$/.test(node.$ref)) {
        found.push({ pointer: at, message: "$ref must point into this document's $defs" });
      } else if (!deref(node, root)) {
        found.push({ pointer: at, message: `$ref ${node.$ref} does not resolve` });
      }
      return;
    }

    const type = node.type;
    const validType =
      (typeof type === "string" && SCALAR_TYPES.has(type)) ||
      (Array.isArray(type) &&
        type.length === 2 &&
        type.includes("null") &&
        type.some((t) => typeof t === "string" && SCALAR_TYPES.has(t)));
    if (!validType) {
      found.push({
        pointer: at,
        message: 'type must be one of object, array, string, integer, number, boolean, or [T, "null"]',
      });
      return;
    }

    const base = baseType(node);
    if (base === "object") {
      if (!isRecord(node.properties)) {
        found.push({ pointer: at, message: "object must declare properties" });
      }
      if (node.additionalProperties !== false) {
        found.push({ pointer: at, message: "object must set additionalProperties: false" });
      }
      const props = isRecord(node.properties) ? node.properties : {};
      if (node.required !== undefined) {
        if (!Array.isArray(node.required)) {
          found.push({ pointer: at, message: "required must be an array" });
        } else {
          node.required.forEach((name, index) => {
            if (typeof name !== "string" || !(name in props)) {
              found.push({ pointer: at, message: `required lists undeclared property ${String(name)}` });
            }
          });
        }
      }
      for (const [name, prop] of Object.entries(props)) {
        visit(prop, at + pointer("properties", name));
      }
    } else if (node.properties !== undefined || node.required !== undefined) {
      found.push({ pointer: at, message: "properties/required only allowed on objects" });
    }

    if (base === "array") {
      if (node.items === undefined) {
        found.push({ pointer: at, message: "array must declare items" });
      } else {
        visit(node.items, `${at}/items`);
      }
    } else if (node.items !== undefined) {
      found.push({ pointer: at, message: "items only allowed on arrays" });
    }
  };

  visit(root, "");
  return found;
}

/**
 * Resolve a property path inside a schema. Returns the sub-schema, or undefined when the
 * path does not exist. A schema with no type, properties or $ref (for example {}) accepts
 * anything, so any deeper path resolves to it.
 */
export function resolvePath(
  schema: JsonSchema,
  segments: string[],
  root: JsonSchema = schema,
  opts: { requireRequired?: boolean } = {},
): JsonSchema | undefined {
  let current: JsonSchema | undefined = deref(schema, root);
  for (const segment of segments) {
    if (!current) {
      return undefined;
    }
    if (current.type === undefined && current.properties === undefined) {
      return current; // open schema: anything goes below
    }
    if (baseType(current) !== "object" || !isRecord(current.properties)) {
      return undefined;
    }
    const next = current.properties[segment];
    if (!isRecord(next)) {
      return undefined;
    }
    if (opts.requireRequired) {
      const required = Array.isArray(current.required) ? current.required : [];
      if (!required.includes(segment)) {
        return undefined;
      }
    }
    current = deref(next, root);
  }
  return current;
}

/** JSON equality with object key order ignored. */
export function jsonEqual(a: unknown, b: unknown): boolean {
  return canonicalJson(a) === canonicalJson(b);
}

/**
 * RFC 8785 (JCS) serialisation for JSON values: object keys sorted by UTF-16 code units,
 * no whitespace, strings and numbers as ECMAScript JSON.stringify prints them.
 */
export function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) {
    return `[${value.map(canonicalJson).join(",")}]`;
  }
  if (isRecord(value)) {
    const keys = Object.keys(value).sort();
    return `{${keys.map((k) => `${JSON.stringify(k)}:${canonicalJson(value[k])}`).join(",")}}`;
  }
  return JSON.stringify(value ?? null);
}
