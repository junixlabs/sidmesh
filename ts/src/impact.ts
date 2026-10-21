import { capabilityKey } from "./capability.js";
import { callsOf, startsOf } from "./steps.js";
import type { Definition } from "./types.js";
import type { Catalog } from "./validate/catalog.js";

export interface ImpactResult {
  target: string;
  kind: "capability" | "definition";
  /** For a capability: definitions that call it, directly or through a child they start. */
  definitions: string[];
  /** For a definition: capabilities it calls (including compensations). */
  capabilities: string[];
  /** For a definition: definitions it can start (spawn or signal start). */
  spawns: string[];
}

const CAPABILITY = /^([a-z][a-z0-9_]*\.[a-z][a-z0-9_]*)(?:@(\d+))?$/;
const DEFINITION = /^([a-z][a-z0-9_]*)(?:@(\d+))?$/;

const callKeys = (def: Definition) => callsOf(def).map((c) => capabilityKey(c.capability, c.version));
const startKeys = (def: Definition) => startsOf(def).map((s) => capabilityKey(s.definition, s.version));

function matches(key: string, name: string, version: string | undefined): boolean {
  return version === undefined ? key.startsWith(`${name}@`) : key === `${name}@${version}`;
}

/**
 * What a change touches, computed from definitions only (no hand-kept dependency lists).
 * A capability affects every definition calling it and, transitively, every definition
 * that can start an affected one (spawn or signal start).
 */
export function impact(catalog: Catalog, target: string): ImpactResult {
  const defs = [...catalog.definitions.entries()].map(([key, entry]) => ({ key, def: entry.definition }));
  const capability = CAPABILITY.exec(target);
  if (capability) {
    const [, name, version] = capability;
    const affected = new Set(
      defs
        .filter(({ def }) => callKeys(def).some((call) => matches(call, name as string, version)))
        .map(({ key }) => key),
    );
    let grew = true;
    while (grew) {
      grew = false;
      for (const { key, def } of defs) {
        if (!affected.has(key) && startKeys(def).some((child) => affected.has(child))) {
          affected.add(key);
          grew = true;
        }
      }
    }
    return { target, kind: "capability", definitions: [...affected].sort(), capabilities: [], spawns: [] };
  }

  const definition = DEFINITION.exec(target);
  if (!definition) {
    throw new Error(`"${target}" is neither a capability (<node>.<verb>[@N]) nor a definition id`);
  }
  const [, name, version] = definition;
  const selected = defs.filter(({ key }) => matches(key, name as string, version));
  if (selected.length === 0) {
    throw new Error(`definition "${target}" not found`);
  }
  return {
    target,
    kind: "definition",
    definitions: selected.map(({ key }) => key).sort(),
    capabilities: [...new Set(selected.flatMap(({ def }) => callKeys(def)))].sort(),
    spawns: [...new Set(selected.flatMap(({ def }) => startKeys(def)))].sort(),
  };
}
