import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { generateGo, generateTs, runCodegen } from "../src/codegen/index.js";
import { impact } from "../src/impact.js";
import { idempotencyKey, idempotencyValues } from "../src/runtime/idempotency.js";
import { newNode } from "../src/scaffold.js";
import { validateDir } from "../src/validate/index.js";
import type { Contract } from "../src/types.js";
import { baseFiles, writeProject } from "./fixture.js";

const TS_ROOT = path.resolve(import.meta.dirname, "..");

function hasGo(): boolean {
  try {
    execFileSync("go", ["version"], { stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}

describe("impact", () => {
  const { catalog } = validateDir(writeProject(baseFiles()));

  it("capability -> definitions calling it, and their spawning parents", () => {
    expect(impact(catalog, "supplier.place_po").definitions).toEqual(["order@1"]);
    expect(impact(catalog, "supplier.cancel_po@1").definitions).toEqual(["order@1"]);
    expect(impact(catalog, "mapper.unused").definitions).toEqual([]);
  });

  it("follows spawns transitively", () => {
    const files = baseFiles();
    const item = files["definitions/item.v1.json"] as { steps: Record<string, unknown>; start: string };
    item.steps.cancel = {
      type: "call",
      capability: "supplier.cancel_po",
      version: 1,
      input: { po_id: "input.sku" },
      on: { cancelled: "notify", failed: "notify" },
    };
    item.start = "cancel";
    const result = validateDir(writeProject(files));
    expect(result.issues).toEqual([]);
    expect(impact(result.catalog, "supplier.cancel_po").definitions).toEqual(["item@1", "order@1"]);
  });

  it("follows signal-start targets like spawns", () => {
    const files = baseFiles();
    const item = files["definitions/item.v1.json"] as { steps: Record<string, unknown>; start: string };
    item.steps.cancel = {
      type: "call",
      capability: "supplier.cancel_po",
      version: 1,
      input: { po_id: "input.sku" },
      on: { cancelled: "notify", failed: "notify" },
    };
    item.start = "cancel";
    files["definitions/starter.v1.json"] = {
      id: "starter",
      version: 1,
      description: "Starts an item only through a signal start",
      input: { type: "object", properties: { sku: { type: "string" } }, required: ["sku"], additionalProperties: false },
      start: "kick",
      steps: {
        kick: {
          type: "signal",
          to: { workflow: { expr: "'item:' + input.sku" } },
          name: "item_done",
          start: { definition: "item", version: 1, input: { sku: "input.sku" } },
          next: "end",
        },
        end: { type: "end", status: "completed" },
      },
    };
    const result = validateDir(writeProject(files));
    expect(result.issues).toEqual([]);
    expect(impact(result.catalog, "supplier.cancel_po").definitions).toEqual(["item@1", "order@1", "starter@1"]);
    expect(impact(result.catalog, "starter").spawns).toEqual(["item@1"]);
  });

  it("definition -> capabilities and spawns", () => {
    const result = impact(catalog, "order");
    expect(result.capabilities).toEqual(["mapper.suggest@1", "supplier.cancel_po@1", "supplier.place_po@1"]);
    expect(result.spawns).toEqual(["item@1"]);
  });
});

describe("codegen", () => {
  const result = validateDir(writeProject(baseFiles()));
  const contractsOf = (dir: string): Contract[] =>
    [...result.catalog.contracts.values()].filter((c) => c.nodeDir === dir).map((c) => c.contract);

  it("writes one generated file per node, by runtime", () => {
    const written = runCodegen(result);
    expect(written.sort()).toEqual(["nodes/mapper/contracts.gen.ts", "nodes/supplier/contracts_gen.go"]);
    const ts = readFileSync(path.join(result.project.root, "nodes/mapper/contracts.gen.ts"), "utf8");
    expect(ts).toContain("export type SuggestV1Input");
    expect(ts).toContain("export type SuggestV1MatchedOutput");
    expect(ts).toContain('"suggest@1": SuggestV1Handler;');
    const go = readFileSync(path.join(result.project.root, "nodes/supplier/contracts_gen.go"), "utf8");
    expect(go).toContain("package supplier");
    expect(go).toContain("type PlacePoV1Input struct");
    expect(go).toMatch(/OrderID +string +`json:"order_id"`/);
    expect(go).toContain("type FindPoV1NotFoundOutput struct{}");
  });

  it("generated TS compiles under strict, types a handler and fits NodeHandlers", () => {
    const dir = path.join(TS_ROOT, "tests", ".gen");
    mkdirSync(dir, { recursive: true });
    const source = generateTs(contractsOf("mapper")).replace('from "@sidmesh/core"', 'from "../../src/index.js"');
    writeFileSync(path.join(dir, "contracts.gen.ts"), source);
    writeFileSync(
      path.join(dir, "index.ts"),
      [
        'import type { NodeHandlers } from "../../src/index.js";',
        'import type { Handlers } from "./contracts.gen.js";',
        "export const handlers: Handlers = {",
        '  "suggest@1": async (input, config) => ({',
        '    outcome: "matched",',
        "    output: { option_set_id: input.variant.length + (config.model ? 1 : 0), confidence: 0.9 },",
        "  }),",
        "};",
        "// What runWorker receives: a node's Handlers must be assignable to NodeHandlers.",
        "export const all: NodeHandlers = { mapper: handlers };",
        "",
      ].join("\n"),
    );
    const tsc = path.join(TS_ROOT, "node_modules", ".bin", "tsc");
    execFileSync(
      tsc,
      ["--noEmit", "--strict", "--skipLibCheck", "--module", "NodeNext", "--moduleResolution", "NodeNext", "--target", "ES2023", path.join(dir, "index.ts")],
      { stdio: "pipe" },
    );
  });

  it.skipIf(!hasGo())("generated Go passes go vet and is gofmt-clean", () => {
    const dir = mkdtempSync(path.join(tmpdir(), "sidmesh-go-"));
    writeFileSync(path.join(dir, "go.mod"), "module example.com/gen\n\ngo 1.22\n");
    const nested: Contract = {
      capability: "supplier.ship",
      version: 2,
      kind: "action",
      effects: "none",
      summary: "nested shapes",
      input: {
        type: "object",
        additionalProperties: false,
        required: ["address", "lines"],
        properties: {
          address: {
            type: "object",
            additionalProperties: false,
            required: ["city"],
            properties: { city: { type: "string" }, zip: { type: ["string", "null"] } },
          },
          lines: {
            type: "array",
            items: {
              type: "object",
              additionalProperties: false,
              properties: { sku: { type: "string" }, qty: { type: "integer" } },
            },
          },
          rush: { type: "boolean" },
          weight: { type: "number" },
          mode: { type: "string", enum: ["air", "sea"] },
        },
      },
      outcomes: { done: { description: "ok", output: { type: "object", additionalProperties: false, properties: {} } } },
    };
    writeFileSync(path.join(dir, "contracts_gen.go"), generateGo("supplier", [...contractsOf("supplier"), nested]));
    execFileSync("go", ["vet", "./..."], { cwd: dir, stdio: "pipe" });
    expect(execFileSync("gofmt", ["-l", "."], { cwd: dir, encoding: "utf8" })).toBe("");
  });
});

describe("new-node", () => {
  it("scaffolds a TS node that validates and generates", () => {
    const root = writeProject(baseFiles());
    const created = newNode(root, "notifier", "ts");
    expect(created).toEqual([
      "nodes/notifier/node.json",
      "nodes/notifier/contracts/example.v1.json",
      "nodes/notifier/index.ts",
    ]);
    const result = validateDir(root);
    expect(result.issues).toEqual([]);
    expect(runCodegen(result, "notifier")).toEqual(["nodes/notifier/contracts.gen.ts"]);
  });

  const goRuntime = path.resolve(TS_ROOT, "..", "go");
  it.skipIf(!hasGo() || !existsSync(path.join(goRuntime, "go.mod")))(
    "the scaffolded Go node compiles against the real Go runtime in ../go",
    () => {
      const root = writeProject({ "sidmesh.json": { name: "scaffold" } });
      newNode(root, "tracker", "go");
      runCodegen(validateDir(root), "tracker");
      writeFileSync(
        path.join(root, "go.mod"),
        [
          "module example.com/scaffold",
          "",
          "go 1.25",
          "",
          "require gitlab.com/sidcorp-it/sidmesh/go v0.0.0",
          "",
          `replace gitlab.com/sidcorp-it/sidmesh/go => ${goRuntime}`,
          "",
        ].join("\n"),
      );
      execFileSync("go", ["mod", "tidy"], { cwd: root, stdio: "pipe" });
      execFileSync("go", ["vet", "./nodes/..."], { cwd: root, stdio: "pipe" });
    },
    180_000,
  );

  it("scaffolds a Go node and refuses to overwrite", () => {
    const root = writeProject(baseFiles());
    newNode(root, "carrier", "go");
    expect(existsSync(path.join(root, "nodes/carrier/handlers.go"))).toBe(true);
    expect(validateDir(root).issues).toEqual([]);
    expect(() => newNode(root, "carrier", "go")).toThrow(/already exists/);
    expect(() => newNode(root, "Bad", "go")).toThrow(/must match/);
  });
});

describe("idempotency key", () => {
  it("is sha256 of tenant, capability@version and JSON-encoded values, newline-joined", () => {
    const expected = createHash("sha256").update('acme\nsupplier.place_po@1\n"o-1"\n2').digest("hex");
    expect(idempotencyKey("acme", "supplier.place_po", 1, ["o-1", 2])).toBe(expected);
  });

  it("reads values from the declared key paths", () => {
    const contract = { idempotency: { key: ["input.order_id", "input.line.sku"] } } as unknown as Contract;
    expect(idempotencyValues(contract, { order_id: "o-1", line: { sku: "S" } })).toEqual(["o-1", "S"]);
  });
});
