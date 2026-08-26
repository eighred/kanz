/**
 * kanz-bus — TypeScript bus client for the Kanz event backbone.
 *
 * EVT-19a is the wire-level layer: connect, publish bytes, subscribe to
 * bytes. EVT-19b adds envelope stamping + pre-publish validation;
 * EVT-19c adds trace / correlation / causation propagation. Mirrors the
 * Go client (kanz/pkg/bus, EVT-17) and Python client (kanz-py, EVT-18)
 * on the wire so producers/consumers in any language interoperate.
 */

/**
 * Wire-level event: opaque body plus transport-agnostic metadata. Body
 * is typically a serialized envelope frame; later EVT-19 layers stamp
 * the envelope before calling {@link Publisher.publish}.
 */
export interface Message {
  /** NATS subject / Kafka topic, per kanz-schemas/README.md
   *  § Subject Taxonomy. */
  subject: string;
  /** Opaque payload — typically a serialized envelope frame. */
  body: Uint8Array;
  /**
   * Partition key. Used directly as the Kafka message key. NATS has no
   * native partition-key concept — wrappers carry it as the
   * {@link NATS_PARTITION_KEY_HEADER} header on the wire and reconstruct
   * `key` on receive.
   */
  key?: Uint8Array;
  /** Free-form transport metadata (trace ids, schema refs, …). */
  headers?: Record<string, string>;
}

/**
 * Async function processing one inbound {@link Message}. Returning
 * normally acks/commits the message; throwing prevents the ack so the
 * broker redelivers per its transport semantics. Bounded retry + DLQ
 * routing layer on top in a later EVT-19 subtask.
 */
export type Handler = (msg: Message) => Promise<void>;

/** Publishes one message at a time. Errors are transport-level. */
export interface Publisher {
  publish(msg: Message): Promise<void>;
}

/**
 * Binds a {@link Handler} to a `(subject, group)` pair. The returned
 * promise resolves when `signal` is aborted; one call per
 * `(subject, group)`. `group` is the durable consumer name (NATS) /
 * consumer-group id (Kafka).
 */
export interface Subscriber {
  subscribe(
    subject: string,
    group: string,
    handler: Handler,
    signal?: AbortSignal,
  ): Promise<void>;
}

/** Transport that does both. {@link NATSClient} and {@link KafkaClient} satisfy it. */
export interface Client extends Publisher, Subscriber {
  close(): Promise<void>;
}

/**
 * Wire header carrying {@link Message.key} over NATS, which lacks a
 * native partition-key concept (Kafka has one). The receive path strips
 * this header from the user-visible {@link Message.headers} and surfaces
 * it as {@link Message.key}. Matches the Go client (`kanz/pkg/bus`,
 * EVT-17a) and Python client (`kanz-py`, EVT-18a).
 */
export const NATS_PARTITION_KEY_HEADER = "Kanz-Partition-Key";
