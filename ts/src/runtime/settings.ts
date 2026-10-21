import type { PayloadCodec } from "@temporalio/common";
import { loadProject } from "../project.js";
import { requireValid } from "../validate/index.js";
import { codecFromEnv } from "./codec.js";
import { projectDocuments } from "./prepare.js";
import { publishFromEnv } from "./registry.js";

/** Validate a project for running and publish its documents (SPEC §5); throws when it does not validate. */
export async function loadAndPublish(projectDir: string): Promise<ReturnType<typeof requireValid>> {
  const result = requireValid(loadProject(projectDir));
  await publishFromEnv(projectDocuments(result.catalog));
  return result;
}

/** Temporal connection settings: explicit options, else TEMPORAL_ADDRESS / TEMPORAL_NAMESPACE / SIDMESH_CODEC_KEYS. */
export function temporalSettings(options: { address?: string; namespace?: string; codec?: PayloadCodec | null }): {
  address: string;
  namespace: string;
  codec: PayloadCodec | null | undefined;
} {
  return {
    address: options.address || process.env.TEMPORAL_ADDRESS || "localhost:7233",
    namespace: options.namespace || process.env.TEMPORAL_NAMESPACE || "default",
    codec: options.codec === undefined ? codecFromEnv() : options.codec,
  };
}
