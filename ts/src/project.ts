import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { IssueList } from "./issues.js";
import type { Issue } from "./issues.js";
import { isRecord } from "./schema.js";

export interface ProjectConfig {
  name: string;
  queues: { workflow: string; go: string; ts: string };
}

export interface LoadedDoc {
  /** Path relative to the project root, forward slashes. */
  file: string;
  /** Parsed JSON, or undefined when the file did not parse. */
  doc: unknown;
}

export interface NodeEntry extends LoadedDoc {
  /** Directory name under nodes/. */
  dir: string;
}

export interface ContractEntry extends LoadedDoc {
  /** Directory name of the owning node. */
  nodeDir: string;
}

export interface Project {
  root: string;
  config: ProjectConfig | undefined;
  nodes: NodeEntry[];
  contracts: ContractEntry[];
  definitions: LoadedDoc[];
  /** Problems found while reading files (missing manifest, unparsable JSON). */
  loadIssues: Issue[];
}

const NAME_PATTERN = /^[a-z][a-z0-9-]*$/;
const SPEC_PATTERN = /^[0-9]+\.[0-9]+$/;

/** The SidMesh spec version this toolchain implements (SPEC.md), as MAJOR.MINOR. */
export const SPEC_VERSION = "0.3";

/**
 * Whether a project declaring spec `declared` can run on a toolchain implementing
 * `implemented` (both MAJOR.MINOR). Before 1.0 they must be equal; from 1.0 the major must
 * match and the declared minor must not be newer.
 */
export function specSupported(declared: string, implemented: string = SPEC_VERSION): boolean {
  const [dMajor = 0, dMinor = 0] = declared.split(".").map(Number);
  const [iMajor = 0, iMinor = 0] = implemented.split(".").map(Number);
  if (iMajor === 0) {
    return dMajor === 0 && dMinor === iMinor;
  }
  return dMajor === iMajor && dMinor <= iMinor;
}

function listDirs(dir: string): string[] {
  if (!existsSync(dir)) {
    return [];
  }
  return readdirSync(dir)
    .filter((name) => !name.startsWith(".") && statSync(path.join(dir, name)).isDirectory())
    .sort();
}

function listJson(dir: string): string[] {
  if (!existsSync(dir)) {
    return [];
  }
  return readdirSync(dir)
    .filter((name) => name.endsWith(".json"))
    .sort();
}

function readJson(root: string, file: string, issues: IssueList, code = "PRJ_SHAPE_INVALID"): unknown {
  try {
    return JSON.parse(readFileSync(path.join(root, file), "utf8"));
  } catch (error) {
    issues.add(code, file, "", `not valid JSON: ${(error as Error).message}`);
    return undefined;
  }
}

function readConfig(root: string, issues: IssueList): ProjectConfig | undefined {
  const file = "sidmesh.json";
  if (!existsSync(path.join(root, file))) {
    issues.add("PRJ_MANIFEST_INVALID", file, "", "sidmesh.json not found");
    return undefined;
  }
  const raw = readJson(root, file, issues, "PRJ_MANIFEST_INVALID");
  if (raw === undefined) {
    return undefined;
  }
  if (!isRecord(raw)) {
    issues.add("PRJ_MANIFEST_INVALID", file, "", "must be an object");
    return undefined;
  }
  const spec = raw.spec;
  if (spec !== undefined) {
    if (typeof spec !== "string" || !SPEC_PATTERN.test(spec)) {
      issues.add("PRJ_MANIFEST_INVALID", file, "/spec", `spec must be a string MAJOR.MINOR, e.g. "${SPEC_VERSION}"`);
      return undefined;
    }
    if (!specSupported(spec)) {
      issues.add(
        "PRJ_SPEC_UNSUPPORTED",
        file,
        "/spec",
        `project declares spec ${spec}; this toolchain implements ${SPEC_VERSION}, see the upgrade notes in CHANGELOG.md`,
      );
      return undefined;
    }
  }
  const name = raw.name;
  if (typeof name !== "string" || !NAME_PATTERN.test(name)) {
    issues.add("PRJ_MANIFEST_INVALID", file, "/name", "name must match ^[a-z][a-z0-9-]*$");
    return undefined;
  }
  const queues = raw.queues ?? {};
  if (!isRecord(queues)) {
    issues.add("PRJ_MANIFEST_INVALID", file, "/queues", "queues must be an object");
    return undefined;
  }
  const queue = (key: "workflow" | "go" | "ts", fallback: string): string | undefined => {
    const value = queues[key];
    if (value === undefined) {
      return fallback;
    }
    if (typeof value !== "string" || value.length === 0) {
      issues.add("PRJ_MANIFEST_INVALID", file, `/queues/${key}`, "queue name must be a string");
      return undefined;
    }
    return value;
  };
  const workflow = queue("workflow", `${name}-workflows`);
  const go = queue("go", `${name}-go`);
  const ts = queue("ts", `${name}-ts`);
  if (!workflow || !go || !ts) {
    return undefined;
  }
  return { name, queues: { workflow, go, ts } };
}

/** Read a project by convention: sidmesh.json, nodes/<id>/..., definitions/*.json. */
export function loadProject(rootDir: string): Project {
  const root = path.resolve(rootDir);
  const issues = new IssueList();
  const config = readConfig(root, issues);

  const nodes: NodeEntry[] = [];
  const contracts: ContractEntry[] = [];
  for (const dir of listDirs(path.join(root, "nodes"))) {
    const nodeFile = `nodes/${dir}/node.json`;
    if (!existsSync(path.join(root, nodeFile))) {
      issues.add("PRJ_SHAPE_INVALID", nodeFile, "", "node directory has no node.json");
      continue;
    }
    nodes.push({ file: nodeFile, doc: readJson(root, nodeFile, issues), dir });
    for (const name of listJson(path.join(root, "nodes", dir, "contracts"))) {
      const file = `nodes/${dir}/contracts/${name}`;
      contracts.push({ file, doc: readJson(root, file, issues), nodeDir: dir });
    }
  }

  const definitions: LoadedDoc[] = listJson(path.join(root, "definitions")).map((name) => {
    const file = `definitions/${name}`;
    return { file, doc: readJson(root, file, issues) };
  });

  return { root, config, nodes, contracts, definitions, loadIssues: issues.items };
}
