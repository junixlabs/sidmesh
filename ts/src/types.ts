// Shapes of the documents after they have passed their JSON Schema. Semantic code only ever
// sees documents that passed shape validation, so these casts are safe there.

export type JsonSchema = Record<string, unknown>;

export interface NodeManifest {
  id: string;
  responsibility: string;
  notOwned?: string[];
  runtime: "go" | "ts";
  permissions?: string[];
}

export interface CapabilityRef {
  capability: string;
  version: number;
}

export interface Outcome {
  description: string;
  output: JsonSchema;
}

export interface RetryPolicy {
  maxAttempts: number;
  initialInterval?: string;
  backoff?: number;
}

export interface Contract {
  capability: string;
  version: number;
  kind: "action" | "ai";
  effects: "none" | "write";
  summary: string;
  config?: JsonSchema;
  input: JsonSchema;
  outcomes: Record<string, Outcome>;
  idempotency?: { key: string[] };
  lookup?: CapabilityRef;
  ledger?: string[];
  defaults?: { timeout?: string; retry?: RetryPolicy };
}

export type Binding = string | { const: unknown } | { expr: string };
export type Bindings = Record<string, Binding>;

export interface Sla {
  after: string;
  alert?: string;
}

export interface CallStep {
  type: "call";
  description?: string;
  capability: string;
  version: number;
  input: Bindings;
  config?: Bindings;
  timeout?: string;
  retry?: RetryPolicy;
  sla?: Sla;
  gate?: { threshold: Binding; below: string };
  compensate?: { capability: string; version: number; input: Bindings; config?: Bindings };
  on: Record<string, string>;
}

export interface DecideStep {
  type: "decide";
  description?: string;
  cases: { when: string; next: string }[];
  else: string;
}

export interface SetStep {
  type: "set";
  description?: string;
  values: Bindings;
  next: string;
}

export interface WaitStep {
  type: "wait";
  description?: string;
  signal?: string;
  duration?: Binding;
  timeout?: Binding;
  sla?: Sla;
  collect?: { window: Binding; max?: number | Binding };
  on: { received?: string; elapsed?: string; timeout?: string };
}

export interface HumanStep {
  type: "human";
  description?: string;
  task: string;
  input: Bindings;
  outcomes: Record<string, JsonSchema>;
  sla?: Sla;
  on: Record<string, string>;
}

export interface SpawnStep {
  type: "spawn";
  description?: string;
  definition: string;
  version: number;
  foreach?: Binding;
  key: Binding;
  input: Bindings;
  params?: Bindings;
  wait: "all" | "none";
  next: string;
  onChildFailed?: string;
}

export interface SignalStep {
  type: "signal";
  description?: string;
  to: "parent" | { workflow: Binding } | { children: string };
  name: string;
  payload?: Bindings;
  next: string;
  start?: {
    definition: string;
    version: number;
    input: Bindings;
    params?: Bindings;
    /** true: never start an id that already ran and closed (RejectDuplicate). Default false. */
    once?: boolean;
  };
}

export interface CompensateStep {
  type: "compensate";
  description?: string;
  next: string;
}

export interface EndStep {
  type: "end";
  description?: string;
  status: "completed" | "failed" | "cancelled";
  output?: Bindings;
  /** Signals whose queued payloads continue the workflow as new instead of ending it. */
  drain?: string[];
}

export type Step =
  | CallStep
  | DecideStep
  | SetStep
  | WaitStep
  | HumanStep
  | SpawnStep
  | SignalStep
  | CompensateStep
  | EndStep;

export interface Param {
  description?: string;
  schema: JsonSchema;
  default: unknown;
}

export interface Var {
  description?: string;
  schema: JsonSchema;
  default: unknown;
}

export interface SignalDecl {
  description?: string;
  schema: JsonSchema;
  goto?: string;
}

export interface Definition {
  id: string;
  version: number;
  description: string;
  input: JsonSchema;
  params?: Record<string, Param>;
  vars?: Record<string, Var>;
  signals?: Record<string, SignalDecl>;
  search?: Record<string, Binding>;
  start: string;
  steps: Record<string, Step>;
}
