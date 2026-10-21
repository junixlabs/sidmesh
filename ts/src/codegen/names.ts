import { capabilityKey, verbOf } from "../capability.js";

const GO_INITIALISMS = new Set(["id", "url", "api", "sku", "http", "json", "uuid", "ip", "po", "sla"]);

function words(name: string): string[] {
  return name
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .split(/[^A-Za-z0-9]+/)
    .filter(Boolean)
    .map((w) => w.toLowerCase());
}

/** place_po -> PlacePo, not_found -> NotFound. */
export function pascal(name: string): string {
  return words(name)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join("");
}

/** Go exported identifier with common initialisms: order_id -> OrderID. */
export function goName(name: string): string {
  const result = words(name)
    .map((w) => (GO_INITIALISMS.has(w) ? w.toUpperCase() : w.charAt(0).toUpperCase() + w.slice(1)))
    .join("");
  return /^[0-9]/.test(result) ? `F${result}` : result || "Field";
}

/** <Verb>V<N>, e.g. supplier.place_po v1 -> PlacePoV1. */
export function typePrefix(capability: string, version: number): string {
  return `${pascal(verbOf(capability) || capability)}V${version}`;
}

export function handlerKey(capability: string, version: number): string {
  return capabilityKey(verbOf(capability), version);
}
