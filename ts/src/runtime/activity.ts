import { ApplicationFailure } from "@temporalio/common";
import type { ValidateFunction } from "ajv";
import { capabilityKey } from "../capability.js";
import type { Project } from "../project.js";
import { describeErrors } from "../spec.js";
import { requireValid } from "../validate/index.js";
import type { Catalog } from "../validate/catalog.js";
import { handlerKey } from "../codegen/names.js";

/** Passed to every handler (SPEC §4.1). */
export interface Meta {
  workflowId: string;
  stepId: string;
  attempt: number;
  tenant: string;
  idempotencyKey?: string;
}

export interface CallRequest {
  capability: string;
  version: number;
  input: unknown;
  config: unknown;
  meta: Meta;
}

export interface CallResponse {
  outcome: string;
  output: unknown;
}

// Handlers are typed precisely by the generated contracts.gen.ts; the runtime only needs
// the call shape, so any parameter types are accepted here.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type Handler = (input: any, config: any, meta: Meta) => Promise<CallResponse>;

/** Handlers per node id, each keyed "<verb>@<N>" as exported by the node module. */
export type NodeHandlers = Record<string, Record<string, Handler>>;

/** Throw from a handler to fail the step without retries. */
export class FatalError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "FatalError";
  }
}

interface Compiled {
  handler: Handler;
  input: ValidateFunction;
  config: ValidateFunction;
  outputs: Map<string, ValidateFunction>;
}

const EMPTY_CONFIG = { type: "object", properties: {}, additionalProperties: false };

/**
 * Build the `sidmesh.call` activity for the TS worker. The project must validate cleanly;
 * the wrapper enforces each contract around the handler so handlers never see bad input
 * and never return unchecked output.
 */
export function createCallActivity(
  project: Project,
  handlers: NodeHandlers,
): { "sidmesh.call": (request: CallRequest) => Promise<CallResponse> } {
  return callActivity(requireValid(project).catalog, handlers);
}

/** createCallActivity over the catalog of a project that already passed requireValid. */
export function callActivity(
  catalog: Catalog,
  handlers: NodeHandlers,
): { "sidmesh.call": (request: CallRequest) => Promise<CallResponse> } {
  const compiled = new Map<string, Compiled>();
  for (const [key, entry] of catalog.contracts) {
    const handler = handlers[entry.nodeId]?.[handlerKey(entry.contract.capability, entry.contract.version)];
    if (!handler) {
      continue;
    }
    const { ajv } = catalog;
    const contract = entry.contract;
    compiled.set(key, {
      handler,
      input: ajv.compile(contract.input),
      config: ajv.compile(contract.config ?? EMPTY_CONFIG),
      outputs: new Map(Object.entries(contract.outcomes).map(([name, o]) => [name, ajv.compile(o.output)])),
    });
  }

  const call = async (request: CallRequest): Promise<CallResponse> => {
    const key = capabilityKey(request.capability, request.version);
    const target = compiled.get(key);
    if (!target) {
      throw ApplicationFailure.nonRetryable(`no TS handler for ${key}`, "UNKNOWN_CAPABILITY");
    }
    const config = request.config ?? {};
    if (!target.input(request.input)) {
      throw ApplicationFailure.nonRetryable(`input: ${describeErrors(target.input.errors)}`, "INPUT_INVALID");
    }
    if (!target.config(config)) {
      throw ApplicationFailure.nonRetryable(`config: ${describeErrors(target.config.errors)}`, "INPUT_INVALID");
    }

    let response: CallResponse;
    try {
      response = await target.handler(request.input, config, request.meta);
    } catch (error) {
      if (error instanceof FatalError) {
        throw ApplicationFailure.nonRetryable(error.message, "FATAL");
      }
      throw error; // retryable under the step's retry policy
    }

    const validate = response && target.outputs.get(response.outcome);
    if (!validate) {
      throw ApplicationFailure.nonRetryable(
        `undeclared outcome "${String(response?.outcome)}" from ${key}`,
        "OUTPUT_INVALID",
      );
    }
    if (!validate(response.output)) {
      throw ApplicationFailure.nonRetryable(
        `output of outcome "${response.outcome}": ${describeErrors(validate.errors)}`,
        "OUTPUT_INVALID",
      );
    }
    return { outcome: response.outcome, output: response.output };
  };

  return { "sidmesh.call": call };
}
