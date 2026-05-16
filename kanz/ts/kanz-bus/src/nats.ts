/** NATS JetStream wrapper — connection mgmt + publish/subscribe. */

import {
  AckPolicy,
  type Consumer,
  type JetStreamClient,
  type JetStreamManager,
  type JsMsg,
  type NatsConnection,
  connect,
  headers as natsHeaders,
} from "nats";

import {
  type Client,
  type Handler,
  type Message,
  NATS_PARTITION_KEY_HEADER,
} from "./bus.js";

export interface NATSConfig {
  /** `nats://host:4222` (comma-separated for cluster). */
  url: string;
  /** Client identifier surfaced to the broker. */
  name?: string;
  /** Connect timeout in ms. Default 10_000. */
  connectTimeoutMs?: number;
  /** Reconnect wait between attempts in ms. Default 2_000. */
  reconnectWaitMs?: number;
  /** Max reconnect attempts (-1 = forever). Default -1. */
  maxReconnectAttempts?: number;
  /** Per-publish timeout in ms. Default 5_000. */
  publishTimeoutMs?: number;
}

/**
 * Bus client over NATS JetStream.
 *
 * ```ts
 * const client = new NATSClient({ url: "nats://localhost:4222" });
 * await client.connect();
 * await client.publish({ subject: "market.equity.trade", body: bytes });
 * await client.close();
 * ```
 */
export class NATSClient implements Client {
  private readonly cfg: Required<NATSConfig>;
  private nc: NatsConnection | null = null;
  private js: JetStreamClient | null = null;
  private jsm: JetStreamManager | null = null;

  constructor(cfg: NATSConfig) {
    if (!cfg.url) throw new Error("nats: url required");
    this.cfg = {
      url: cfg.url,
      name: cfg.name ?? "kanz",
      connectTimeoutMs: cfg.connectTimeoutMs ?? 10_000,
      reconnectWaitMs: cfg.reconnectWaitMs ?? 2_000,
      maxReconnectAttempts: cfg.maxReconnectAttempts ?? -1,
      publishTimeoutMs: cfg.publishTimeoutMs ?? 5_000,
    };
  }

  async connect(): Promise<void> {
    if (this.nc) return;
    this.nc = await connect({
      servers: this.cfg.url,
      name: this.cfg.name,
      timeout: this.cfg.connectTimeoutMs,
      reconnectTimeWait: this.cfg.reconnectWaitMs,
      maxReconnectAttempts: this.cfg.maxReconnectAttempts,
    });
    this.js = this.nc.jetstream();
    this.jsm = await this.nc.jetstreamManager();
  }

  async close(): Promise<void> {
    if (!this.nc) return;
    await this.nc.close();
    this.nc = null;
    this.js = null;
    this.jsm = null;
  }

  async publish(msg: Message): Promise<void> {
    if (!this.js) {
      throw new Error("nats: client not connected; call connect() first");
    }
    const h = natsHeaders();
    if (msg.headers) {
      for (const [k, v] of Object.entries(msg.headers)) h.append(k, v);
    }
    if (msg.key && msg.key.length > 0) {
      h.append(NATS_PARTITION_KEY_HEADER, new TextDecoder().decode(msg.key));
    }
    await this.js.publish(msg.subject, msg.body, {
      headers: h,
      timeout: this.cfg.publishTimeoutMs,
    });
  }

  /**
   * Pull-consume `subject` under durable consumer `group`. Resolves when
   * `signal` is aborted. Handler exceptions trigger `nak` so the broker
   * redelivers; normal return triggers `ack`. JetStream stream binding
   * is provisioned out-of-band by `kanz/infra/nats/`; this method binds
   * (idempotently upserts) the durable consumer on that stream.
   */
  async subscribe(
    subject: string,
    group: string,
    handler: Handler,
    signal?: AbortSignal,
  ): Promise<void> {
    if (!this.js || !this.jsm) throw new Error("nats: client not connected");
    const streamName = await this.jsm.streams.find(subject);
    await this.ensureConsumer(streamName, group, subject);
    const consumer: Consumer = await this.js.consumers.get(streamName, group);
    const iter = await consumer.consume();
    const stop = () => {
      iter.stop();
    };
    if (signal) {
      if (signal.aborted) {
        stop();
        return;
      }
      signal.addEventListener("abort", stop, { once: true });
    }
    try {
      for await (const m of iter) {
        try {
          await handler(natsToMessage(m));
        } catch {
          m.nak();
          continue;
        }
        m.ack();
      }
    } finally {
      if (signal) signal.removeEventListener("abort", stop);
    }
  }

  private async ensureConsumer(
    stream: string,
    group: string,
    subject: string,
  ): Promise<void> {
    if (!this.jsm) throw new Error("nats: client not connected");
    try {
      await this.jsm.consumers.info(stream, group);
    } catch {
      await this.jsm.consumers.add(stream, {
        durable_name: group,
        ack_policy: AckPolicy.Explicit,
        filter_subject: subject,
      });
    }
  }
}

function natsToMessage(m: JsMsg): Message {
  const out: Message = { subject: m.subject, body: m.data };
  const hdr = m.headers;
  if (!hdr) return out;
  const headers: Record<string, string> = {};
  for (const k of hdr.keys()) {
    const values = hdr.values(k);
    if (values.length === 0) continue;
    const v = values[0]!;
    if (k === NATS_PARTITION_KEY_HEADER) {
      out.key = new TextEncoder().encode(v);
      continue;
    }
    headers[k] = v;
  }
  if (Object.keys(headers).length > 0) out.headers = headers;
  return out;
}
