import assert from "node:assert/strict";
import { test } from "node:test";

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";

import {
  EnvelopeSchema,
  QualityFlag,
} from "@kanz-eng/kanz-schemas/envelope/v1/envelope_pb.js";
import { EventClass } from "@kanz-eng/kanz-schemas/envelope/v1/event_class_pb.js";

import { validate } from "../src/index.js";

function validEnvelope() {
  const ts = timestampFromDate(new Date("2026-05-15T12:00:00Z"));
  return create(EnvelopeSchema, {
    eventId: "evt-1",
    eventType: "market.equity.trade",
    schemaVersion: 1,
    envelopeVersion: 1,
    eventClass: EventClass.EVENT_CLASS_FACT,
    domain: "market",
    eventTime: ts,
    ingestionTime: ts,
    publishTime: ts,
    correlationId: "evt-1",
    source: "svc/inst",
    producerVersion: "1.0.0",
    idempotencyKey: "evt-1",
    payloadSchemaRef: "market.v1.MarketDataEvent:1",
    partitionKey: "AAPL",
    producerSequence: 1n,
  });
}

test("validate accepts canonical envelope", () => {
  validate(validEnvelope());
});

test("validate rejects null", () => {
  assert.throws(() => validate(null), /envelope is null/);
});

const missingFieldCases: Array<[string, (env: ReturnType<typeof validEnvelope>) => void]> = [
  ["event_id", (e) => { e.eventId = ""; }],
  ["event_type", (e) => { e.eventType = ""; }],
  ["schema_version", (e) => { e.schemaVersion = 0; }],
  ["envelope_version", (e) => { e.envelopeVersion = 0; }],
  ["event_class", (e) => { e.eventClass = EventClass.EVENT_CLASS_UNSPECIFIED; }],
  ["domain", (e) => { e.domain = ""; }],
  ["event_time", (e) => { e.eventTime = undefined; }],
  ["ingestion_time", (e) => { e.ingestionTime = undefined; }],
  ["publish_time", (e) => { e.publishTime = undefined; }],
  ["correlation_id", (e) => { e.correlationId = ""; }],
  ["source", (e) => { e.source = ""; }],
  ["producer_version", (e) => { e.producerVersion = ""; }],
  ["idempotency_key", (e) => { e.idempotencyKey = ""; }],
  ["payload_schema_ref", (e) => { e.payloadSchemaRef = ""; }],
];

for (const [fieldName, mutate] of missingFieldCases) {
  test(`validate rejects missing ${fieldName}`, () => {
    const env = validEnvelope();
    mutate(env);
    assert.throws(() => validate(env), new RegExp(fieldName));
  });
}

test("validate rejects FACT with mismatched idempotency_key", () => {
  const env = validEnvelope();
  env.idempotencyKey = "different-key";
  assert.throws(() => validate(env), /FACT/);
});

test("validate rejects REPLAYED flag", () => {
  const env = validEnvelope();
  env.qualityFlags = [QualityFlag.QUALITY_FLAG_REPLAYED];
  assert.throws(() => validate(env), /REPLAYED/);
});

test("validate rejects sequence without partition_key", () => {
  const env = validEnvelope();
  env.partitionKey = "";
  env.producerSequence = 5n;
  assert.throws(() => validate(env), /producer_sequence/);
});

test("validate allows COMMAND with caller idempotency_key", () => {
  const env = validEnvelope();
  env.eventClass = EventClass.EVENT_CLASS_COMMAND;
  env.idempotencyKey = "caller-key"; // != event_id is fine for COMMAND
  validate(env);
});
