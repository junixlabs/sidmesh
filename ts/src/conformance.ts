import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { parseCodecKeys, payloadFromJson, payloadToJson, SidmeshCodec, type JsonPayload } from "./runtime/codec.js";
import { idempotencyKey } from "./runtime/idempotency.js";
import { documentHash } from "./runtime/prepare.js";
import { canonicalJson, jsonEqual } from "./schema.js";
import { validateDir } from "./validate/index.js";

interface ExpectedError {
  code: string;
  file: string;
  pointer?: string;
}

interface Expected {
  valid: boolean;
  errors: ExpectedError[];
}

export interface CaseResult {
  name: string;
  ok: boolean;
  /** Expected (code, file[, pointer]) not reported. */
  missing: string[];
  /** Reported (code, file) not expected. */
  unexpected: string[];
}

/** Case directories under <dir>/projects that carry an expected.json. */
export function conformanceCases(dir: string): string[] {
  const projects = path.join(dir, "projects");
  if (!existsSync(projects)) {
    return [];
  }
  return readdirSync(projects)
    .filter((name) => {
      const caseDir = path.join(projects, name);
      return statSync(caseDir).isDirectory() && existsSync(path.join(caseDir, "expected.json"));
    })
    .sort();
}

/**
 * Validate one fixture project and compare with its expected.json: the set of (code, file)
 * pairs must match exactly, and an expected pointer must also be reported when given.
 */
export function runCase(dir: string, name: string): CaseResult {
  const caseDir = path.join(dir, "projects", name);
  const expected = JSON.parse(readFileSync(path.join(caseDir, "expected.json"), "utf8")) as Expected;
  const issues = validateDir(caseDir).issues;

  const pair = (code: string, file: string) => `${code} ${file}`;
  const actualPairs = new Set(issues.map((i) => pair(i.code, i.file)));
  const expectedPairs = new Set(expected.errors.map((e) => pair(e.code, e.file)));

  const missing: string[] = [];
  for (const error of expected.errors) {
    const reported = issues.some(
      (i) =>
        i.code === error.code &&
        i.file === error.file &&
        (error.pointer === undefined || i.pointer === error.pointer),
    );
    if (!reported) {
      missing.push(error.pointer === undefined ? pair(error.code, error.file) : `${pair(error.code, error.file)}#${error.pointer}`);
    }
  }
  const unexpected = [...actualPairs].filter((p) => !expectedPairs.has(p));
  const validMatches = expected.valid === (issues.length === 0);
  return {
    name,
    ok: validMatches && missing.length === 0 && unexpected.length === 0,
    missing,
    unexpected,
  };
}

export function runConformance(dir: string): CaseResult[] {
  return conformanceCases(dir).map((name) => runCase(dir, name));
}

const vectorResult = (name: string, problem: string | undefined): CaseResult => ({
  name,
  ok: problem === undefined,
  missing: problem === undefined ? [] : [problem],
  unexpected: [],
});

function readVectors<T>(dir: string, file: string): T[] {
  const full = path.join(dir, file);
  return existsSync(full) ? (JSON.parse(readFileSync(full, "utf8")) as T[]) : [];
}

const sameJson = (p: Parameters<typeof payloadToJson>[0], want: unknown) => jsonEqual(payloadToJson(p), want);

/** Run the shared value vectors: idempotency keys, document hashes and the payload codec. */
export async function runVectors(dir: string): Promise<CaseResult[]> {
  const results: CaseResult[] = [];
  type Key = { note: string; tenant: string; capability: string; version: number; values: unknown[]; key: string };
  for (const v of readVectors<Key>(dir, "idempotency.json")) {
    const got = idempotencyKey(v.tenant, v.capability, v.version, v.values);
    results.push(vectorResult(`idempotency: ${v.note}`, got === v.key ? undefined : `got ${got}`));
  }
  type Hash = { note: string; text: string; jcs: string; hash: string };
  for (const v of readVectors<Hash>(dir, "vectors/document-hash.json")) {
    const doc = JSON.parse(v.text);
    const problem = canonicalJson(doc) !== v.jcs ? "JCS differs" : documentHash(doc) !== v.hash ? `got ${documentHash(doc)}` : undefined;
    results.push(vectorResult(`document-hash: ${v.note}`, problem));
  }
  type Codec = { note: string; keys: string; nonce?: string; payload?: JsonPayload; encrypted: JsonPayload; expect: "ok" | "error" };
  for (const v of readVectors<Codec>(dir, "codec.json")) {
    const keys = parseCodecKeys(v.keys);
    let problem: string | undefined;
    try {
      const [decoded] = await new SidmeshCodec(keys).decode([payloadFromJson(v.encrypted)]);
      if (v.expect === "error") {
        problem = "decoded, expected an error";
      } else if (!sameJson(decoded!, v.payload)) {
        problem = "decoded payload differs";
      } else if (v.nonce) {
        const nonce = Buffer.from(v.nonce, "base64");
        const [encoded] = await new SidmeshCodec(keys, () => nonce).encode([payloadFromJson(v.payload!)]);
        problem = sameJson(encoded!, v.encrypted) ? undefined : "encoded bytes differ";
      }
    } catch (err) {
      problem = v.expect === "error" ? undefined : (err as Error).message;
    }
    results.push(vectorResult(`codec: ${v.note}`, problem));
  }
  return results;
}
