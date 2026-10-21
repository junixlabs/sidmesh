-- SPEC §6.1. Idempotent: run on every start.
-- record_key (not part of the contract) makes a retried sink activity write a row once.
create table if not exists sidmesh_events (
  id          bigserial primary key,
  time        timestamptz not null,
  tenant      text not null,
  workflow_id text not null,
  parent_id   text null,
  definition  text not null,
  version     int not null,
  kind        text not null,
  step_id     text null,
  step_type   text null,
  outcome     text null,
  started_at  timestamptz null,
  ended_at    timestamptz null,
  status      text null,
  record_key  text unique
);
create index if not exists sidmesh_events_workflow on sidmesh_events (workflow_id, id);
create index if not exists sidmesh_events_tenant on sidmesh_events (tenant, id);

create table if not exists sidmesh_ledger (
  id          bigserial primary key,
  time        timestamptz not null,
  tenant      text not null,
  workflow_id text not null,
  step_id     text not null,
  capability  text not null,
  version     int not null,
  outcome     text not null,
  confidence  double precision null,
  gated       boolean not null,
  fields      jsonb not null default '{}',
  record_key  text unique
);
create index if not exists sidmesh_ledger_capability on sidmesh_ledger (capability, version, time);
create index if not exists sidmesh_ledger_workflow on sidmesh_ledger (workflow_id);

create table if not exists sidmesh_tasks (
  task_id     text primary key,
  tenant      text not null,
  workflow_id text not null,
  step_id     text not null,
  task        text not null,
  status      text not null,
  input       jsonb,
  outcomes    jsonb,
  opened_at   timestamptz not null,
  resolved_at timestamptz null,
  outcome     text null,
  output      jsonb null,
  actor       text null
);
create index if not exists sidmesh_tasks_open on sidmesh_tasks (tenant, status, opened_at);

create table if not exists sidmesh_alerts (
  id          bigserial primary key,
  time        timestamptz not null,
  tenant      text not null,
  workflow_id text not null,
  step_id     text null,
  kind        text not null,
  alert       text null,
  dedupe_key  text unique not null,
  message     text not null,
  resolved_at timestamptz null
);

-- Tenant params (SPEC §5): written by the application, read by the engine at run start.
create table if not exists sidmesh_params (
  tenant      text not null,
  definition  text not null,
  version     int not null,
  params      jsonb not null default '{}',
  updated_at  timestamptz not null default now(),
  primary key (tenant, definition, version)
);

-- Pinned documents (SPEC §5 "Version pinning"): content-addressed, insert-only.
create table if not exists sidmesh_documents (
  hash        text primary key,
  kind        text not null,
  name        text not null,
  version     int not null,
  doc         jsonb not null,
  created_at  timestamptz not null default now()
);
create index if not exists sidmesh_documents_kind_name_version on sidmesh_documents (kind, name, version);
