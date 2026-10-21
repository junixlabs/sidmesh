import { describe, expect, it } from "vitest";
import { baseFiles, doc, issuesAfter, writeProject, type Files } from "./fixture.js";
import { validateDir } from "../src/validate/index.js";
import { SPEC_VERSION, specSupported } from "../src/project.js";

const PO = "nodes/supplier/contracts/place_po.v1.json";
const FIND = "nodes/supplier/contracts/find_po.v1.json";
const CANCEL = "nodes/supplier/contracts/cancel_po.v1.json";
const SUGGEST = "nodes/mapper/contracts/suggest.v1.json";
const ORDER = "definitions/order.v1.json";
const ITEM = "definitions/item.v1.json";

const order = (f: Files) => doc(f, ORDER);
const steps = (f: Files) => order(f).steps;

describe("base fixture", () => {
  it("validates with no issues", () => {
    const issues = validateDir(writeProject(baseFiles())).issues;
    expect(issues).toEqual([]);
  });
});

type Case = [code: string, file: string, mutate: (f: Files) => void, pointer?: string];

const cases: Record<string, Case> = {
  // project
  "manifest missing": ["PRJ_MANIFEST_INVALID", "sidmesh.json", (f) => delete f["sidmesh.json"]],
  "manifest bad name": ["PRJ_MANIFEST_INVALID", "sidmesh.json", (f) => (f["sidmesh.json"] = { name: "Bad Name" })],
  "manifest spec not a string": ["PRJ_MANIFEST_INVALID", "sidmesh.json", (f) => (doc(f, "sidmesh.json").spec = 0.3), "/spec"],
  "manifest spec format": ["PRJ_MANIFEST_INVALID", "sidmesh.json", (f) => (doc(f, "sidmesh.json").spec = "v0.3"), "/spec"],
  "spec unsupported": ["PRJ_SPEC_UNSUPPORTED", "sidmesh.json", (f) => (doc(f, "sidmesh.json").spec = "0.2"), "/spec"],
  "shape: unknown field": ["PRJ_SHAPE_INVALID", PO, (f) => (doc(f, PO).extra = 1)],
  "shape: not json": ["PRJ_SHAPE_INVALID", PO, (f) => (f[PO] = "{not json")],
  "node dir mismatch": ["PRJ_NODE_DIR_MISMATCH", "nodes/mapper/node.json", (f) => (doc(f, "nodes/mapper/node.json").id = "other")],
  "duplicate capability": [
    "PRJ_DUPLICATE_CAPABILITY",
    "nodes/supplier/contracts/zz_cancel_po.v1.json",
    (f) => (f["nodes/supplier/contracts/zz_cancel_po.v1.json"] = doc(f, CANCEL)),
  ],
  "duplicate definition": ["PRJ_DUPLICATE_DEFINITION", "definitions/item.v2.json", (f) => (f["definitions/item.v2.json"] = doc(f, ITEM))],

  // contract
  "filename": ["CON_FILENAME", "nodes/supplier/contracts/cancel.v1.json", (f) => {
    f["nodes/supplier/contracts/cancel.v1.json"] = doc(f, CANCEL);
    delete f[CANCEL];
  }],
  "capability prefix": ["CON_CAPABILITY_PREFIX", SUGGEST, (f) => {
    doc(f, SUGGEST).capability = "other.suggest";
    order(f).steps.map.capability = "other.suggest";
  }],
  "reserved failed": ["CON_RESERVED_OUTCOME", CANCEL, (f) => (doc(f, CANCEL).outcomes.failed = doc(f, CANCEL).outcomes.cancelled), "/outcomes/failed"],
  "not_found outside lookup": ["CON_RESERVED_OUTCOME", CANCEL, (f) => (doc(f, CANCEL).outcomes.not_found = doc(f, CANCEL).outcomes.cancelled)],
  "write without idempotency": ["CON_WRITE_IDEMPOTENCY", PO, (f) => delete doc(f, PO).idempotency],
  "write without lookup": ["CON_WRITE_LOOKUP", PO, (f) => delete doc(f, PO).lookup],
  "idempotency path": ["CON_IDEMPOTENCY_PATH", PO, (f) => (doc(f, PO).idempotency.key = ["input.missing"]), "/idempotency/key/0"],
  "idempotency path not required": ["CON_IDEMPOTENCY_PATH", PO, (f) => {
    doc(f, PO).input.required = ["sku"];
    doc(f, FIND).input.required = ["sku"];
  }],
  "lookup unknown": ["CON_LOOKUP_UNKNOWN", PO, (f) => (doc(f, PO).lookup.version = 9)],
  "lookup other node": ["CON_LOOKUP_UNKNOWN", PO, (f) => (doc(f, PO).lookup = { capability: "mapper.suggest", version: 1 })],
  "lookup writes": ["CON_LOOKUP_EFFECTS", PO, (f) => {
    doc(f, FIND).effects = "write";
    doc(f, FIND).idempotency = { key: ["input.order_id"] };
    doc(f, FIND).lookup = { capability: "supplier.find_po", version: 1 };
  }],
  "lookup input differs": ["CON_LOOKUP_INPUT", PO, (f) => (doc(f, FIND).input.properties.sku = { type: "integer" })],
  "lookup outcomes differ": ["CON_LOOKUP_OUTCOMES", PO, (f) => delete doc(f, FIND).outcomes.rejected],
  "lookup output differs": ["CON_LOOKUP_OUTCOMES", PO, (f) => (doc(f, FIND).outcomes.accepted.output.properties.po_id = { type: "integer" })],
  "ai write": ["CON_AI_WRITE", SUGGEST, (f) => {
    const c = doc(f, SUGGEST);
    c.effects = "write";
    c.idempotency = { key: ["input.variant"] };
    c.lookup = { capability: "mapper.suggest", version: 1 };
  }],
  "ai confidence": ["CON_AI_CONFIDENCE", SUGGEST, (f) => (doc(f, SUGGEST).outcomes.matched.output.properties.confidence.maximum = 100)],
  "ledger path": ["CON_LEDGER_PATH", SUGGEST, (f) => doc(f, SUGGEST).ledger.push("output.nope"), "/ledger/3"],
  "schema strict: open object": ["CON_SCHEMA_STRICT", CANCEL, (f) => delete doc(f, CANCEL).input.additionalProperties],
  "schema strict: oneOf": ["CON_SCHEMA_STRICT", CANCEL, (f) => (doc(f, CANCEL).input.properties.po_id = { oneOf: [{ type: "string" }] })],
  "schema strict: array without items": ["CON_SCHEMA_STRICT", CANCEL, (f) => (doc(f, CANCEL).input.properties.po_id = { type: "array" })],
  "schema invalid": ["CON_SCHEMA_INVALID", CANCEL, (f) => (doc(f, CANCEL).input.properties.po_id = { type: "string", minLength: -1 })],

  // definition
  "def schema strict": ["DEF_SCHEMA_STRICT", ORDER, (f) => delete order(f).signals.cancel.schema.additionalProperties],
  "step unknown": ["DEF_STEP_UNKNOWN", ORDER, (f) => (steps(f).check.else = "nowhere"), "/steps/check/else"],
  "start unknown": ["DEF_STEP_UNKNOWN", ORDER, (f) => (order(f).start = "nope"), "/start"],
  "unreachable": ["DEF_UNREACHABLE", ORDER, (f) => (steps(f).orphan = { type: "end", status: "completed" }), "/steps/orphan"],
  "dead end": ["DEF_DEAD_END", ORDER, (f) => {
    steps(f).loop = { type: "wait", duration: { const: "1h" }, on: { elapsed: "loop" } };
    steps(f).check.else = "loop";
  }, "/steps/loop"],
  "capability unknown": ["DEF_CAPABILITY_UNKNOWN", ORDER, (f) => (steps(f).map.version = 7), "/steps/map"],
  "compensate unknown": ["DEF_CAPABILITY_UNKNOWN", ORDER, (f) => (steps(f).place.compensate.version = 7), "/steps/place"],
  "outcome unrouted": ["DEF_OUTCOME_UNROUTED", ORDER, (f) => delete steps(f).place.on.rejected, "/steps/place/on"],
  "unknown unrouted on write": ["DEF_OUTCOME_UNROUTED", ORDER, (f) => delete steps(f).place.on.unknown],
  "failed unrouted": ["DEF_OUTCOME_UNROUTED", ORDER, (f) => delete steps(f).map.on.failed],
  "outcome extra": ["DEF_OUTCOME_EXTRA", ORDER, (f) => (steps(f).map.on.unknown = "failed"), "/steps/map/on/unknown"],
  "gate not ai": ["DEF_GATE_NOT_AI", ORDER, (f) => (steps(f).place.gate = { threshold: { const: 0.5 }, below: "review" })],
  "input missing": ["DEF_INPUT_MISSING", ORDER, (f) => delete steps(f).place.input.sku, "/steps/place/input"],
  "input extra": ["DEF_INPUT_EXTRA", ORDER, (f) => (steps(f).place.input.extra = { const: 1 }), "/steps/place/input/extra"],
  "config extra": ["DEF_INPUT_EXTRA", ORDER, (f) => (steps(f).map.config.temperature = { const: 1 })],
  "spawn input missing": ["DEF_INPUT_MISSING", ORDER, (f) => delete steps(f).items.input.sku],
  "item outside spawn": ["DEF_BINDING_ROOT", ORDER, (f) => (steps(f).place.input.sku = "item.sku")],
  "item in foreach": ["DEF_BINDING_ROOT", ORDER, (f) => (steps(f).items.foreach = "item.list")],
  "step does not dominate": ["DEF_BINDING_STEP", ORDER, (f) => (steps(f).map.input.variant = "steps.review.output.option_set_id")],
  "step refers to itself": ["DEF_BINDING_STEP", ORDER, (f) => (steps(f).place.input.sku = "steps.place.output.po_id")],
  "binding step unknown": ["DEF_BINDING_STEP", ORDER, (f) => (steps(f).done.output.po = "steps.ghost.output.x")],
  "binding path input": ["DEF_BINDING_PATH", ORDER, (f) => (steps(f).place.input.sku = "input.nope")],
  "binding path step output": ["DEF_BINDING_PATH", ORDER, (f) => (steps(f).done.output.po = "steps.place.output.nope")],
  "binding path signal": ["DEF_BINDING_PATH", ORDER, (f) => (steps(f).done.output.po = "signals.cancel.nope")],
  "binding path workflow": ["DEF_BINDING_PATH", ORDER, (f) => (steps(f).done.output.po = "workflow.name")],
  "binding path item": ["DEF_BINDING_PATH", ORDER, (f) => (steps(f).items.input.sku = "item.nope")],
  "param unknown": ["DEF_PARAM_UNKNOWN", ORDER, (f) => (steps(f).hold.duration = "params.nope")],
  "param default": ["DEF_PARAM_DEFAULT", ORDER, (f) => (order(f).params.threshold.default = "high"), "/params/threshold/default"],
  "signal unknown in binding": ["DEF_SIGNAL_UNKNOWN", ORDER, (f) => (steps(f).done.output.po = "signals.nope.reason")],
  "signal unknown in wait": ["DEF_SIGNAL_UNKNOWN", ORDER, (f) => {
    steps(f).hold = { type: "wait", signal: "nope", on: { received: "map" } };
  }],
  "wait both signal and duration": ["DEF_WAIT_SHAPE", ORDER, (f) => (steps(f).hold.signal = "cancel")],
  "wait timeout without route": ["DEF_WAIT_SHAPE", ORDER, (f) => (steps(f).hold.timeout = { const: "1h" })],
  "wait duration without elapsed": ["DEF_WAIT_SHAPE", ORDER, (f) => {
    steps(f).hold.on = { received: "map" };
  }],
  "human unrouted": ["DEF_HUMAN_UNROUTED", ORDER, (f) => (steps(f).review.outcomes.rejected = { type: "object", properties: {}, additionalProperties: false })],
  "human extra route": ["DEF_HUMAN_UNROUTED", ORDER, (f) => (steps(f).review.on.rejected = "failed")],
  "spawn unknown": ["DEF_SPAWN_UNKNOWN", ORDER, (f) => (steps(f).items.version = 4)],
  "spawn without key is a shape error": ["PRJ_SHAPE_INVALID", ORDER, (f) => delete steps(f).items.key],
  // v0.1
  "var unknown in set": ["DEF_VAR_UNKNOWN", ORDER, (f) => (steps(f).count.values.retries = { const: 1 }), "/steps/count/values/retries"],
  "var unknown in binding": ["DEF_VAR_UNKNOWN", ORDER, (f) => (steps(f).done.output.po = "vars.nope"), "/steps/done/output/po"],
  "var path": ["DEF_BINDING_PATH", ORDER, (f) => (steps(f).done.output.po = "vars.attempts.x")],
  "var default": ["DEF_VAR_DEFAULT", ORDER, (f) => (order(f).vars.attempts.default = "zero"), "/vars/attempts/default"],
  "duration const invalid": ["DEF_DURATION_INVALID", ORDER, (f) => (steps(f).ship_wait.timeout = { const: "a week" }), "/steps/ship_wait/timeout"],
  "collect window invalid": ["DEF_DURATION_INVALID", ORDER, (f) => (steps(f).ship_wait.collect.window = { const: 60 }), "/steps/ship_wait/collect/window"],
  "drain names an undeclared signal": ["DEF_DRAIN_UNKNOWN", ORDER, (f) => (steps(f).done.drain = ["shipped", "nope"]), "/steps/done/drain/1"],
  "collect.max binding with an undeclared param": ["DEF_PARAM_UNKNOWN", ORDER, (f) => (steps(f).ship_wait.collect.max = "params.nope"), "/steps/ship_wait/collect/max"],
  "collect without signal": ["DEF_WAIT_SHAPE", ORDER, (f) => (steps(f).hold.collect = { window: { const: "1h" } }), "/steps/hold"],
  "signal to children of a non-spawn": ["DEF_SIGNAL_TARGET", ORDER, (f) => (steps(f).nudge.to = { children: "place" }), "/steps/nudge"],
  "signal to children of a missing step": ["DEF_SIGNAL_TARGET", ORDER, (f) => (steps(f).nudge.to = { children: "ghost" })],
  "signal start without to.workflow": ["DEF_SIGNAL_TARGET", ORDER, (f) => (steps(f).audit.to = "parent"), "/steps/audit"],
  "signal start unknown definition": ["DEF_SPAWN_UNKNOWN", ORDER, (f) => (steps(f).audit.start.version = 9), "/steps/audit"],
  "signal start input missing": ["DEF_INPUT_MISSING", ORDER, (f) => delete steps(f).audit.start.input.sku, "/steps/audit/start/input"],
  "signal start param unknown": ["DEF_PARAM_UNKNOWN", ORDER, (f) => (steps(f).audit.start.params.speed = { const: 1 }), "/steps/audit/start/params/speed"],
  "spawn param unknown": ["DEF_PARAM_UNKNOWN", ORDER, (f) => (steps(f).items.params.speed = { const: 1 }), "/steps/items/params/speed"],
  "step time follows dominance": ["DEF_BINDING_STEP", ORDER, (f) => (steps(f).map.input.variant = "steps.review.started_at")],
  "step time has no fields": ["DEF_BINDING_PATH", ORDER, (f) => (steps(f).done.output.po = "steps.place.ended_at.day")],
  "format outside the list": ["CON_SCHEMA_STRICT", CANCEL, (f) => (doc(f, CANCEL).input.properties.po_id.format = "hostname"), "/input/properties/po_id"],
  "expr does not parse": ["DEF_EXPR_INVALID", ORDER, (f) => (steps(f).check.cases[0].when = "steps.place.outcome ==")],
  "expr unknown variable": ["DEF_EXPR_INVALID", ORDER, (f) => (steps(f).place.input.sku = { expr: "orderz + 'x'" })],
  "expr path checked": ["DEF_BINDING_PATH", ORDER, (f) => (steps(f).check.cases[0].when = "steps.place.output.nope == 'x'")],
  "expr dominance checked": ["DEF_BINDING_STEP", ORDER, (f) => (steps(f).map.input.variant = { expr: "string(steps.review.output.option_set_id)" })],
};

describe("rules", () => {
  for (const [name, [code, file, mutate, ptr]] of Object.entries(cases)) {
    it(`${code}: ${name}`, () => {
      const issues = issuesAfter(mutate);
      const hit = issues.filter((i) => i.code === code && i.file === file);
      expect(hit, JSON.stringify(issues, null, 2)).not.toHaveLength(0);
      if (ptr !== undefined) {
        expect(hit.map((i) => i.pointer)).toContain(ptr);
      }
    });
  }
});

describe("precision", () => {
  it("accepts v0.1 roots: vars anywhere, workflow.now, step times, title, allowed formats", () => {
    const issues = issuesAfter((f) => {
      steps(f).hold.duration = { expr: "string(vars.attempts) + 'h'" };
      steps(f).done.output.at = "workflow.now";
      steps(f).done.output.took = { expr: "timestamp(steps.place.ended_at) - timestamp(steps.place.started_at)" };
      steps(f).done.output.batch = "steps.ship_wait.output.items";
      doc(f, CANCEL).input.title = "Cancel input";
      doc(f, CANCEL).input.properties.po_id.format = "uuid";
    });
    expect(issues).toEqual([]);
  });

  it("accepts workflow.tenant in a binding and in CEL (v0.1.1)", () => {
    const issues = issuesAfter((f) => {
      steps(f).done.output.tenant = "workflow.tenant";
      steps(f).check.cases[0].when = "workflow.tenant == 'acme'";
    });
    expect(issues).toEqual([]);
  });

  it("accepts v0.2 end drain and a collect.max binding", () => {
    const issues = issuesAfter((f) => {
      steps(f).done.drain = ["shipped", "cancel"];
      steps(f).ship_wait.collect.max = { expr: "size(input.items) + 1" };
    });
    expect(issues).toEqual([]);
  });

  it("a referenced definition that fails its shape still exists: only its shape errors", () => {
    const issues = issuesAfter((f) => (doc(f, ITEM).steps.end.status = "bogus"));
    expect(new Set(issues.map((i) => `${i.code} ${i.file}`))).toEqual(new Set([`PRJ_SHAPE_INVALID ${ITEM}`]));
  });

  it("a called and compensating contract that fails its shape still exists: only its shape errors", () => {
    const issues = issuesAfter((f) => {
      doc(f, CANCEL).kind = "bogus";
      doc(f, FIND).kind = "bogus";
    });
    expect(new Set(issues.map((i) => i.code))).toEqual(new Set(["PRJ_SHAPE_INVALID"]));
  });

  it("a shape-invalid document without a readable version is not registered", () => {
    const issues = issuesAfter((f) => (doc(f, ITEM).version = "one"));
    expect(issues.map((i) => i.code)).toContain("DEF_SPAWN_UNKNOWN");
  });

  it("an unknown workflow field still reports DEF_BINDING_PATH", () => {
    const issues = issuesAfter((f) => (steps(f).done.output.x = "workflow.region"));
    expect(issues.map((i) => i.code)).toEqual(["DEF_BINDING_PATH"]);
  });

  it("an unknown start reports DEF_STEP_UNKNOWN only, not every step unreachable", () => {
    const issues = issuesAfter((f) => (order(f).start = "nope"));
    expect(issues.map((i) => i.code)).toEqual(["DEF_STEP_UNKNOWN"]);
  });

  it("a compensation may read the output of the step it undoes", () => {
    const issues = issuesAfter(() => {});
    expect(issues.filter((i) => i.code === "DEF_BINDING_STEP")).toEqual([]);
  });

  it("a signal goto is an entry point, so steps it reaches are not dominated by start", () => {
    const issues = issuesAfter((f) => {
      steps(f).cancelled = { type: "end", status: "cancelled", output: { po: "steps.place.output.po_id" } };
      delete steps(f).end_cancelled;
    });
    expect(issues.map((i) => i.code)).toEqual(["DEF_BINDING_STEP"]);
  });

  it("comprehension variables are not free identifiers", () => {
    const issues = issuesAfter((f) => {
      steps(f).check.cases[0].when = "input.items.exists(x, x.sku == 'a')";
    });
    expect(issues).toEqual([]);
  });
});

describe("spec version", () => {
  it("accepts the implemented spec and an absent one", () => {
    expect(issuesAfter((f) => (doc(f, "sidmesh.json").spec = SPEC_VERSION))).toEqual([]);
    expect(issuesAfter(() => {})).toEqual([]);
  });

  it("names both versions when unsupported", () => {
    const issues = issuesAfter((f) => (doc(f, "sidmesh.json").spec = "0.2"));
    expect(issues.map((i) => i.message)).toEqual([
      `project declares spec 0.2; this toolchain implements ${SPEC_VERSION}, see the upgrade notes in CHANGELOG.md`,
    ]);
  });

  it("before 1.0 needs an exact match; from 1.0 the major matches and the minor is not newer", () => {
    expect(specSupported("0.3", "0.3")).toBe(true);
    expect(specSupported("0.2", "0.3")).toBe(false);
    expect(specSupported("0.4", "0.3")).toBe(false);
    expect(specSupported("1.0", "0.3")).toBe(false);
    expect(specSupported("1.0", "1.2")).toBe(true);
    expect(specSupported("1.2", "1.2")).toBe(true);
    expect(specSupported("1.3", "1.2")).toBe(false);
    expect(specSupported("0.9", "1.2")).toBe(false);
    expect(specSupported("2.0", "1.2")).toBe(false);
  });
});
