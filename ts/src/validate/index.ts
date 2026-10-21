import { formatIssue, IssueList, type Issue } from "../issues.js";
import { loadProject, type Project } from "../project.js";
import { buildCatalog, type Catalog } from "./catalog.js";
import { validateContracts } from "./contracts.js";
import { validateDefinitions } from "./definitions.js";

export interface ValidationResult {
  project: Project;
  catalog: Catalog;
  issues: Issue[];
}

/** Run every rule in SPEC §2–§3 over a loaded project. */
export function validateProject(project: Project): ValidationResult {
  const issues = new IssueList();
  issues.items.push(...project.loadIssues);
  const catalog = buildCatalog(project, issues);
  validateContracts(catalog, issues);
  validateDefinitions(catalog, issues);
  return { project, catalog, issues: issues.items };
}

export function validateDir(dir: string): ValidationResult {
  return validateProject(loadProject(dir));
}

/** Validate for running: any issue, or a missing sidmesh.json, is fatal. */
export function requireValid(project: Project): ValidationResult & { config: NonNullable<Project["config"]> } {
  const result = validateProject(project);
  if (result.issues.length > 0 || !project.config) {
    throw new Error(`project does not validate:\n${result.issues.map(formatIssue).join("\n")}`);
  }
  return { ...result, config: project.config };
}

export { capabilityKey } from "../capability.js";
export type { Catalog, CatalogContract, CatalogDefinition } from "./catalog.js";
