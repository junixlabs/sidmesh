import type { PayloadCodec } from "@temporalio/common";
import { NativeConnection, Worker } from "@temporalio/worker";
import { callActivity, type NodeHandlers } from "./activity.js";
import { loadAndPublish, temporalSettings } from "./settings.js";

export interface RunWorkerOptions {
  projectDir: string;
  handlers: NodeHandlers;
  /** Temporal frontend; default TEMPORAL_ADDRESS, else localhost:7233. */
  address?: string;
  /** Default TEMPORAL_NAMESPACE, else "default". */
  namespace?: string;
  /** Payload codec; default from SIDMESH_CODEC_KEYS, off when unset. `null` forces it off. */
  codec?: PayloadCodec | null;
}

/** Run the TS activity worker on the project's `queues.ts` task queue until shutdown. */
export async function runWorker(options: RunWorkerOptions): Promise<void> {
  const { catalog, config } = await loadAndPublish(options.projectDir);
  const { address, namespace, codec } = temporalSettings(options);
  const connection = await NativeConnection.connect({ address });
  try {
    const worker = await Worker.create({
      connection,
      namespace,
      taskQueue: config.queues.ts,
      activities: callActivity(catalog, options.handlers),
      ...(codec ? { dataConverter: { payloadCodecs: [codec] } } : {}),
    });
    await worker.run();
  } finally {
    await connection.close();
  }
}
