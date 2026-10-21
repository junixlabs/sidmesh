import { Client, Connection, type WorkflowExecutionInfo } from "@temporalio/client";
import type { PayloadCodec } from "@temporalio/common";
import {
  HUMAN_UPDATE,
  pinKey,
  prepareStart,
  withDefinitions,
  WORKFLOW_NAME,
  type PinnedDocument,
  type StartInput,
  type StartRequest,
} from "./runtime/prepare.js";
import { PG_URL_ENV, publishDocuments } from "./runtime/registry.js";
import { loadAndPublish, temporalSettings } from "./runtime/settings.js";
import type { Definition } from "./types.js";
import type { Catalog } from "./validate/catalog.js";

export { HUMAN_UPDATE, prepareStart, WORKFLOW_NAME } from "./runtime/prepare.js";
export type { StartInput, StartRequest } from "./runtime/prepare.js";

export interface ConnectOptions {
  /** Temporal frontend; default TEMPORAL_ADDRESS, else localhost:7233. */
  address?: string;
  /** Default TEMPORAL_NAMESPACE, else "default". */
  namespace?: string;
  /** Payload codec; default from SIDMESH_CODEC_KEYS, off when unset. `null` forces it off. */
  codec?: PayloadCodec | null;
  /** Project directory; required to start workflows. Its documents are published to the
   * registry when SIDMESH_PG_URL is set, so what a start pins can be resolved. */
  projectDir?: string;
}

/** Argument of the `human` update (SPEC §5 "Human"). */
export interface Resolution {
  stepId: string;
  outcome: string;
  output?: Record<string, unknown>;
  actor?: string;
}

/** What the interpreter returns when it reaches an end step. */
export interface WorkflowResult {
  status: string;
  output?: Record<string, unknown>;
}

export interface WorkflowSummary {
  workflowId: string;
  runId: string;
  type: string;
  status: string;
  startTime: Date;
  closeTime?: Date;
  searchAttributes: Record<string, unknown>;
}

export class SidmeshClient {
  constructor(
    readonly temporal: Client,
    private readonly connection: Connection,
    private readonly project?: { catalog: Catalog; queues: StartInput["queues"] },
  ) {}

  /**
   * Build the pinned start input without starting anything (what `start` sends). `adHoc`
   * definitions (not in the project, e.g. one built per request) can be started or spawned.
   */
  prepare(req: StartRequest, adHoc: Definition[] = []): StartInput {
    if (!this.project) {
      throw new Error("connect with projectDir to start workflows");
    }
    const { catalog, queues } = this.project;
    return prepareStart(adHoc.length > 0 ? withDefinitions(catalog, adHoc) : catalog, queues, req);
  }

  /**
   * Start an interpreter workflow under the caller's workflow id. A second start with the same
   * id is rejected, so a duplicate source event cannot start a second run.
   */
  async start(req: StartRequest, adHoc: Definition[] = []): Promise<{ workflowId: string; runId: string }> {
    const input = this.prepare(req, adHoc);
    // Project documents went to the registry at connect; ad-hoc ones the run pins go now, so
    // workers can resolve them. The last ad-hoc definition per id@version is the one pinned.
    const url = process.env[PG_URL_ENV];
    if (url) {
      const docs = new Map<string, PinnedDocument>();
      for (const d of adHoc) {
        const key = pinKey("definition", d.id, d.version);
        const hash = input.pins[key];
        if (hash !== undefined) {
          docs.set(key, { hash, kind: "definition", name: d.id, version: d.version, doc: d });
        }
      }
      if (docs.size > 0) {
        await publishDocuments(url, [...docs.values()]);
      }
    }
    const handle = await this.temporal.workflow.start(WORKFLOW_NAME, {
      workflowId: req.workflowId,
      taskQueue: input.queues.workflow,
      args: [input],
      workflowIdReusePolicy: "REJECT_DUPLICATE",
      workflowIdConflictPolicy: "FAIL",
    });
    return { workflowId: handle.workflowId, runId: handle.firstExecutionRunId };
  }

  async signal(workflowId: string, name: string, payload: unknown = {}): Promise<void> {
    await this.temporal.workflow.getHandle(workflowId).signal(name, payload);
  }

  /** Resolve a human step; a rejected resolution throws and the task stays open. */
  async resolve(workflowId: string, resolution: Resolution): Promise<void> {
    const r = { output: {}, actor: "", ...resolution };
    await this.temporal.workflow.getHandle(workflowId).executeUpdate(HUMAN_UPDATE, { args: [r] });
  }

  /** Wait for the workflow (following continue-as-new) and return its end result. */
  async result(workflowId: string): Promise<WorkflowResult> {
    return (await this.temporal.workflow.getHandle(workflowId).result()) as WorkflowResult;
  }

  async cancel(workflowId: string): Promise<void> {
    await this.temporal.workflow.getHandle(workflowId).cancel();
  }

  async terminate(workflowId: string, reason?: string): Promise<void> {
    await this.temporal.workflow.getHandle(workflowId).terminate(reason);
  }

  /** List workflows by a Temporal visibility query, e.g. `OrderId = "123"`. */
  async list(query: string, limit = 100): Promise<WorkflowSummary[]> {
    const out: WorkflowSummary[] = [];
    for await (const info of this.temporal.workflow.list({ query })) {
      out.push(summary(info));
      if (out.length >= limit) {
        break;
      }
    }
    return out;
  }

  async close(): Promise<void> {
    await this.connection.close();
  }
}

function summary(info: WorkflowExecutionInfo): WorkflowSummary {
  // Keyword attributes come back as single-element arrays; unwrap them.
  const searchAttributes: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(info.searchAttributes ?? {})) {
    searchAttributes[key] = Array.isArray(value) && value.length === 1 ? value[0] : value;
  }
  return {
    workflowId: info.workflowId,
    runId: info.runId,
    type: info.type,
    status: info.status.name,
    startTime: info.startTime,
    ...(info.closeTime ? { closeTime: info.closeTime } : {}),
    searchAttributes,
  };
}

/** Connect to Temporal with the SidMesh codec (when configured) and, optionally, a project. */
export async function connect(options: ConnectOptions = {}): Promise<SidmeshClient> {
  let project: { catalog: Catalog; queues: StartInput["queues"] } | undefined;
  if (options.projectDir) {
    const { catalog, config } = await loadAndPublish(options.projectDir);
    project = { catalog, queues: config.queues };
  }
  const { address, namespace, codec } = temporalSettings(options);
  const connection = await Connection.connect({ address });
  const temporal = new Client({
    connection,
    namespace,
    ...(codec ? { dataConverter: { payloadCodecs: [codec] } } : {}),
  });
  return new SidmeshClient(temporal, connection, project);
}
