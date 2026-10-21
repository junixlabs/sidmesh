import { describe, expect, it, vi } from "vitest";
import { loadProject } from "../src/project.js";
import { documentHash, prepareStart, projectDocuments, validateParams, type StartRequest } from "../src/runtime/prepare.js";
import type { Definition } from "../src/types.js";
import { validateProject } from "../src/validate/index.js";
import { baseFiles, writeProject } from "./fixture.js";

function prepare(req: Partial<StartRequest> = {}) {
  const project = loadProject(writeProject(baseFiles()));
  const { catalog, issues } = validateProject(project);
  expect(issues).toEqual([]);
  return prepareStart(catalog, project.config!.queues, {
    definition: "order",
    version: 1,
    workflowId: "acme:dp:1",
    tenant: "acme",
    input: { order_id: "1", items: [{ id: "a", sku: "s" }] },
    ...req,
  });
}

describe("prepareStart (mirrors go/engine Prepare)", () => {
  it("pins by hash the definition, the definitions it starts, and every contract incl. compensate and lookup", () => {
    const input = prepare({ params: { hold: "2h" } });
    expect(input.tenant).toBe("acme");
    expect(input.definition).toBe("definition:order@1");
    expect(Object.keys(input.pins).sort()).toEqual([
      "contract:mapper.suggest@1",
      "contract:supplier.cancel_po@1",
      "contract:supplier.find_po@1",
      "contract:supplier.place_po@1",
      "definition:item@1",
      "definition:order@1",
      "node:mapper",
      "node:supplier",
    ]);
    const files = baseFiles();
    expect(input.pins["definition:order@1"]).toBe(documentHash(files["definitions/order.v1.json"]));
    expect(input.pins["contract:supplier.find_po@1"]).toBe(documentHash(files["nodes/supplier/contracts/find_po.v1.json"]));
    expect(input.pins["node:supplier"]).toBe(documentHash(files["nodes/supplier/node.json"]));
    expect(Object.values(input.pins).every((h) => /^sha256:[0-9a-f]{64}$/.test(h))).toBe(true);
    expect(JSON.stringify(input)).not.toContain('"steps"');
    expect(input.queues.workflow).toMatch(/-workflows$/);
    expect(input.params).toEqual({ hold: "2h" });
  });

  it("hashes the JCS form, so key order and whitespace do not change a pin", () => {
    expect(documentHash({ b: 1, a: [1, "x"] })).toBe(documentHash(JSON.parse('{ "a": [1, "x"],  "b": 1 }')));
    expect(documentHash({ a: 1 })).not.toBe(documentHash({ a: 2 }));
  });

  it("lists every project document for the registry", () => {
    const project = loadProject(writeProject(baseFiles()));
    const docs = projectDocuments(validateProject(project).catalog);
    const names = docs.map((d) => `${d.kind}:${d.name}@${d.version}`);
    expect(names).toContain("contract:mapper.suggest@1");
    expect(names).toContain("node:supplier@0");
    expect(docs.every((d) => d.hash === documentHash(d.doc))).toBe(true);
  });

  it("omits params when there are no overrides, as Go's omitempty does", () => {
    expect("params" in prepare()).toBe(false);
  });

  it("rejects an unknown definition, invalid input and bad param overrides", () => {
    expect(() => prepare({ definition: "nope" })).toThrow(/definition nope@1 not found/);
    expect(() => prepare({ input: { order_id: 1 } })).toThrow(/^input:/);
    expect(() => prepare({ params: { nope: 1 } })).toThrow(/param "nope" is not declared/);
    expect(() => prepare({ params: { threshold: "high" } })).toThrow(/param "threshold"/);
  });
});

describe("validateParams (tenant params, SPEC §5)", () => {
  const order = baseFiles()["definitions/order.v1.json"] as Definition;

  it("accepts a partial object of declared, valid params", () => {
    expect(validateParams(order, { hold: "2h" })).toEqual({ ok: true, errors: [] });
    expect(validateParams(order, {})).toEqual({ ok: true, errors: [] });
  });

  it("reports every unknown key and every invalid value, by param", () => {
    const result = validateParams(order, { hold: 5, nope: 1, threshold: 0.5 });
    expect(result.ok).toBe(false);
    expect(result.errors.map((e) => e.param).sort()).toEqual(["hold", "nope"]);
    expect(result.errors.find((e) => e.param === "nope")!.message).toMatch(/not declared by order@1/);
  });
});

describe("SidmeshClient.prepare with ad-hoc definitions", () => {
  it("pins an ad-hoc definition next to the project's, and without it the start is refused", async () => {
    const { SidmeshClient } = await import("../src/client.js");
    const project = loadProject(writeProject(baseFiles()));
    const sm = new SidmeshClient({} as never, {} as never, { catalog: validateProject(project).catalog, queues: project.config!.queues });
    const adHoc = {
      ...(baseFiles()["definitions/item.v1.json"] as Definition),
      id: "probe",
    };
    const req = { definition: "probe", version: 1, workflowId: "w", tenant: "t", input: { sku: "s" } };
    expect(() => sm.prepare(req)).toThrow(/definition probe@1 not found/);
    const input = sm.prepare(req, [adHoc]);
    expect(input.definition).toBe("definition:probe@1");
    expect(input.pins["definition:probe@1"]).toBe(documentHash(adHoc));
  });

  it("compiles ad-hoc schemas off the catalog's Ajv, so a long-lived client does not cache them", async () => {
    const { SidmeshClient } = await import("../src/client.js");
    const project = loadProject(writeProject(baseFiles()));
    const catalog = validateProject(project).catalog;
    const sm = new SidmeshClient({} as never, {} as never, { catalog, queues: project.config!.queues });
    const compile = vi.spyOn(catalog.ajv, "compile");
    const adHoc = { ...(baseFiles()["definitions/item.v1.json"] as Definition), id: "probe" };
    sm.prepare({ definition: "probe", version: 1, workflowId: "w", tenant: "t", input: { sku: "s" }, params: { priority: "x" } }, [adHoc]);
    expect(compile).not.toHaveBeenCalled();
    sm.prepare({ definition: "item", version: 1, workflowId: "w", tenant: "t", input: { sku: "s" }, params: { priority: "x" } });
    expect(compile).toHaveBeenCalledTimes(2);
  });
});
