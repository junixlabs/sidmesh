import { ApplicationFailure } from "@temporalio/common";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { handlerKey } from "../src/codegen/names.js";
import { loadProject } from "../src/project.js";
import { createCallActivity, type CallResponse, type Handler } from "../src/runtime/activity.js";
import { runVectors } from "../src/conformance.js";
import { specDir } from "../src/spec.js";
import type { Contract } from "../src/types.js";
import { writeProject, type Files } from "./fixture.js";

// Shared vectors in spec/conformance, written once for both runtimes.
const dir = path.join(specDir(), "conformance");
const read = (name: string) => JSON.parse(readFileSync(path.join(dir, name), "utf8"));

describe("shared value vectors (idempotency, document hash, codec)", () => {
  it("all pass, and the files are present", async () => {
    const results = await runVectors(dir);
    expect(results.filter((r) => !r.ok)).toEqual([]);
    for (const prefix of ["idempotency:", "document-hash:", "codec:"]) {
      expect(results.some((r) => r.name.startsWith(prefix)), prefix).toBe(true);
    }
  });
});

interface PayloadCase {
  name: string;
  contract: string;
  input: unknown;
  config?: unknown;
  result: CallResponse;
  expect: "ok" | "INPUT_INVALID" | "OUTPUT_INVALID";
}

describe.skipIf(!existsSync(path.join(dir, "payloads.json")))("payload vectors", () => {
  const data: { contracts: Record<string, Contract>; cases: PayloadCase[] } = existsSync(path.join(dir, "payloads.json"))
    ? read("payloads.json")
    : { contracts: {}, cases: [] };

  // One TS node per capability prefix, so the real project loader and wrapper are used.
  const files: Files = { "sidmesh.json": { name: "vectors" } };
  for (const contract of Object.values(data.contracts)) {
    const [node, verb] = contract.capability.split(".");
    files[`nodes/${node}/node.json`] = { id: node, responsibility: "Payload vectors.", runtime: "ts" };
    files[`nodes/${node}/contracts/${verb}.v${contract.version}.json`] = contract;
  }
  const project = writeProject(files);

  for (const c of data.cases) {
    it(`${c.name} -> ${c.expect}`, async () => {
      const contract = data.contracts[c.contract] as Contract;
      const node = contract.capability.split(".")[0] as string;
      const handler: Handler = async () => c.result;
      const call = createCallActivity(loadProject(project), {
        [node]: { [handlerKey(contract.capability, contract.version)]: handler },
      })["sidmesh.call"];
      const run = call({
        capability: contract.capability,
        version: contract.version,
        input: c.input,
        config: c.config,
        meta: { workflowId: "w", stepId: "s", attempt: 1, tenant: "t" },
      });
      if (c.expect === "ok") {
        await expect(run).resolves.toEqual(c.result);
        return;
      }
      const failure = await run.then(
        () => undefined,
        (error: unknown) => error,
      );
      expect(failure).toBeInstanceOf(ApplicationFailure);
      expect((failure as ApplicationFailure).type).toBe(c.expect);
    });
  }
});
