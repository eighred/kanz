/**
 * Envelope stamping + framed publish (EVT-19b).
 *
 * Mirrors the Go `kanz/pkg/bus` Producer (EVT-17b) and Python
 * `kanz_bus.producer` (EVT-18b) so events produced from any client are
 * wire-identical.
 */

import { create, toBinary } from "@bufbuild/protobuf";
import type { DescMessage, MessageShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { randomBytes } from "node:crypto";

import {
  type Envelope,
  EnvelopeSchema,
  type QualityFlag,
} from "@eighred/kanz-schemas/envelope/v1/envelope_pb.js";
import { EventClass } from "@eighred/kanz-schemas/envelope/v1/event_class_pb.js";
import { EventFrameSchema } from "@eighred/kanz-schemas/envelope/v1/event_frame_pb.js";

import type { Client } from "./bus.js";
import {
  getCausationId,
  getCorrelationId,
  getTraceContext,
} from "./propagation.js";
import { validate } from "./validate.js";

/**
 * Envelope schema version this client builds against. Bumps once per
 * additive change to envelope.proto (rare; see
 * `kanz-schemas/docs/envelope-policy.md` §6).
 */
export const ENVELOPE_VERSION = 1;

/** A protobuf message accompanied by its serializer descriptor. */
export interface PayloadWithSchema<Desc extends DescMessage = DescMessage> {
  /** The protobuf descriptor used to serialize {@link message}. */
  schema: Desc;
  message: MessageShape<Desc>;
}

/**
 * Producer-facing form: caller-known envelope fields plus the domain
 * payload (with its schema descriptor for serialization). Auto fields
 * (event_id, publish_time, source, producer_version, producer_sequence,
 * envelope_version, correlation_id for roots, idempotency_key for
 * non-COMMAND classes) are stamped by {@link Producer.publish}.
 */
export interface Event {
  subject: string;
  eventType: string;
  eventClass: EventClass;
  schemaVersion: number;
  domain: string;
  eventTime: Date;
  payloadSchemaRef: string;
  payload: PayloadWithSchema;

  /** Defaults to `now()` when omitted. */
  ingestionTime?: Date;
  /** Empty on roots; {@link Producer.publish} fills with `event_id`. */
  correlationId?: string;
  causationId?: string;
  traceContext?: string;
  partitionKey?: string;
  /** Required for COMMAND; `""` or `== event_id` otherwise. */
  idempotencyKey?: string;
  qualityFlags?: QualityFlag[];
}

/** Identity fields the producer stamps on every event. */
export interface ProducerConfig {
  /** `service/instance` — e.g. `"market-ingest/pod-7"`. */
  source: string;
  /** Git SHA or semver of the producing code. */
  producerVersion: string;
}

/**
 * Stamps + validates envelopes and frames them onto a {@link Client}.
 *
 * Async-safe within a single Node event loop: the
 * per-(event_type, partition_key) sequence counter mutation runs to
 * completion synchronously inside {@link nextSequence} (no `await`), so
 * the Map read-modify-write is atomic under Node's cooperative
 * scheduling — no lock needed (same reasoning as the Python client).
 */
export class Producer {
  private readonly client: Client;
  private readonly config: ProducerConfig;
  private readonly sequence = new Map<string, bigint>();

  constructor(client: Client, config: ProducerConfig) {
    if (!client) throw new Error("bus: client is required");
    if (!config.source) throw new Error("bus: ProducerConfig.source required");
    if (!config.producerVersion) {
      throw new Error("bus: ProducerConfig.producerVersion required");
    }
    this.client = client;
    this.config = config;
  }

  async publish(event: Event): Promise<void> {
    if (!event.payload) throw new Error("Event.payload required");
    const env = this.stamp(event);
    validate(env);
    const payloadBytes = toBinary(event.payload.schema, event.payload.message);
    const frame = create(EventFrameSchema, {
      envelope: env,
      payload: payloadBytes,
    });
    const body = toBinary(EventFrameSchema, frame);
    await this.client.publish({
      subject: event.subject,
      body,
      key: env.partitionKey
        ? new TextEncoder().encode(env.partitionKey)
        : undefined,
      // NATS JetStream keys its broker-side dedup window on Nats-Msg-Id
      // (EVT-08); Kafka treats this as an inert user header. One header
      // serves both transports.
      headers: { "Nats-Msg-Id": env.idempotencyKey },
    });
  }

  private stamp(e: Event): Envelope {
    if (!e.eventTime) throw new Error("Event.eventTime required");
    const eventId = uuidv7();
    const now = new Date();
    const ingestion = e.ingestionTime ?? now;

    // Lineage precedence: explicit Event field > async-store value > root
    // default. EVT-19c's Consumer stashes inbound envelope fields onto
    // the AsyncLocalStorage so a handler that publishes a derived event
    // auto-inherits them.
    const correlation = e.correlationId || getCorrelationId() || eventId;
    const causation = e.causationId || getCausationId();
    const trace = e.traceContext || getTraceContext();

    let idempotency: string;
    if (e.eventClass === EventClass.COMMAND) {
      if (!e.idempotencyKey) {
        throw new Error("idempotency_key required for COMMAND events");
      }
      idempotency = e.idempotencyKey;
    } else {
      // FACT / STATE_SNAPSHOT / OBSERVATION: idempotency_key = event_id.
      if (e.idempotencyKey && e.idempotencyKey !== eventId) {
        throw new Error(
          "idempotency_key must equal event_id for non-COMMAND events",
        );
      }
      idempotency = eventId;
    }

    const sequence = this.nextSequence(e.eventType, e.partitionKey ?? "");

    return create(EnvelopeSchema, {
      eventId,
      eventType: e.eventType,
      schemaVersion: e.schemaVersion,
      envelopeVersion: ENVELOPE_VERSION,
      eventClass: e.eventClass,
      domain: e.domain,
      eventTime: timestampFromDate(e.eventTime),
      ingestionTime: timestampFromDate(ingestion),
      publishTime: timestampFromDate(now),
      correlationId: correlation,
      causationId: causation,
      traceContext: trace,
      source: this.config.source,
      producerVersion: this.config.producerVersion,
      partitionKey: e.partitionKey ?? "",
      producerSequence: sequence,
      idempotencyKey: idempotency,
      qualityFlags: e.qualityFlags ?? [],
      payloadSchemaRef: e.payloadSchemaRef,
    });
  }

  private nextSequence(eventType: string, partitionKey: string): bigint {
    if (!partitionKey) return 0n; // 0 means N/A per envelope.proto
    const key = `${eventType}\x00${partitionKey}`;
    const next = (this.sequence.get(key) ?? 0n) + 1n;
    this.sequence.set(key, next);
    return next;
  }
}

/**
 * Generate an RFC 9562 v7 UUID. Node's stdlib `crypto.randomUUID` is
 * only v4 as of Node 24; v7 needs ~10 lines of bit-packing, same
 * approach as the Python client's inline implementation.
 *
 * Layout: 48-bit Unix-ms timestamp + 4-bit version + 12-bit random +
 * 2-bit variant + 62-bit random.
 */
export function uuidv7(): string {
  const ms = BigInt(Date.now());
  const rand = randomBytes(10);
  const buf = Buffer.alloc(16);
  buf[0] = Number((ms >> 40n) & 0xffn);
  buf[1] = Number((ms >> 32n) & 0xffn);
  buf[2] = Number((ms >> 24n) & 0xffn);
  buf[3] = Number((ms >> 16n) & 0xffn);
  buf[4] = Number((ms >> 8n) & 0xffn);
  buf[5] = Number(ms & 0xffn);
  // Byte 6: 4-bit version (0x7) | upper 4 bits of rand_a
  buf[6] = 0x70 | (rand[0]! & 0x0f);
  // Byte 7: lower 8 bits of rand_a
  buf[7] = rand[1]!;
  // Byte 8: 2-bit variant (0b10) | upper 6 bits of rand_b
  buf[8] = 0x80 | (rand[2]! & 0x3f);
  // Bytes 9-15: lower 56 bits of rand_b
  for (let i = 0; i < 7; i++) buf[9 + i] = rand[3 + i]!;
  const hex = buf.toString("hex");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20, 32)}`;
}
