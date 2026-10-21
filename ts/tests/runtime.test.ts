import { ApplicationFailure } from "@temporalio/common";
import { describe, expect, it } from "vitest";
import { loadProject } from "../src/project.js";
import { createCallActivity, FatalError, type Handler, type Meta } from "../src/runtime/activity.js";
import { baseFiles, writeProject } from "./fixture.js";

const meta: Meta = { workflowId: "acme:dp:1", stepId: "map", attempt: 1, tenant: "acme" };

function activity(handler: Handler) {
  const project = loadProject(writeProject(baseFiles()));
  return createCallActivity(project, { mapper: { "suggest@1": handler } })["sidmesh.call"];
}

const request = (input: unknown, config: unknown = { model: "small" }) => ({
  capability: "mapper.suggest",
  version: 1,
  input,
  config,
  meta,
});

async function failureOf(promise: Promise<unknown>): Promise<ApplicationFailure> {
  try {
    await promise;
  } catch (error) {
    if (error instanceof ApplicationFailure) {
      return error;
    }
    throw error;
  }
  throw new Error("expected a failure");
}

describe("sidmesh.call", () => {
  const ok: Handler = async (input) => ({
    outcome: "matched",
    output: { option_set_id: String(input.variant).length, confidence: 0.92 },
  });

  it("runs the handler and returns a checked result", async () => {
    const call = activity(ok);
    await expect(call(request({ variant: "M / Sand" }))).resolves.toEqual({
      outcome: "matched",
      output: { option_set_id: 8, confidence: 0.92 },
    });
  });

  it("passes meta through to the handler", async () => {
    let seen: Meta | undefined;
    const call = activity(async (input, _config, m) => {
      seen = m;
      return ok(input, {}, m);
    });
    await call(request({ variant: "x" }));
    expect(seen).toEqual(meta);
  });

  it("rejects invalid input and config without running the handler", async () => {
    let ran = false;
    const call = activity(async () => {
      ran = true;
      return { outcome: "matched", output: {} };
    });
    const input = await failureOf(call(request({ variant: 1 })));
    expect([input.type, input.nonRetryable]).toEqual(["INPUT_INVALID", true]);
    const config = await failureOf(call(request({ variant: "x" }, { model: "a", extra: 1 })));
    expect(config.type).toBe("INPUT_INVALID");
    expect(ran).toBe(false);
  });

  it("rejects undeclared outcomes and invalid output", async () => {
    const undeclared = await failureOf(activity(async () => ({ outcome: "maybe", output: {} }))(request({ variant: "x" })));
    expect([undeclared.type, undeclared.nonRetryable]).toEqual(["OUTPUT_INVALID", true]);
    const invalid = await failureOf(
      activity(async () => ({ outcome: "matched", output: { option_set_id: 1, confidence: 7 } }))(request({ variant: "x" })),
    );
    expect(invalid.type).toBe("OUTPUT_INVALID");
  });

  it("maps FatalError to a non-retryable FATAL and rethrows other errors", async () => {
    const fatal = await failureOf(activity(async () => {
      throw new FatalError("supplier closed");
    })(request({ variant: "x" })));
    expect([fatal.type, fatal.nonRetryable, fatal.message]).toEqual(["FATAL", true, "supplier closed"]);
    const transient = activity(async () => {
      throw new Error("socket hang up");
    });
    await expect(transient(request({ variant: "x" }))).rejects.toThrow("socket hang up");
  });

  it("reports capabilities without a TS handler as UNKNOWN_CAPABILITY", async () => {
    const call = activity(ok);
    const failure = await failureOf(call({ ...request({ order_id: "o", sku: "s" }), capability: "supplier.place_po" }));
    expect([failure.type, failure.nonRetryable]).toEqual(["UNKNOWN_CAPABILITY", true]);
  });

  it("refuses to start on a project that does not validate", () => {
    const files = baseFiles();
    delete files["sidmesh.json"];
    expect(() => createCallActivity(loadProject(writeProject(files)), {})).toThrow(/does not validate/);
  });
});
