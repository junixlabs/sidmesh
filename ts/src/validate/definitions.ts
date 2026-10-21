import { capabilityKey } from "../capability.js";
import { analyzeCel, CEL_ROOTS } from "../cel.js";
import { canReach, dominators, reachable, type Graph } from "../graph.js";
import { pointer, type IssueList } from "../issues.js";
import { isRecord, resolvePath } from "../schema.js";
import type {
  Binding,
  Bindings,
  CallStep,
  CapabilityRef,
  Definition,
  HumanStep,
  JsonSchema,
  SignalStep,
  SpawnStep,
  Step,
  WaitStep,
} from "../types.js";
import type { Catalog, CatalogContract } from "./catalog.js";
import { checkUserSchema } from "./schemas.js";

interface Edge {
  target: string;
  at: string;
}

/** Every step id a step names, with the pointer of the field that names it. */
function stepEdges(id: string, step: Step): Edge[] {
  const base = (...keys: (string | number)[]) => pointer("steps", id, ...keys);
  switch (step.type) {
    case "call": {
      const edges = Object.entries(step.on).map(([o, t]) => ({ target: t, at: base("on", o) }));
      if (step.gate) {
        edges.push({ target: step.gate.below, at: base("gate", "below") });
      }
      return edges;
    }
    case "decide":
      return [
        ...step.cases.map((c, i) => ({ target: c.next, at: base("cases", i, "next") })),
        { target: step.else, at: base("else") },
      ];
    case "wait":
      return Object.entries(step.on)
        .filter((entry): entry is [string, string] => typeof entry[1] === "string")
        .map(([o, t]) => ({ target: t, at: base("on", o) }));
    case "human":
      return Object.entries(step.on).map(([o, t]) => ({ target: t, at: base("on", o) }));
    case "spawn": {
      const edges = [{ target: step.next, at: base("next") }];
      if (step.onChildFailed) {
        edges.push({ target: step.onChildFailed, at: base("onChildFailed") });
      }
      return edges;
    }
    case "set":
    case "signal":
    case "compensate":
      return [{ target: step.next, at: base("next") }];
    case "end":
      return [];
  }
}

const SPAWN_ALL_OUTPUT: JsonSchema = {
  type: "object",
  properties: {
    children: {
      type: "array",
      items: {
        type: "object",
        properties: { key: {}, status: { type: "string" }, output: {} },
      },
    },
  },
};
const SPAWN_NONE_OUTPUT: JsonSchema = {
  type: "object",
  properties: { count: { type: "integer" } },
};
const EMPTY_OUTPUT: JsonSchema = { type: "object", properties: {} };
const TIME: JsonSchema = { type: "string", format: "date-time" };

const DURATION = /^[1-9][0-9]*(ms|s|m|h|d)$/;

interface Where {
  /** Step that uses the binding; undefined for definition-level bindings (search). */
  stepId?: string;
  at: string;
  allowItem: boolean;
  itemSchema?: JsonSchema;
  /** A compensation may read the output of the step it undoes. */
  selfOk?: boolean;
}

class DefinitionChecker {
  private dom!: Map<string, Set<string>>;

  constructor(
    private readonly def: Definition,
    private readonly file: string,
    private readonly catalog: Catalog,
    private readonly issues: IssueList,
  ) {}

  run(): void {
    this.checkSchemas();
    this.checkParams();
    const { entries, graph } = this.checkGraph();
    this.dom = dominators(graph, entries);
    for (const [id, step] of Object.entries(this.def.steps)) {
      this.checkStep(id, step);
    }
    for (const [name, binding] of Object.entries(this.def.search ?? {})) {
      this.checkBinding(binding, { at: pointer("search", name), allowItem: false });
    }
  }

  private add(code: string, at: string, message: string): void {
    this.issues.add(code, this.file, at, message);
  }

  // --- schemas and params -------------------------------------------------------------

  private checkSchemas(): void {
    const { ajv } = this.catalog;
    checkUserSchema(ajv, this.def.input, this.file, "/input", "DEF", this.issues);
    for (const [name, signal] of Object.entries(this.def.signals ?? {})) {
      checkUserSchema(ajv, signal.schema, this.file, pointer("signals", name, "schema"), "DEF", this.issues);
    }
    for (const [id, step] of Object.entries(this.def.steps)) {
      if (step.type === "human") {
        for (const [name, schema] of Object.entries(step.outcomes)) {
          checkUserSchema(ajv, schema, this.file, pointer("steps", id, "outcomes", name), "DEF", this.issues);
        }
      }
    }
  }

  /** Param and var defaults must validate against their own schema. */
  private checkParams(): void {
    const groups = [
      ["params", this.def.params, "DEF_PARAM_DEFAULT"],
      ["vars", this.def.vars, "DEF_VAR_DEFAULT"],
    ] as const;
    for (const [root, entries, code] of groups) {
      for (const [name, entry] of Object.entries(entries ?? {})) {
        let validate;
        try {
          validate = this.catalog.ajv.compile(entry.schema);
        } catch (error) {
          this.add("DEF_SCHEMA_INVALID", pointer(root, name, "schema"), (error as Error).message);
          continue;
        }
        if (!validate(entry.default)) {
          this.add(code, pointer(root, name, "default"), `default does not match the ${root} schema`);
        }
      }
    }
  }

  // --- graph --------------------------------------------------------------------------

  private graph(): Graph {
    const graph: Graph = new Map();
    for (const [id, step] of Object.entries(this.def.steps)) {
      graph.set(
        id,
        stepEdges(id, step).map((e) => e.target).filter((t) => t in this.def.steps),
      );
    }
    return graph;
  }

  /** Report unknown targets, unreachable steps and dead ends. Returns the entry points and the graph. */
  private checkGraph(): { entries: string[]; graph: Graph } {
    const steps = this.def.steps;
    const entries: string[] = [];
    if (this.def.start in steps) {
      entries.push(this.def.start);
    } else {
      this.add("DEF_STEP_UNKNOWN", "/start", `start step "${this.def.start}" does not exist`);
    }
    for (const [name, signal] of Object.entries(this.def.signals ?? {})) {
      if (signal.goto === undefined) {
        continue;
      }
      if (signal.goto in steps) {
        entries.push(signal.goto);
      } else {
        this.add("DEF_STEP_UNKNOWN", pointer("signals", name, "goto"), `step "${signal.goto}" does not exist`);
      }
    }
    for (const [id, step] of Object.entries(steps)) {
      for (const edge of stepEdges(id, step)) {
        if (!(edge.target in steps)) {
          this.add("DEF_STEP_UNKNOWN", edge.at, `step "${edge.target}" does not exist`);
        }
      }
    }

    const graph = this.graph();
    const live = reachable(graph, entries);
    // With an unknown start every step looks unreachable; that cascade is suppressed.
    const startKnown = this.def.start in steps;
    const ends = Object.entries(steps)
      .filter(([, step]) => step.type === "end")
      .map(([id]) => id);
    const finishing = canReach(graph, ends);
    for (const id of Object.keys(steps)) {
      if (startKnown && !live.has(id)) {
        this.add("DEF_UNREACHABLE", pointer("steps", id), `step "${id}" is never reached`);
      }
      if (!finishing.has(id)) {
        this.add("DEF_DEAD_END", pointer("steps", id), `step "${id}" has no path to an end step`);
      }
    }
    return { entries, graph };
  }

  // --- steps --------------------------------------------------------------------------

  private checkStep(id: string, step: Step): void {
    const at = (...keys: (string | number)[]) => pointer("steps", id, ...keys);
    const where = (ptr: string, extra: Partial<Where> = {}): Where => ({
      stepId: id,
      at: ptr,
      allowItem: false,
      ...extra,
    });
    switch (step.type) {
      case "call":
        this.checkCall(step, at, where);
        return;
      case "decide":
        step.cases.forEach((c, i) => this.checkExpr(c.when, where(at("cases", i, "when"))));
        return;
      case "set":
        for (const [name, binding] of Object.entries(step.values)) {
          if (!(name in (this.def.vars ?? {}))) {
            this.add("DEF_VAR_UNKNOWN", at("values", name), `var "${name}" is not declared`);
          }
          this.checkBinding(binding, where(at("values", name)));
        }
        return;
      case "wait":
        this.checkWait(step, at, where);
        return;
      case "human":
        this.checkHuman(step, at, where);
        return;
      case "spawn":
        this.checkSpawn(step, at, where);
        return;
      case "signal":
        this.checkSignal(step, at, where);
        return;
      case "compensate":
        return;
      case "end":
        this.checkBindings(step.output, (k) => where(at("output", k)));
        step.drain?.forEach((name, i) => {
          if (!(name in (this.def.signals ?? {}))) {
            this.add("DEF_DRAIN_UNKNOWN", at("drain", i), `drain names undeclared signal "${name}"`);
          }
        });
        return;
    }
  }

  private checkCall(
    step: CallStep,
    at: (...keys: (string | number)[]) => string,
    where: (ptr: string) => Where,
  ): void {
    const entry = this.contractAt(step, at());
    if (entry) {
      const contract = entry.contract;
      const routable = new Set([...Object.keys(contract.outcomes), "failed"]);
      if (contract.effects === "write") {
        routable.add("unknown");
      }
      for (const outcome of routable) {
        if (!(outcome in step.on)) {
          this.add("DEF_OUTCOME_UNROUTED", at("on"), `outcome "${outcome}" is not routed`);
        }
      }
      for (const outcome of Object.keys(step.on)) {
        if (!routable.has(outcome)) {
          this.add("DEF_OUTCOME_EXTRA", at("on", outcome), `${step.capability} has no outcome "${outcome}"`);
        }
      }
      if (step.gate && contract.kind !== "ai") {
        this.add("DEF_GATE_NOT_AI", at("gate"), "gate is only allowed on ai capabilities");
      }
      this.checkKeys(step.input, contract.input, at("input"));
      this.checkKeys(step.config ?? {}, contract.config ?? { properties: {} }, at("config"));
    }
    this.checkBindings(step.input, (k) => where(at("input", k)));
    this.checkBindings(step.config, (k) => where(at("config", k)));
    if (step.gate) {
      this.checkBinding(step.gate.threshold, where(at("gate", "threshold")));
    }
    if (step.compensate) {
      const comp = step.compensate;
      const target = this.contractAt(comp, at());
      if (target) {
        this.checkKeys(comp.input, target.contract.input, at("compensate", "input"));
        this.checkKeys(comp.config ?? {}, target.contract.config ?? { properties: {} }, at("compensate", "config"));
      }
      this.checkBindings(comp.input, (k) => ({ ...where(at("compensate", "input", k)), selfOk: true }));
      this.checkBindings(comp.config, (k) => ({ ...where(at("compensate", "config", k)), selfOk: true }));
    }
  }

  private checkWait(step: WaitStep, at: (...keys: (string | number)[]) => string, where: (ptr: string) => Where): void {
    const hasSignal = step.signal !== undefined;
    const hasDuration = step.duration !== undefined;
    if (hasSignal === hasDuration) {
      this.add("DEF_WAIT_SHAPE", at(), "a wait needs exactly one of signal or duration");
    }
    if (hasSignal && step.on.received === undefined) {
      this.add("DEF_WAIT_SHAPE", at(), "a signal wait needs on.received");
    }
    if (hasDuration && step.on.elapsed === undefined) {
      this.add("DEF_WAIT_SHAPE", at(), "a duration wait needs on.elapsed");
    }
    if ((step.timeout !== undefined) !== (step.on.timeout !== undefined)) {
      this.add("DEF_WAIT_SHAPE", at(), "timeout and on.timeout go together");
    }
    if (step.collect !== undefined && !hasSignal) {
      this.add("DEF_WAIT_SHAPE", at(), "collect needs a signal");
    }
    this.checkDurationBinding(step.duration, where(at("duration")));
    this.checkDurationBinding(step.timeout, where(at("timeout")));
    this.checkDurationBinding(step.collect?.window, where(at("collect", "window")));
    if (step.collect?.max !== undefined && typeof step.collect.max !== "number") {
      this.checkBinding(step.collect.max, where(at("collect", "max")));
    }
    if (hasSignal && !(step.signal! in (this.def.signals ?? {}))) {
      this.add("DEF_SIGNAL_UNKNOWN", at("signal"), `signal "${step.signal}" is not declared`);
    }
  }

  private checkHuman(step: HumanStep, at: (...keys: (string | number)[]) => string, where: (ptr: string) => Where): void {
    for (const outcome of Object.keys(step.outcomes)) {
      if (!(outcome in step.on)) {
        this.add("DEF_HUMAN_UNROUTED", at("on"), `outcome "${outcome}" is not routed`);
      }
    }
    for (const outcome of Object.keys(step.on)) {
      if (!(outcome in step.outcomes)) {
        this.add("DEF_HUMAN_UNROUTED", at("on"), `"${outcome}" is not a declared outcome`);
      }
    }
    this.checkBindings(step.input, (k) => where(at("input", k)));
  }

  /** The called contract, or undefined after reporting it unknown (unless it only failed its shape). */
  private contractAt(ref: CapabilityRef, stepPtr: string): CatalogContract | undefined {
    const key = capabilityKey(ref.capability, ref.version);
    const entry = this.catalog.contracts.get(key);
    if (!entry && !this.catalog.shapeInvalid.contracts.has(key)) {
      this.add("DEF_CAPABILITY_UNKNOWN", stepPtr, `${key} does not exist`);
    }
    return entry;
  }

  /** A spawn's or signal start's target definition: exists, and input/params match it. */
  private checkStartTarget(
    ref: { definition: string; version: number; input: Bindings; params?: Bindings },
    stepPtr: string,
    inputPtr: string,
    paramsPtr: string,
  ): void {
    const key = capabilityKey(ref.definition, ref.version);
    const target = this.catalog.definitions.get(key);
    if (!target) {
      if (!this.catalog.shapeInvalid.definitions.has(key)) {
        this.add("DEF_SPAWN_UNKNOWN", stepPtr, `${key} does not exist`);
      }
      return;
    }
    this.checkKeys(ref.input, target.definition.input, inputPtr);
    this.checkParamKeys(ref.params, target.definition, paramsPtr);
  }

  private checkSpawn(
    step: SpawnStep,
    at: (...keys: (string | number)[]) => string,
    where: (ptr: string, extra?: Partial<Where>) => Where,
  ): void {
    this.checkStartTarget(step, at(), at("input"), at("params"));
    this.checkBindings(step.params, (k) => where(at("params", k)));
    let itemSchema: JsonSchema | undefined;
    if (step.foreach !== undefined) {
      const listSchema = this.checkBinding(step.foreach, where(at("foreach")));
      if (listSchema && isRecord(listSchema.items)) {
        itemSchema = listSchema.items as JsonSchema;
      }
    }
    const withItem = { allowItem: true, itemSchema };
    this.checkBinding(step.key, where(at("key"), withItem));
    this.checkBindings(step.input, (k) => where(at("input", k), withItem));
  }

  private checkSignal(
    step: SignalStep,
    at: (...keys: (string | number)[]) => string,
    where: (ptr: string) => Where,
  ): void {
    const to = step.to;
    if (to !== "parent" && "workflow" in to) {
      this.checkBinding(to.workflow, where(at("to", "workflow")));
    }
    if (to !== "parent" && "children" in to && this.def.steps[to.children]?.type !== "spawn") {
      this.add("DEF_SIGNAL_TARGET", at(), `to.children "${to.children}" is not a spawn step`);
    }
    if (step.start) {
      if (to === "parent" || !("workflow" in to)) {
        this.add("DEF_SIGNAL_TARGET", at(), "start needs to.workflow");
      }
      const start = step.start;
      this.checkStartTarget(start, at(), at("start", "input"), at("start", "params"));
      this.checkBindings(start.input, (k) => where(at("start", "input", k)));
      this.checkBindings(start.params, (k) => where(at("start", "params", k)));
    }
    this.checkBindings(step.payload, (k) => where(at("payload", k)));
  }

  /** Spawn and signal-start params must name params of the target definition. */
  private checkParamKeys(bindings: Bindings | undefined, target: Definition, at: string): void {
    for (const key of Object.keys(bindings ?? {})) {
      if (!(key in (target.params ?? {}))) {
        this.add("DEF_PARAM_UNKNOWN", `${at}${pointer(key)}`, `${target.id} has no param "${key}"`);
      }
    }
  }

  /** A duration binding: a constant must be a duration string; otherwise it is checked as a binding. */
  private checkDurationBinding(binding: Binding | undefined, where: Where): void {
    if (binding === undefined) {
      return;
    }
    if (typeof binding !== "string" && "const" in binding) {
      if (typeof binding.const !== "string" || !DURATION.test(binding.const)) {
        this.add("DEF_DURATION_INVALID", where.at, `${JSON.stringify(binding.const)} is not a duration like 30s or 7d`);
      }
      return;
    }
    this.checkBinding(binding, where);
  }

  /** DEF_INPUT_MISSING / DEF_INPUT_EXTRA for a bindings object against a target schema. */
  private checkKeys(bindings: Bindings, schema: JsonSchema, at: string): void {
    const props = isRecord(schema.properties) ? schema.properties : {};
    const required = Array.isArray(schema.required) ? (schema.required as string[]) : [];
    for (const key of Object.keys(bindings)) {
      if (!(key in props)) {
        this.add("DEF_INPUT_EXTRA", `${at}${pointer(key)}`, `"${key}" is not a property of the target input`);
      }
    }
    for (const key of required) {
      if (!(key in bindings)) {
        this.add("DEF_INPUT_MISSING", at, `required "${key}" is not bound`);
      }
    }
  }

  // --- bindings -----------------------------------------------------------------------

  private checkBindings(bindings: Bindings | undefined, where: (key: string) => Where): void {
    for (const [key, binding] of Object.entries(bindings ?? {})) {
      this.checkBinding(binding, where(key));
    }
  }

  /** Check one binding. Returns the schema a path binding resolves to, when known. */
  private checkBinding(binding: Binding, where: Where): JsonSchema | undefined {
    if (typeof binding === "string") {
      return this.checkPath(binding.split("."), where);
    }
    if ("expr" in binding) {
      this.checkExpr(binding.expr, { ...where, at: `${where.at}/expr` });
    }
    return undefined;
  }

  private checkExpr(source: string, where: Where): void {
    const analysis = analyzeCel(source);
    if (analysis.error) {
      this.add("DEF_EXPR_INVALID", where.at, `expression does not parse: ${analysis.error}`);
      return;
    }
    const roots: readonly string[] = CEL_ROOTS;
    for (const name of analysis.identifiers) {
      if (!roots.includes(name)) {
        this.add("DEF_EXPR_INVALID", where.at, `unknown variable "${name}"`);
      } else if (name === "item" && !where.allowItem) {
        this.add("DEF_BINDING_ROOT", where.at, "item is only available in a spawn's input and key");
      }
    }
    for (const path of analysis.paths) {
      const segments = path.split(".");
      if (roots.includes(segments[0] as string) && !(segments[0] === "item" && !where.allowItem)) {
        this.checkPath(segments, where);
      }
    }
  }

  private checkPath(segments: string[], where: Where): JsonSchema | undefined {
    const [root, name, ...rest] = segments;
    const text = segments.join(".");
    const resolveIn = (schema: JsonSchema, path: string[]): JsonSchema | undefined => {
      const found = resolvePath(schema, path);
      if (!found) {
        this.add("DEF_BINDING_PATH", where.at, `${text} does not resolve`);
      }
      return found;
    };

    switch (root) {
      case "input":
        return resolveIn(this.def.input, segments.slice(1));
      case "params": {
        const param = this.def.params?.[name as string];
        if (!param) {
          this.add("DEF_PARAM_UNKNOWN", where.at, `param "${name}" is not declared`);
          return undefined;
        }
        return resolveIn(param.schema, rest);
      }
      case "vars": {
        // Vars always hold a value (their default), so they need no dominance check.
        const variable = this.def.vars?.[name as string];
        if (!variable) {
          this.add("DEF_VAR_UNKNOWN", where.at, `var "${name}" is not declared`);
          return undefined;
        }
        return resolveIn(variable.schema, rest);
      }
      case "signals": {
        const signal = this.def.signals?.[name as string];
        if (!signal) {
          this.add("DEF_SIGNAL_UNKNOWN", where.at, `signal "${name}" is not declared`);
          return undefined;
        }
        return resolveIn(signal.schema, rest);
      }
      case "steps":
        return this.checkStepPath(name as string, rest, text, where);
      case "item":
        if (!where.allowItem) {
          this.add("DEF_BINDING_ROOT", where.at, "item is only available in a spawn's input and key");
          return undefined;
        }
        return where.itemSchema ? resolveIn(where.itemSchema, segments.slice(1)) : undefined;
      case "workflow":
        if (segments.length !== 2 || !["id", "parent_id", "tenant", "now"].includes(name as string)) {
          this.add("DEF_BINDING_PATH", where.at, `${text} does not resolve; use workflow.id, parent_id, tenant or now`);
          return undefined;
        }
        return name === "now" ? TIME : { type: "string" };
      default:
        this.add("DEF_BINDING_ROOT", where.at, `unknown root "${root}"`);
        return undefined;
    }
  }

  private checkStepPath(
    stepId: string,
    rest: string[],
    text: string,
    where: Where,
  ): JsonSchema | undefined {
    const step = this.def.steps[stepId];
    if (!step) {
      this.add("DEF_BINDING_STEP", where.at, `step "${stepId}" does not exist`);
      return undefined;
    }
    if (where.stepId !== undefined) {
      const dom = this.dom.get(where.stepId);
      const isSelf = stepId === where.stepId;
      // Unreachable steps have no dominators; they are already reported.
      if (dom && ((isSelf && !where.selfOk) || (!isSelf && !dom.has(stepId)))) {
        this.add(
          "DEF_BINDING_STEP",
          where.at,
          `step "${stepId}" does not run before step "${where.stepId}" on every path`,
        );
      }
    }
    const [field, ...path] = rest;
    if (field === undefined) {
      return undefined;
    }
    if (field === "outcome" || field === "started_at" || field === "ended_at") {
      if (path.length > 0) {
        this.add("DEF_BINDING_PATH", where.at, `${text} does not resolve`);
        return undefined;
      }
      return field === "outcome" ? { type: "string" } : TIME;
    }
    if (field !== "output") {
      this.add("DEF_BINDING_PATH", where.at, `${text} does not resolve; use .outcome, .output, .started_at or .ended_at`);
      return undefined;
    }
    if (path.length === 0) {
      return undefined;
    }
    if (step.type === "call" && !this.catalog.contracts.has(capabilityKey(step.capability, step.version))) {
      return undefined; // DEF_CAPABILITY_UNKNOWN already covers it
    }
    for (const schema of this.outputSchemas(step)) {
      const found = resolvePath(schema, path);
      if (found) {
        return found;
      }
    }
    this.add("DEF_BINDING_PATH", where.at, `${text} does not resolve in any output of step "${stepId}"`);
    return undefined;
  }

  private outputSchemas(step: Step): JsonSchema[] {
    switch (step.type) {
      case "call": {
        const entry = this.catalog.contracts.get(capabilityKey(step.capability, step.version));
        return entry ? Object.values(entry.contract.outcomes).map((o) => o.output) : [];
      }
      case "human":
        return Object.values(step.outcomes);
      case "wait": {
        const signal = step.signal ? this.def.signals?.[step.signal] : undefined;
        if (!signal) {
          return [EMPTY_OUTPUT];
        }
        const received = step.collect
          ? { type: "object", properties: { items: { type: "array", items: signal.schema } } }
          : signal.schema;
        return [received, EMPTY_OUTPUT];
      }
      case "spawn":
        return [step.wait === "all" ? SPAWN_ALL_OUTPUT : SPAWN_NONE_OUTPUT];
      default:
        return [];
    }
  }
}

export function validateDefinitions(catalog: Catalog, issues: IssueList): void {
  for (const { file, definition } of catalog.definitions.values()) {
    new DefinitionChecker(definition, file, catalog, issues).run();
  }
}
