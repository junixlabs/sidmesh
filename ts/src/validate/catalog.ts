import type { ValidateFunction } from "ajv";
import { capabilityKey } from "../capability.js";
import { IssueList } from "../issues.js";
import type { Project } from "../project.js";
import { metaValidators, payloadAjv, type Ajv } from "../spec.js";
import type { Contract, Definition, NodeManifest } from "../types.js";

export interface CatalogNode {
  dir: string;
  file: string;
  /** Present when node.json passed its schema. */
  manifest?: NodeManifest;
}

export interface CatalogContract {
  file: string;
  nodeDir: string;
  /** node.json id when valid, else the directory name. */
  nodeId: string;
  contract: Contract;
}

export interface CatalogDefinition {
  file: string;
  definition: Definition;
}

/** Everything that passed shape validation, indexed for semantic checks. */
export interface Catalog {
  nodes: Map<string, CatalogNode>;
  contracts: Map<string, CatalogContract>;
  definitions: Map<string, CatalogDefinition>;
  /** Contracts that repeat a capability@version: excluded from lookups, still checked as files. */
  duplicateContracts: CatalogContract[];
  /**
   * Keys of documents that failed their shape but name a readable id/capability and version
   * (SPEC §3 cascades): references to them are not *_UNKNOWN, and no rule runs inside them.
   */
  shapeInvalid: { contracts: Set<string>; definitions: Set<string> };
  ajv: Ajv;
}

function checkShape(
  validate: ValidateFunction,
  doc: unknown,
  file: string,
  issues: IssueList,
): boolean {
  if (validate(doc)) {
    return true;
  }
  const seen = new Set<string>();
  for (const error of validate.errors ?? []) {
    // if/then branches repeat the same location; one issue per location is enough.
    if (error.keyword === "if" || seen.has(error.instancePath)) {
      continue;
    }
    seen.add(error.instancePath);
    issues.add("PRJ_SHAPE_INVALID", file, error.instancePath, `${error.message ?? error.keyword}`);
  }
  return false;
}

/** `<name>@<version>` of a shape-invalid document, when both fields are readable. */
function readableKey(doc: unknown, field: "capability" | "id"): string | undefined {
  const d = doc as Record<string, unknown> | null;
  const name = d?.[field];
  const version = d?.version;
  return typeof name === "string" && Number.isInteger(version) ? capabilityKey(name, version as number) : undefined;
}

/** Shape-validate every document and project-level uniqueness rules. */
export function buildCatalog(project: Project, issues: IssueList): Catalog {
  const meta = metaValidators();
  const catalog: Catalog = {
    nodes: new Map(),
    contracts: new Map(),
    definitions: new Map(),
    duplicateContracts: [],
    shapeInvalid: { contracts: new Set(), definitions: new Set() },
    ajv: payloadAjv(),
  };

  for (const node of project.nodes) {
    const entry: CatalogNode = { dir: node.dir, file: node.file };
    if (node.doc !== undefined && checkShape(meta.node, node.doc, node.file, issues)) {
      entry.manifest = node.doc as NodeManifest;
      if (entry.manifest.id !== node.dir) {
        issues.add(
          "PRJ_NODE_DIR_MISMATCH",
          node.file,
          "/id",
          `node id "${entry.manifest.id}" differs from its directory "${node.dir}"`,
        );
      }
    }
    catalog.nodes.set(node.dir, entry);
  }

  for (const item of project.contracts) {
    if (item.doc === undefined) {
      continue;
    }
    if (!checkShape(meta.contract, item.doc, item.file, issues)) {
      const key = readableKey(item.doc, "capability");
      if (key) catalog.shapeInvalid.contracts.add(key);
      continue;
    }
    const contract = item.doc as Contract;
    const key = capabilityKey(contract.capability, contract.version);
    const node = catalog.nodes.get(item.nodeDir);
    const entry: CatalogContract = {
      file: item.file,
      nodeDir: item.nodeDir,
      nodeId: node?.manifest?.id ?? item.nodeDir,
      contract,
    };
    // Files arrive in path order, so the later file of a pair is the one reported.
    const existing = catalog.contracts.get(key);
    if (existing) {
      issues.add(
        "PRJ_DUPLICATE_CAPABILITY",
        item.file,
        "/capability",
        `${key} is already declared in ${existing.file}`,
      );
      catalog.duplicateContracts.push(entry);
      continue;
    }
    catalog.contracts.set(key, entry);
  }

  for (const item of project.definitions) {
    if (item.doc === undefined) {
      continue;
    }
    if (!checkShape(meta.definition, item.doc, item.file, issues)) {
      const key = readableKey(item.doc, "id");
      if (key) catalog.shapeInvalid.definitions.add(key);
      continue;
    }
    const definition = item.doc as Definition;
    const key = capabilityKey(definition.id, definition.version);
    const existing = catalog.definitions.get(key);
    if (existing) {
      issues.add(
        "PRJ_DUPLICATE_DEFINITION",
        item.file,
        "/id",
        `${key} is already declared in ${existing.file}`,
      );
      continue;
    }
    catalog.definitions.set(key, { file: item.file, definition });
  }

  return catalog;
}
