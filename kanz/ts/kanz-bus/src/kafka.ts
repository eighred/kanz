/** Kafka wrapper — connection mgmt + publish/subscribe via kafkajs. */

import {
  CompressionCodecs,
  CompressionTypes,
  type Consumer,
  Kafka,
  type KafkaMessage,
  type Producer,
} from "kafkajs";
// @ts-expect-error — kafkajs-snappy ships without bundled types.
import SnappyCodec from "kafkajs-snappy";

import type { Client, Handler, Message } from "./bus.js";

// Snappy must be wired up before the first send/receive that touches a
// snappy-compressed batch. Idempotent — the assignment is harmless if
// repeated across multiple KafkaClient instances. Matches the Go writer's
// kafka.Snappy and the Python aiokafka producer's compression_type="snappy".
CompressionCodecs[CompressionTypes.Snappy] = SnappyCodec;

export interface KafkaConfig {
  brokers: string[];
  clientId?: string;
  /** Per-publish timeout in ms (kafkajs `timeout`). Default 5_000. */
  publishTimeoutMs?: number;
}

/**
 * Bus client over Kafka (kafkajs).
 *
 * ```ts
 * const client = new KafkaClient({ brokers: ["localhost:9092"] });
 * await client.connect();
 * await client.publish({ subject: "market.equity", key: bytes, body: bytes });
 * await client.close();
 * ```
 */
export class KafkaClient implements Client {
  private readonly cfg: Required<KafkaConfig>;
  private readonly kafka: Kafka;
  private producer: Producer | null = null;
  private readonly consumers: Set<Consumer> = new Set();

  constructor(cfg: KafkaConfig) {
    if (!cfg.brokers || cfg.brokers.length === 0) {
      throw new Error("kafka: at least one broker required");
    }
    this.cfg = {
      brokers: cfg.brokers,
      clientId: cfg.clientId ?? "kanz",
      publishTimeoutMs: cfg.publishTimeoutMs ?? 5_000,
    };
    this.kafka = new Kafka({
      brokers: this.cfg.brokers,
      clientId: this.cfg.clientId,
    });
  }

  async connect(): Promise<void> {
    if (this.producer) return;
    // allowAutoTopicCreation matches the Go writer's
    // AllowAutoTopicCreation=false (EVT-09: topics provisioned explicitly
    // by the topics bootstrap Job, never by the client).
    this.producer = this.kafka.producer({ allowAutoTopicCreation: false });
    await this.producer.connect();
  }

  async close(): Promise<void> {
    const errs: unknown[] = [];
    if (this.producer) {
      try {
        await this.producer.disconnect();
      } catch (e) {
        errs.push(e);
      }
      this.producer = null;
    }
    for (const c of this.consumers) {
      try {
        await c.disconnect();
      } catch (e) {
        errs.push(e);
      }
    }
    this.consumers.clear();
    if (errs.length > 0) throw errs[0];
  }

  async publish(msg: Message): Promise<void> {
    if (!this.producer) {
      throw new Error("kafka: client not connected; call connect() first");
    }
    const headers: Record<string, string> = msg.headers ?? {};
    await this.producer.send({
      topic: msg.subject,
      compression: CompressionTypes.Snappy,
      timeout: this.cfg.publishTimeoutMs,
      acks: -1, // RequireAll — matches Go writer
      messages: [
        {
          key: msg.key ? Buffer.from(msg.key) : null,
          value: Buffer.from(msg.body),
          headers,
        },
      ],
    });
  }

  /**
   * Consume `topic` under consumer-group `group`. Resolves when `signal`
   * is aborted. Handler exceptions skip the commit so the message is
   * redelivered after consumer restart / rebalance; normal return
   * commits. Bounded retry + DLQ routing layer on top in a later EVT-19
   * subtask.
   */
  async subscribe(
    topic: string,
    group: string,
    handler: Handler,
    signal?: AbortSignal,
  ): Promise<void> {
    const consumer = this.kafka.consumer({ groupId: group });
    this.consumers.add(consumer);
    await consumer.connect();
    await consumer.subscribe({ topic, fromBeginning: false });
    await consumer.run({
      autoCommit: false,
      eachMessage: async ({ topic: t, partition, message }) => {
        const msg = kafkaToMessage(t, message);
        try {
          await handler(msg);
        } catch {
          // No commit — message is redelivered after restart / rebalance.
          return;
        }
        await consumer.commitOffsets([
          { topic: t, partition, offset: (BigInt(message.offset) + 1n).toString() },
        ]);
      },
    });
    try {
      await waitForAbort(signal);
    } finally {
      try {
        await consumer.disconnect();
      } finally {
        this.consumers.delete(consumer);
      }
    }
  }
}

function kafkaToMessage(topic: string, m: KafkaMessage): Message {
  const out: Message = {
    subject: topic,
    body: m.value ? new Uint8Array(m.value) : new Uint8Array(),
  };
  if (m.key) out.key = new Uint8Array(m.key);
  if (m.headers) {
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(m.headers)) {
      if (v == null) {
        headers[k] = "";
      } else if (typeof v === "string") {
        headers[k] = v;
      } else if (Array.isArray(v)) {
        const first = v[0];
        headers[k] = first == null ? "" : Buffer.from(first).toString("utf-8");
      } else {
        headers[k] = Buffer.from(v).toString("utf-8");
      }
    }
    if (Object.keys(headers).length > 0) out.headers = headers;
  }
  return out;
}

function waitForAbort(signal?: AbortSignal): Promise<void> {
  if (!signal) return new Promise(() => {}); // never resolves
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}
