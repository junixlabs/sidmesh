import pg from "pg";
import { describe, expect, it } from "vitest";
import { loadProject } from "../src/project.js";
import { SidmeshClient } from "../src/client.js";
import { documentHash, projectDocuments } from "../src/runtime/prepare.js";
import type { Definition } from "../src/types.js";
import { publishDocuments } from "../src/runtime/registry.js";
import { validateProject } from "../src/validate/index.js";
import { baseFiles, writeProject } from "./fixture.js";

// Needs a throwaway database: SIDMESH_TEST_PG_URL (same variable as the Go sink tests).
const url = process.env.SIDMESH_TEST_PG_URL;

describe.skipIf(!url)("document registry (sidmesh_documents)", () => {
  it("inserts each document once and never updates", async () => {
    const docs = projectDocuments(validateProject(loadProject(writeProject(baseFiles()))).catalog);
    const first = await publishDocuments(url!, docs);
    expect(await publishDocuments(url!, docs)).toBe(0);
    const client = new pg.Client({ connectionString: url });
    await client.connect();
    try {
      const { rows } = await client.query("select hash, kind, name, version, doc from sidmesh_documents where hash = any($1)", [docs.map((d) => d.hash)]);
      expect(rows).toHaveLength(docs.length);
      expect(first).toBeLessThanOrEqual(docs.length);
      const order = docs.find((d) => d.name === "order")!;
      expect(rows.find((r) => r.hash === order.hash)).toMatchObject({ kind: "definition", name: "order", version: 1, doc: order.doc });
    } finally {
      await client.end();
    }
  });

  it("client.start publishes the ad-hoc definitions it pins before starting", async () => {
    const project = loadProject(writeProject(baseFiles()));
    const { catalog } = validateProject(project);
    const adHoc: Definition = {
      id: "adhoc_lookup", version: 1, description: "one step", input: { type: "object", properties: {}, additionalProperties: false },
      start: "end", steps: { end: { type: "end", status: "completed" } },
    };
    let startedWith: unknown;
    const temporal = { workflow: { start: async (_: string, o: { args: unknown[] }) => ((startedWith = o.args[0]), { workflowId: "w", firstExecutionRunId: "r" }) } };
    const sm = new SidmeshClient(temporal as never, {} as never, { catalog, queues: project.config!.queues });
    process.env.SIDMESH_PG_URL = url;
    try {
      await sm.start({ definition: "adhoc_lookup", version: 1, workflowId: "w", tenant: "t", input: {} }, [adHoc]);
    } finally {
      delete process.env.SIDMESH_PG_URL;
    }
    expect((startedWith as { pins: Record<string, string> }).pins["definition:adhoc_lookup@1"]).toBe(documentHash(adHoc));
    const client = new pg.Client({ connectionString: url });
    await client.connect();
    try {
      const { rows } = await client.query("select name from sidmesh_documents where hash = $1", [documentHash(adHoc)]);
      expect(rows).toEqual([{ name: "adhoc_lookup" }]);
    } finally {
      await client.end();
    }
  });
});
