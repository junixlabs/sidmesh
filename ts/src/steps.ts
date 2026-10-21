// Step walks shared by impact analysis and start pinning, so both see the same dependencies.

import type { CapabilityRef, Definition } from "./types.js";

/** Every capability a definition calls, in step order: each call step, then its compensation. */
export function callsOf(def: Definition): CapabilityRef[] {
  return Object.values(def.steps).flatMap((step) =>
    step.type !== "call" ? [] : step.compensate ? [step, step.compensate] : [step],
  );
}

/** Every definition a definition can start, in step order: spawn targets and signal-start targets. */
export function startsOf(def: Definition): { stepId: string; definition: string; version: number }[] {
  return Object.entries(def.steps).flatMap(([stepId, step]) => {
    const target = step.type === "spawn" ? step : step.type === "signal" ? step.start : undefined;
    return target ? [{ stepId, definition: target.definition, version: target.version }] : [];
  });
}
