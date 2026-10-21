#!/usr/bin/env node
import path from "node:path";
import { runCodegen } from "./codegen/index.js";
import { runConformance, runVectors } from "./conformance.js";
import { impact } from "./impact.js";
import { formatIssue } from "./issues.js";
import { newNode } from "./scaffold.js";
import { validateDir } from "./validate/index.js";

const USAGE = `usage:
  sidmesh validate [projectDir]
  sidmesh impact <capability|definition> [projectDir]
  sidmesh codegen [projectDir]
  sidmesh new-node <id> --runtime go|ts [projectDir]
  sidmesh conformance [conformanceDir]`;

function fail(message: string): never {
  console.error(message);
  process.exit(1);
}

/** Validate and print issues; exits 1 when the project has any. */
function validateOrExit(dir: string) {
  const result = validateDir(dir);
  if (result.issues.length > 0) {
    for (const issue of result.issues) {
      console.error(formatIssue(issue));
    }
    fail(`${result.issues.length} issue(s)`);
  }
  return result;
}

async function main(argv: string[]): Promise<void> {
  const [command, ...rest] = argv;
  switch (command) {
    case "validate": {
      const result = validateOrExit(rest[0] ?? ".");
      const { catalog } = result;
      console.log(
        `ok: ${catalog.nodes.size} node(s), ${catalog.contracts.size} contract(s), ${catalog.definitions.size} definition(s)`,
      );
      return;
    }
    case "impact": {
      const target = rest[0] ?? fail(USAGE);
      const result = validateOrExit(rest[1] ?? ".");
      console.log(JSON.stringify(impact(result.catalog, target), null, 2));
      return;
    }
    case "codegen": {
      const result = validateOrExit(rest[0] ?? ".");
      for (const file of runCodegen(result)) {
        console.log(`wrote ${file}`);
      }
      return;
    }
    case "new-node": {
      const id = rest[0] ?? fail(USAGE);
      const flag = rest.indexOf("--runtime");
      const runtime = flag >= 0 ? rest[flag + 1] : undefined;
      if (runtime !== "go" && runtime !== "ts") {
        fail("--runtime go|ts is required");
      }
      const positional = rest.slice(1).filter((_, i, all) => all[i - 1] !== "--runtime" && all[i] !== "--runtime");
      const dir = positional[0] ?? ".";
      for (const file of newNode(dir, id, runtime)) {
        console.log(`created ${file}`);
      }
      const result = validateDir(dir);
      for (const file of runCodegen(result, id)) {
        console.log(`wrote ${file}`);
      }
      return;
    }
    case "conformance": {
      const dir = path.resolve(rest[0] ?? "spec/conformance");
      const results = [...runConformance(dir), ...(await runVectors(dir))];
      let failed = 0;
      for (const r of results) {
        if (r.ok) {
          console.log(`ok    ${r.name}`);
        } else {
          failed++;
          console.log(`FAIL  ${r.name}`);
          r.missing.forEach((m) => console.log(`        missing    ${m}`));
          r.unexpected.forEach((u) => console.log(`        unexpected ${u}`));
        }
      }
      console.log(`${results.length - failed}/${results.length} checks pass`);
      if (failed > 0) {
        process.exit(1);
      }
      return;
    }
    default:
      fail(USAGE);
  }
}

try {
  await main(process.argv.slice(2));
} catch (error) {
  fail((error as Error).message);
}
