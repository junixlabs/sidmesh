import { createCipheriv, createDecipheriv, randomBytes } from "node:crypto";
import type { Payload, PayloadCodec } from "@temporalio/common";
// @temporalio/proto is CommonJS: Node ESM only sees its default export.
import proto from "@temporalio/proto";

// SPEC §7: every payload is encrypted with AES-256-GCM after being serialised as a Temporal
// Payload protobuf, stored as nonce || ciphertext || tag.

export const ENCRYPTED_ENCODING = "binary/encrypted";
export const KEY_ID_METADATA = "encryption-key-id";
export const CODEC_KEYS_ENV = "SIDMESH_CODEC_KEYS";

const NONCE_BYTES = 12;
const TAG_BYTES = 16;
const ProtoPayload = proto.temporal.api.common.v1.Payload;
const utf8 = new TextEncoder();
const fromUtf8 = new TextDecoder();

export interface CodecKey {
  id: string;
  key: Buffer;
}

/** Parse `<id>:<base64 32 bytes>,...`; the first key encrypts, any key decrypts. */
export function parseCodecKeys(value: string): CodecKey[] {
  const keys = value
    .split(",")
    .map((entry) => entry.trim())
    .filter((entry) => entry !== "")
    .map((entry) => {
      const sep = entry.indexOf(":");
      if (sep <= 0) {
        throw new Error(`${CODEC_KEYS_ENV}: entry must be <id>:<base64 key>`);
      }
      const id = entry.slice(0, sep);
      const key = Buffer.from(entry.slice(sep + 1), "base64");
      if (key.length !== 32) {
        throw new Error(`${CODEC_KEYS_ENV}: key "${id}" must be 32 bytes, got ${key.length}`);
      }
      return { id, key };
    });
  if (keys.length === 0) {
    throw new Error(`${CODEC_KEYS_ENV}: no keys`);
  }
  return keys;
}

export class SidmeshCodec implements PayloadCodec {
  private readonly byId: Map<string, Buffer>;

  /** `nonce` is for test vectors only; production always draws a fresh random nonce. */
  constructor(
    private readonly keys: CodecKey[],
    private readonly nonce: () => Buffer = () => randomBytes(NONCE_BYTES),
  ) {
    if (keys.length === 0) {
      throw new Error("codec needs at least one key");
    }
    this.byId = new Map(keys.map((k) => [k.id, k.key]));
  }

  async encode(payloads: Payload[]): Promise<Payload[]> {
    return payloads.map((p) => this.encryptOne(p));
  }

  async decode(payloads: Payload[]): Promise<Payload[]> {
    return payloads.map((p) => this.decryptOne(p));
  }

  encryptOne(payload: Payload): Payload {
    const { id, key } = this.keys[0]!;
    const nonce = this.nonce();
    // Metadata keys sorted, as Go's deterministic proto marshal writes map entries, so a
    // fixed nonce gives the same bytes in both languages.
    const metadata = Object.fromEntries(Object.entries(payload.metadata ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
    // Proto3 omits empty bytes; protobufjs writes a set field even when empty, so leave it unset.
    const data = payload.data && payload.data.length > 0 ? { data: payload.data } : {};
    const plain = ProtoPayload.encode(ProtoPayload.create({ metadata, ...data })).finish();
    const cipher = createCipheriv("aes-256-gcm", key, nonce);
    const body = Buffer.concat([cipher.update(plain), cipher.final()]);
    return {
      metadata: { encoding: utf8.encode(ENCRYPTED_ENCODING), [KEY_ID_METADATA]: utf8.encode(id) },
      data: Buffer.concat([nonce, body, cipher.getAuthTag()]),
    };
  }

  /** Payloads not marked binary/encrypted pass through, so the codec can be turned on later. */
  decryptOne(payload: Payload): Payload {
    const encoding = payload.metadata?.encoding;
    if (!encoding || fromUtf8.decode(encoding) !== ENCRYPTED_ENCODING) {
      return payload;
    }
    const idBytes = payload.metadata?.[KEY_ID_METADATA];
    const id = idBytes ? fromUtf8.decode(idBytes) : "";
    const key = this.byId.get(id);
    if (!key) {
      throw new Error(`no codec key "${id}" to decrypt payload`);
    }
    const data = Buffer.from(payload.data ?? new Uint8Array());
    if (data.length < NONCE_BYTES + TAG_BYTES) {
      throw new Error("encrypted payload is too short");
    }
    const decipher = createDecipheriv("aes-256-gcm", key, data.subarray(0, NONCE_BYTES));
    decipher.setAuthTag(data.subarray(data.length - TAG_BYTES));
    const plain = Buffer.concat([decipher.update(data.subarray(NONCE_BYTES, data.length - TAG_BYTES)), decipher.final()]);
    const decoded = ProtoPayload.decode(plain);
    return { metadata: decoded.metadata ?? {}, data: decoded.data ?? new Uint8Array() };
  }
}

/** The codec from SIDMESH_CODEC_KEYS, or undefined when it is not set (codec off). */
export function codecFromEnv(env: NodeJS.ProcessEnv = process.env): SidmeshCodec | undefined {
  const value = env[CODEC_KEYS_ENV];
  return value ? new SidmeshCodec(parseCodecKeys(value)) : undefined;
}

/** Payload as Temporal's codec server JSON carries it: base64 metadata values and data. */
export interface JsonPayload {
  metadata?: Record<string, string>;
  data?: string;
}

export interface CodecServerResponse {
  status: number;
  body: { payloads: JsonPayload[] } | { error: string };
}

export function payloadFromJson(p: JsonPayload): Payload {
  const metadata: Record<string, Uint8Array> = {};
  for (const [k, v] of Object.entries(p.metadata ?? {})) {
    metadata[k] = Buffer.from(v, "base64");
  }
  return { metadata, data: Buffer.from(p.data ?? "", "base64") };
}

export function payloadToJson(p: Payload): JsonPayload {
  const metadata: Record<string, string> = {};
  for (const [k, v] of Object.entries(p.metadata ?? {})) {
    metadata[k] = Buffer.from(v).toString("base64");
  }
  return { metadata, data: Buffer.from(p.data ?? new Uint8Array()).toString("base64") };
}

/**
 * Framework-free logic of Temporal's codec server contract: POST `/encode` or `/decode` with
 * `{"payloads": [...]}`. Mount it behind the application's auth (SPEC §7); CORS and the
 * Temporal UI's headers are the caller's concern.
 */
export async function handleCodecRequest(codec: PayloadCodec, path: string, body: unknown): Promise<CodecServerResponse> {
  const op = path.replace(/\/+$/, "").split("/").pop();
  if (op !== "encode" && op !== "decode") {
    return { status: 404, body: { error: `unknown codec endpoint ${path}` } };
  }
  const payloads = (body as { payloads?: unknown } | null)?.payloads;
  if (!Array.isArray(payloads)) {
    return { status: 400, body: { error: "body must be {\"payloads\": [...]}" } };
  }
  try {
    const input = payloads.map((p) => payloadFromJson(p as JsonPayload));
    const output = op === "encode" ? await codec.encode(input) : await codec.decode(input);
    return { status: 200, body: { payloads: output.map(payloadToJson) } };
  } catch (err) {
    return { status: 400, body: { error: err instanceof Error ? err.message : String(err) } };
  }
}
