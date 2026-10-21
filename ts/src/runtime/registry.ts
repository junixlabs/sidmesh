import pg from "pg";
import type { PinnedDocument } from "./prepare.js";

export const PG_URL_ENV = "SIDMESH_PG_URL";

// Same DDL as the reference Postgres sink (SPEC §6.1); idempotent, so either side may run first.
const DDL = `
create table if not exists sidmesh_documents (
  hash text primary key,
  kind text not null,
  name text not null,
  version int not null,
  doc jsonb not null,
  created_at timestamptz not null default now()
);
create index if not exists sidmesh_documents_kind_name_version on sidmesh_documents (kind, name, version);`;

// URLs whose DDL already ran in this process; it is idempotent, so a race only repeats it.
const migrated = new Set<string>();

/** Insert the documents the registry does not have yet; never updates. Returns how many were new. */
export async function publishDocuments(url: string, docs: PinnedDocument[]): Promise<number> {
  const client = new pg.Client({ connectionString: url });
  await client.connect();
  try {
    if (!migrated.has(url)) {
      await client.query(DDL);
      migrated.add(url);
    }
    // One round trip for every document; a hash already stored (or repeated in docs) is skipped.
    const res = await client.query(
      `insert into sidmesh_documents (hash, kind, name, version, doc)
       select * from unnest($1::text[], $2::text[], $3::text[], $4::int[], $5::jsonb[])
       on conflict (hash) do nothing`,
      [
        docs.map((d) => d.hash),
        docs.map((d) => d.kind),
        docs.map((d) => d.name),
        docs.map((d) => d.version),
        docs.map((d) => JSON.stringify(d.doc)),
      ],
    );
    return res.rowCount ?? 0;
  } finally {
    await client.end();
  }
}

/** Publish documents when SIDMESH_PG_URL is set (SPEC §5); a no-op otherwise. */
export async function publishFromEnv(docs: PinnedDocument[]): Promise<void> {
  const url = process.env[PG_URL_ENV];
  if (url && docs.length > 0) {
    await publishDocuments(url, docs);
  }
}
