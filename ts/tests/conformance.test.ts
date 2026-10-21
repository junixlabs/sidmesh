import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { conformanceCases, runCase } from "../src/conformance.js";
import { specDir } from "../src/spec.js";
import { baseFiles, type Files } from "./fixture.js";

function writeCase(root: string, name: string, files: Files, expected: unknown): void {
  const dir = path.join(root, "projects", name);
  for (const [file, content] of Object.entries({ ...files, "expected.json": expected })) {
    mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
    writeFileSync(path.join(dir, file), JSON.stringify(content));
  }
}

describe("conformance runner", () => {
  it("matches (code, file) sets and optional pointers", () => {
    const root = mkdtempSync(path.join(tmpdir(), "sidmesh-conf-"));
    writeCase(root, "valid", baseFiles(), { valid: true, errors: [] });
    const broken = baseFiles();
    (broken["definitions/order.v1.json"] as { steps: { check: { else: string } } }).steps.check.else = "nope";
    writeCase(root, "start-unknown", broken, {
      valid: false,
      errors: [{ code: "DEF_STEP_UNKNOWN", file: "definitions/order.v1.json", pointer: "/steps/check/else" }],
    });
    writeCase(root, "wrong-expectation", broken, {
      valid: false,
      errors: [{ code: "DEF_UNREACHABLE", file: "definitions/order.v1.json" }],
    });

    expect(conformanceCases(root)).toEqual(["start-unknown", "valid", "wrong-expectation"]);
    expect(runCase(root, "valid").ok).toBe(true);
    expect(runCase(root, "start-unknown").ok).toBe(true);
    const wrong = runCase(root, "wrong-expectation");
    expect(wrong.ok).toBe(false);
    expect(wrong.missing).toEqual(["DEF_UNREACHABLE definitions/order.v1.json"]);
    expect(wrong.unexpected).toContain("DEF_STEP_UNKNOWN definitions/order.v1.json");
  });
});

// Shared fixtures written by the spec owners. Skipped until they exist.
const shared = path.join(specDir(), "conformance");
const sharedCases = conformanceCases(shared);

describe.skipIf(sharedCases.length === 0)("spec/conformance fixtures", () => {
  for (const name of sharedCases) {
    it(name, () => {
      const result = runCase(shared, name);
      expect({ missing: result.missing, unexpected: result.unexpected, ok: result.ok }).toEqual({
        missing: [],
        unexpected: [],
        ok: true,
      });
    });
  }
});
