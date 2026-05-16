/**
 * Envelope-aware consumer wrapper (EVT-19c).
 *
 * Wraps a {@link Subscriber}, turning its byte-level {@link Handler} into
 * an envelope-aware {@link EventHandler}. Per delivery:
 *
 *   `unframe` → `validate` → stash lineage on async-store → dispatch
 *
 * `unframe` / `validate` failures throw out of the wrapped handler so
 * the underlying NATSClient/KafkaClient triggers broker redelivery
 * (NATS `nak`, Kafka skip-commit). Bounded retry + DLQ routing are
 * outside EVT-19's scope per the task board — handlers must be
 * idempotent (event-class-rules §1) and the broker's redelivery is the
 * only recovery primitive.
 *
 * The handler signature deliberately omits an explicit context object —
 * lineage rides {@link propagationStore} so any {@link Producer.publish}
 * inside the handler auto-inherits the inbound envelope's
 * `correlation_id`, `causation_id` (set to the inbound `event_id`), and
 * `trace_context`. Same idiom as the Python client (EVT-18c).
 */

import type { Envelope } from "@kanz-eng/kanz-schemas/envelope/v1/envelope_pb.js";

import type { Handler, Subscriber } from "./bus.js";
import { unframe } from "./frame.js";
import { withPropagation } from "./propagation.js";
import { validate } from "./validate.js";

/**
 * Async function processing one inbound event.
 *
 * Returning normally acks; throwing prevents ack so the broker
 * redelivers per its transport semantics. Lineage from the inbound
 * envelope is on the async-store while the handler runs.
 */
export type EventHandler = (
  envelope: Envelope,
  payload: Uint8Array,
) => Promise<void>;

/** Wraps a {@link Subscriber} with envelope unframe/validate + lineage stashing. */
export class Consumer {
  private readonly subscriber: Subscriber;

  constructor(subscriber: Subscriber) {
    if (!subscriber) throw new Error("bus: subscriber is required");
    this.subscriber = subscriber;
  }

  /**
   * Bind an {@link EventHandler} to `(subject, group)`. Resolves when
   * `signal` is aborted (delegates to the underlying subscriber).
   */
  async subscribe(
    subject: string,
    group: string,
    handler: EventHandler,
    signal?: AbortSignal,
  ): Promise<void> {
    const wrapped: Handler = async (msg) => {
      const { envelope, payload } = unframe(msg.body);
      validate(envelope);
      // causation_id for the *next* event the handler emits is *this*
      // event's event_id — that is how the lineage chain links.
      await withPropagation(
        {
          correlationId: envelope.correlationId,
          causationId: envelope.eventId,
          traceContext: envelope.traceContext,
        },
        () => handler(envelope, payload),
      );
    };
    await this.subscriber.subscribe(subject, group, wrapped, signal);
  }
}
