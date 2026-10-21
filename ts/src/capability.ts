// Capability-string helpers shared by validation, codegen and the runtime.

/** Registry key of a capability or definition: supplier.place_po v1 -> supplier.place_po@1. */
export function capabilityKey(capability: string, version: number): string {
  return `${capability}@${version}`;
}

/** Node part of a capability: supplier.place_po -> supplier. */
export function nodeOf(capability: string): string {
  return capability.split(".")[0] ?? "";
}

/** Verb part of a capability: supplier.place_po -> place_po. */
export function verbOf(capability: string): string {
  return capability.split(".")[1] ?? "";
}
