/**
 * Cross-language serialization parity (EVT-21b), TypeScript side.
 *
 * Loads the Go-generated fixtures from `kanz/test/contract/serialization/
 * fixtures/` and asserts that `unframe` produces an envelope whose field
 * values match the manifest entry exactly. The fixtures are the wire
 * contract: if TS reads Go's bytes and gets different field values, the
 * bus client is wire-incompatible.
 *
 * CI regenerates fixtures before this suite runs:
 *   go run ./test/contract/serialization/cmd/genfixtures \
 *       -out ./test/contract/serialization/fixtures
 *
 * Local dev: same command. If fixtures are absent the suite skips with
 * the regen hint.
 */

import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

// EventClass lives in its OWN generated module, not envelope_pb: it is declared in
// envelope/v1/event_class.proto, and protobuf-es emits one module per .proto file.
// Importing it from envelope_pb threw at load time ("does not provide an export named
// 'EventClass'") — which every other file in this package already got right.
import { EventClass } from "@kanz-eng/kanz-schemas/envelope/v1/event_class_pb.js";
import { QualityFlag } from "@kanz-eng/kanz-schemas/envelope/v1/envelope_pb.js";

import { unframe, validate } from "../src/index.js";

const here = dirname(fileURLToPath(import.meta.url));
// kanz/ts/kanz-bus/test/ → ../../../test/contract/serialization/fixtures/
const FIXTURES_DIR = resolve(
  here,
  "..",
  "..",
  "..",
  "test",
  "contract",
  "serialization",
  "fixtures",
);
const MANIFEST_PATH = resolve(FIXTURES_DIR, "manifest.json");

interface ManifestEntry {
  name: string;
  file: string;
  payload_hex: string;
  envelope: ExpectedEnvelope;
}

interface ExpectedEnvelope {
  event_id: string;
  event_type: string;
  schema_version: number;
  envelope_version: number;
  event_class: string;
  domain: string;
  event_time: { seconds: number; nanos: number };
  ingestion_time: { seconds: number; nanos: number };
  publish_time: { seconds: number; nanos: number };
  correlation_id: string;
  causation_id: string;
  trace_context: string;
  source: string;
  producer_version: string;
  partition_key: string;
  producer_sequence: number;
  idempotency_key: string;
  quality_flags: string[];
  payload_schema_ref: string;
}

function loadManifest(): ManifestEntry[] {
  if (!existsSync(MANIFEST_PATH)) {
    return [];
  }
  const raw = readFileSync(MANIFEST_PATH, "utf8");
  return (JSON.parse(raw).fixtures as ManifestEntry[]) ?? [];
}

function hexToBytes(s: string): Uint8Array {
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(s.slice(2 * i, 2 * i + 2), 16);
  }
  return out;
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

const manifest = loadManifest();

test("fixtures directory present", { skip: manifest.length === 0 ? "fixtures absent — run: go run ./test/contract/serialization/cmd/genfixtures -out ./test/contract/serialization/fixtures" : false }, () => {
  assert.ok(manifest.length > 0, "manifest must list at least one fixture");
});

for (const entry of manifest) {
  test(`ts unframes go fixture: ${entry.name}`, () => {
    const body = readFileSync(resolve(FIXTURES_DIR, entry.file));
    const { envelope, payload } = unframe(new Uint8Array(body));

    // Roundtripped envelope passes the same EVT-21a contract.
    validate(envelope);

    assert.ok(
      bytesEqual(payload, hexToBytes(entry.payload_hex)),
      `${entry.name}: payload mismatch`,
    );

    const want = entry.envelope;
    assert.equal(envelope.eventId, want.event_id);
    assert.equal(envelope.eventType, want.event_type);
    assert.equal(envelope.schemaVersion, want.schema_version);
    assert.equal(envelope.envelopeVersion, want.envelope_version);
    assert.equal(
      envelope.eventClass,
      EventClass[want.event_class as keyof typeof EventClass],
    );
    assert.equal(envelope.domain, want.domain);
    assertTs(envelope.eventTime, want.event_time);
    assertTs(envelope.ingestionTime, want.ingestion_time);
    assertTs(envelope.publishTime, want.publish_time);
    assert.equal(envelope.correlationId, want.correlation_id);
    assert.equal(envelope.causationId, want.causation_id);
    assert.equal(envelope.traceContext, want.trace_context);
    assert.equal(envelope.source, want.source);
    assert.equal(envelope.producerVersion, want.producer_version);
    assert.equal(envelope.partitionKey, want.partition_key);
    // producer_sequence is uint64 → bigint in Protobuf-ES; manifest carries
    // a JSON number, which is safe for the small sequence values fixtures use.
    assert.equal(envelope.producerSequence, BigInt(want.producer_sequence));
    assert.equal(envelope.idempotencyKey, want.idempotency_key);
    assert.equal(envelope.payloadSchemaRef, want.payload_schema_ref);

    const gotFlags = envelope.qualityFlags.map((f) => QualityFlag[f]);
    assert.deepEqual(gotFlags, want.quality_flags);
  });
}

function assertTs(
  got: { seconds: bigint; nanos: number } | undefined,
  want: { seconds: number; nanos: number },
): void {
  assert.ok(got, "timestamp must be present");
  assert.equal(got.seconds, BigInt(want.seconds));
  assert.equal(got.nanos, want.nanos);
}
