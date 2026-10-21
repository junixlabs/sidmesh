import type { Payload } from "@temporalio/common";
import { describe, expect, it } from "vitest";
import {
  codecFromEnv,
  handleCodecRequest,
  parseCodecKeys,
  SidmeshCodec,
  type JsonPayload,
} from "../src/runtime/codec.js";

const K1 = "k1:" + Buffer.from(Array.from({ length: 32 }, (_, i) => i)).toString("base64");
const K2 = "k2:" + Buffer.from(Array.from({ length: 32 }, (_, i) => i + 32)).toString("base64");
const text = (b: Uint8Array | null | undefined) => Buffer.from(b ?? []).toString("utf8");
const json = (v: unknown): Payload => ({
  metadata: { encoding: Buffer.from("json/plain") },
  data: Buffer.from(JSON.stringify(v)),
});
const b64 = (b: Uint8Array | null | undefined) => Buffer.from(b ?? []).toString("base64");
const toJson = (p: Payload): JsonPayload => ({
  metadata: Object.fromEntries(Object.entries(p.metadata ?? {}).map(([k, v]) => [k, b64(v)])),
  data: b64(p.data),
});

describe("payload codec (SPEC §7)", () => {
  it("round-trips and only ciphertext leaves the codec", async () => {
    const codec = new SidmeshCodec(parseCodecKeys(K1));
    const [enc] = await codec.encode([json({ order_ref: "DP-1" })]);
    expect(text(enc!.metadata!.encoding)).toBe("binary/encrypted");
    expect(text(enc!.metadata!["encryption-key-id"])).toBe("k1");
    expect(text(enc!.data)).not.toContain("DP-1");
    const [dec] = await codec.decode([enc!]);
    expect(JSON.parse(text(dec!.data))).toEqual({ order_ref: "DP-1" });
    expect(text(dec!.metadata!.encoding)).toBe("json/plain");
  });

  it("uses a fresh nonce per payload", async () => {
    const codec = new SidmeshCodec(parseCodecKeys(K1));
    const [a, b] = await codec.encode([json(1), json(1)]);
    expect(b64(a!.data)).not.toBe(b64(b!.data));
  });

  it("rotation: the first key encrypts, an older key still decrypts", async () => {
    const old = new SidmeshCodec(parseCodecKeys(K2));
    const rotated = new SidmeshCodec(parseCodecKeys(`${K1},${K2}`));
    const [before] = await old.encode([json("x")]);
    expect(text((await rotated.decode([before!]))[0]!.data)).toBe('"x"');
    const [after] = await rotated.encode([json("x")]);
    expect(text(after!.metadata!["encryption-key-id"])).toBe("k1");
  });

  it("passes through payloads that are not encrypted", async () => {
    const codec = new SidmeshCodec(parseCodecKeys(K1));
    const plain = json({ a: 1 });
    expect((await codec.decode([plain]))[0]).toBe(plain);
  });

  it("rejects malformed keys and is off without the env var", () => {
    expect(() => parseCodecKeys("k1:AAAA")).toThrow(/32 bytes/);
    expect(() => parseCodecKeys("nokey")).toThrow(/<id>:<base64 key>/);
    expect(codecFromEnv({})).toBeUndefined();
    expect(codecFromEnv({ SIDMESH_CODEC_KEYS: K1 })).toBeInstanceOf(SidmeshCodec);
  });

  it("codec server: /encode then /decode restores the payload; bad requests are 4xx", async () => {
    const codec = new SidmeshCodec(parseCodecKeys(K1));
    const original = toJson(json({ hello: "world" }));
    const enc = await handleCodecRequest(codec, "/encode", { payloads: [original] });
    expect(enc.status).toBe(200);
    const dec = await handleCodecRequest(codec, "/api/codec/decode/", enc.body);
    expect(dec).toEqual({ status: 200, body: { payloads: [original] } });
    expect((await handleCodecRequest(codec, "/other", { payloads: [] })).status).toBe(404);
    expect((await handleCodecRequest(codec, "/decode", {})).status).toBe(400);
    const tampered = structuredClone(enc.body) as { payloads: JsonPayload[] };
    tampered.payloads[0]!.metadata!["encryption-key-id"] = b64(Buffer.from("zz"));
    expect((await handleCodecRequest(codec, "/decode", tampered)).status).toBe(400);
  });
});
