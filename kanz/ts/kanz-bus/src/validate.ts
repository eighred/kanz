/**
 * Envelope validation — publish-side rules.
 *
 * Enforces the invariants in `kanz-schemas/docs/envelope-policy.md` §7
 * plus the per-class rules from `docs/event-class-rules.md`.
 * {@link Producer.publish} runs this after stamping; consumers should
 * run it on receive too — the envelope is the constitution and a
 * defensive check is cheap.
 */

import type { Envelope } from "@kanz-eng/kanz-schemas/envelope/v1/envelope_pb.js";
import { QualityFlag } from "@kanz-eng/kanz-schemas/envelope/v1/envelope_pb.js";
import { EventClass } from "@kanz-eng/kanz-schemas/envelope/v1/event_class_pb.js";

/** Throw on any envelope-policy violation. */
export function validate(env: Envelope | null | undefined): void {
  if (!env) throw new Error("envelope is null");
  requireNonEmpty(env.eventId, "event_id");
  requireNonEmpty(env.eventType, "event_type");
  if (env.schemaVersion === 0) {
    throw new Error("schema_version required (must be >= 1)");
  }
  if (env.envelopeVersion === 0) {
    throw new Error("envelope_version required");
  }
  if (env.eventClass === EventClass.UNSPECIFIED) {
    throw new Error("event_class must not be UNSPECIFIED");
  }
  requireNonEmpty(env.domain, "domain");
  if (!env.eventTime) throw new Error("event_time required");
  if (!env.ingestionTime) throw new Error("ingestion_time required");
  if (!env.publishTime) throw new Error("publish_time required");
  requireNonEmpty(env.correlationId, "correlation_id");
  requireNonEmpty(env.source, "source");
  requireNonEmpty(env.producerVersion, "producer_version");
  requireNonEmpty(env.idempotencyKey, "idempotency_key");
  requireNonEmpty(env.payloadSchemaRef, "payload_schema_ref");
  // FACT events: idempotency_key MUST equal event_id (event-class-rules §1).
  if (
    env.eventClass === EventClass.FACT &&
    env.idempotencyKey !== env.eventId
  ) {
    throw new Error("FACT events require idempotency_key == event_id");
  }
  // QUALITY_FLAG_REPLAYED is set only by replay tooling (EVT-20). A live
  // publisher must reject it before the bus.
  for (const qf of env.qualityFlags) {
    if (qf === QualityFlag.REPLAYED) {
      throw new Error("live publish must not set QUALITY_FLAG_REPLAYED");
    }
  }
  // producer_sequence is per-(source, partition_key); 0 means N/A. Empty
  // partition_key ⇒ sequence must be 0 (envelope.proto field comment).
  if (env.partitionKey === "" && env.producerSequence !== 0n) {
    throw new Error("producer_sequence must be 0 when partition_key is empty");
  }
}

function requireNonEmpty(s: string, name: string): void {
  if (!s) throw new Error(`${name} required`);
}
