import { createHash } from "node:crypto";
import { capabilityKey } from "../capability.js";
import { canonicalJson } from "../schema.js";
import type { Contract } from "../types.js";

/**
 * SPEC §4: lowercase hex SHA-256 of
 * "<tenant>\n<capability>@<version>\n<value1>\n<value2>..." with each value JSON-encoded
 * canonically (RFC 8785), so Go and TS produce the same key for object values too.
 */
export function idempotencyKey(
  tenant: string,
  capability: string,
  version: number,
  values: unknown[],
): string {
  const lines = [tenant, capabilityKey(capability, version), ...values.map(canonicalJson)];
  return createHash("sha256").update(lines.join("\n"), "utf8").digest("hex");
}

/** Values of the contract's idempotency key paths, read from the input. */
export function idempotencyValues(contract: Contract, input: unknown): unknown[] {
  return (contract.idempotency?.key ?? []).map((keyPath) => {
    let value: unknown = input;
    for (const segment of keyPath.split(".").slice(1)) {
      value =
        typeof value === "object" && value !== null ? (value as Record<string, unknown>)[segment] : undefined;
    }
    return value;
  });
}
