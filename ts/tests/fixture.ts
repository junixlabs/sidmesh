import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { validateDir } from "../src/validate/index.js";
import type { Issue } from "../src/issues.js";

export type Files = Record<string, unknown>;

const obj = (properties: Record<string, unknown>, required: string[] = Object.keys(properties)) => ({
  type: "object",
  properties,
  required,
  additionalProperties: false,
});
const str = () => ({ type: "string" });

/** A small project that passes every rule; tests mutate one thing at a time. */
export function baseFiles(): Files {
  // Factories, not shared objects: tests mutate one contract and must not touch the other.
  const poInput = () => obj({ order_id: str(), sku: str() });
  const accepted = () => ({ description: "PO created", output: obj({ po_id: str() }) });
  const rejected = () => ({ description: "Supplier refused", output: obj({ reason: str() }) });
  return {
    "sidmesh.json": { name: "demo" },
    "nodes/supplier/node.json": { id: "supplier", responsibility: "Places POs.", runtime: "go" },
    "nodes/supplier/contracts/place_po.v1.json": {
      capability: "supplier.place_po",
      version: 1,
      kind: "action",
      effects: "write",
      summary: "Place a PO.",
      input: poInput(),
      outcomes: { accepted: accepted(), rejected: rejected() },
      idempotency: { key: ["input.order_id"] },
      lookup: { capability: "supplier.find_po", version: 1 },
    },
    "nodes/supplier/contracts/find_po.v1.json": {
      capability: "supplier.find_po",
      version: 1,
      kind: "action",
      effects: "none",
      summary: "Find an earlier PO attempt.",
      input: poInput(),
      outcomes: { accepted: accepted(), rejected: rejected(), not_found: { description: "No attempt", output: obj({}) } },
    },
    "nodes/supplier/contracts/cancel_po.v1.json": {
      capability: "supplier.cancel_po",
      version: 1,
      kind: "action",
      effects: "none",
      summary: "Cancel a PO.",
      input: obj({ po_id: str() }),
      outcomes: { cancelled: { description: "Cancelled", output: obj({}) } },
    },
    "nodes/mapper/node.json": { id: "mapper", responsibility: "Maps variants.", runtime: "ts" },
    "nodes/mapper/contracts/suggest.v1.json": {
      capability: "mapper.suggest",
      version: 1,
      kind: "ai",
      effects: "none",
      summary: "Suggest an option set.",
      config: obj({ model: str() }, []),
      input: obj({ variant: str() }),
      outcomes: {
        matched: {
          description: "Found one",
          output: obj({ option_set_id: { type: "integer" }, confidence: { type: "number", minimum: 0, maximum: 1 } }),
        },
      },
      ledger: ["input.variant", "output.option_set_id", "config.model"],
    },
    "definitions/order.v1.json": {
      id: "order",
      version: 1,
      description: "Order flow",
      input: obj({ order_id: str(), items: { type: "array", items: obj({ id: str(), sku: str() }) } }),
      params: {
        hold: { schema: { type: "string" }, default: "4h" },
        threshold: { schema: { type: "number" }, default: 0.8 },
      },
      vars: { attempts: { schema: { type: "integer" }, default: 0 } },
      signals: {
        cancel: { schema: obj({ reason: str() }, []), goto: "cancelled" },
        shipped: { schema: obj({ tracking: str() }) },
      },
      search: { OrderId: "input.order_id" },
      start: "hold",
      steps: {
        hold: { type: "wait", duration: "params.hold", on: { elapsed: "map" } },
        map: {
          type: "call",
          capability: "mapper.suggest",
          version: 1,
          input: { variant: "input.order_id" },
          config: { model: { const: "small" } },
          gate: { threshold: "params.threshold", below: "review" },
          on: { matched: "place", failed: "failed" },
        },
        review: {
          type: "human",
          task: "review.mapping",
          input: { suggestion: "steps.map.output.option_set_id" },
          outcomes: { approved: obj({ option_set_id: { type: "integer" } }) },
          on: { approved: "place" },
        },
        place: {
          type: "call",
          capability: "supplier.place_po",
          version: 1,
          input: { order_id: "input.order_id", sku: { expr: "input.order_id + '-x'" } },
          compensate: {
            capability: "supplier.cancel_po",
            version: 1,
            input: { po_id: "steps.place.output.po_id" },
          },
          on: { accepted: "count", rejected: "failed", failed: "failed", unknown: "failed" },
        },
        count: { type: "set", values: { attempts: { expr: "vars.attempts + 1" } }, next: "items" },
        nudge: { type: "signal", to: { children: "items" }, name: "ping", next: "audit" },
        audit: {
          type: "signal",
          to: { workflow: { expr: "'audit:' + input.order_id" } },
          name: "item_done",
          start: { definition: "item", version: 1, input: { sku: "input.order_id" }, params: { priority: { const: "low" } } },
          next: "ship_wait",
        },
        ship_wait: {
          type: "wait",
          signal: "shipped",
          collect: { window: { const: "1h" }, max: 3 },
          timeout: { const: "7d" },
          on: { received: "check", timeout: "check" },
        },
        items: {
          type: "spawn",
          definition: "item",
          version: 1,
          foreach: "input.items",
          key: "item.id",
          input: { sku: "item.sku" },
          params: { priority: { const: "high" } },
          wait: "all",
          next: "nudge",
        },
        check: {
          type: "decide",
          cases: [{ when: "steps.place.outcome == 'accepted' && size(steps.items.output.children) > 0", next: "done" }],
          else: "failed",
        },
        cancelled: { type: "compensate", next: "end_cancelled" },
        end_cancelled: { type: "end", status: "cancelled" },
        failed: { type: "end", status: "failed" },
        done: { type: "end", status: "completed", output: { po: "steps.place.output.po_id" } },
      },
    },
    "definitions/item.v1.json": {
      id: "item",
      version: 1,
      description: "Item flow",
      input: obj({ sku: str() }),
      params: { priority: { schema: { type: "string" }, default: "normal" } },
      start: "notify",
      steps: {
        notify: { type: "signal", to: "parent", name: "item_done", payload: { sku: "input.sku" }, next: "end" },
        end: { type: "end", status: "completed" },
      },
    },
  };
}

export function writeProject(files: Files): string {
  const root = mkdtempSync(path.join(tmpdir(), "sidmesh-"));
  for (const [file, content] of Object.entries(files)) {
    const full = path.join(root, file);
    mkdirSync(path.dirname(full), { recursive: true });
    writeFileSync(full, typeof content === "string" ? content : JSON.stringify(content, null, 2));
  }
  return root;
}

/** Apply a mutation to a fresh copy of the base files and return the issues. */
export function issuesAfter(mutate: (files: Files) => void): Issue[] {
  const files = structuredClone(baseFiles());
  mutate(files);
  return validateDir(writeProject(files)).issues;
}

/** Typed access into the fixture for mutations. */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function doc(files: Files, file: string): any {
  return files[file];
}
