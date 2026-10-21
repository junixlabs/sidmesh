import { writeFileSync } from "node:fs";
import path from "node:path";
import type { Contract } from "../types.js";
import type { ValidationResult } from "../validate/index.js";
import { generateGo } from "./go.js";
import { generateTs } from "./typescript.js";

export { generateGo } from "./go.js";
export { generateTs } from "./typescript.js";

/**
 * Write contracts.gen.ts (TS nodes) or contracts_gen.go (Go nodes) next to each node's
 * contracts. Only shape-valid contracts are used; callers should refuse to generate from a
 * project with validation issues. Returns the written paths relative to the project root.
 */
export function runCodegen(result: ValidationResult, onlyNode?: string): string[] {
  const written: string[] = [];
  for (const node of result.catalog.nodes.values()) {
    if (!node.manifest || (onlyNode && node.dir !== onlyNode)) {
      continue;
    }
    const contracts: Contract[] = [...result.catalog.contracts.values()]
      .filter((c) => c.nodeDir === node.dir)
      .sort((a, b) => a.file.localeCompare(b.file))
      .map((c) => c.contract);
    if (contracts.length === 0) {
      continue;
    }
    const isGo = node.manifest.runtime === "go";
    const file = `nodes/${node.dir}/${isGo ? "contracts_gen.go" : "contracts.gen.ts"}`;
    const content = isGo ? generateGo(node.manifest.id, contracts) : generateTs(contracts);
    writeFileSync(path.join(result.project.root, file), content);
    written.push(file);
  }
  return written;
}
