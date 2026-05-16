/**
 * Lineage propagation via Node's {@link AsyncLocalStorage} — the TS idiom
 * for what Go does with `context.Context` and Python does with
 * `contextvars` (PEP 567).
 *
 * EVT-19b shipped the getters used by {@link Producer.publish}; EVT-19c
 * adds {@link withPropagation} (the setter API) and the {@link Consumer}
 * that stashes inbound envelope fields onto the store for the handler's
 * duration.
 *
 * `AsyncLocalStorage` is preserved across `await` and `Promise.then`
 * within the same async chain (Node copies the store on async boundaries
 * via async_hooks), so lineage propagates through awaits without further
 * effort — same property as Python's contextvars.
 */

import { AsyncLocalStorage } from "node:async_hooks";

export interface PropagationContext {
  correlationId: string;
  causationId: string;
  traceContext: string;
}

/** @internal — exported so EVT-19c's setter API can run inside its scope. */
export const propagationStore = new AsyncLocalStorage<PropagationContext>();

/** Return the correlation_id on the current async context, or `""`. */
export function getCorrelationId(): string {
  return propagationStore.getStore()?.correlationId ?? "";
}

/** Return the causation_id on the current async context, or `""`. */
export function getCausationId(): string {
  return propagationStore.getStore()?.causationId ?? "";
}

/** Return the trace_context on the current async context, or `""`. */
export function getTraceContext(): string {
  return propagationStore.getStore()?.traceContext ?? "";
}

/**
 * Run `fn` with lineage fields set on the async-local store for the
 * duration of the call (and any awaited descendants).
 *
 * Empty fields do NOT clobber an outer-context value — matches the Go
 * client's "non-empty only" stashing rule and the Python client's
 * `propagation_context` partial-set behaviour. Pass only the fields you
 * want to update.
 *
 * ```ts
 * await withPropagation({ correlationId: "root", traceContext: "00-..." }, async () => {
 *   await producer.publish(event); // auto-inherits correlation/trace
 * });
 * ```
 */
export function withPropagation<T>(
  ctx: Partial<PropagationContext>,
  fn: () => T,
): T {
  const current = propagationStore.getStore();
  const next: PropagationContext = {
    correlationId: ctx.correlationId || current?.correlationId || "",
    causationId: ctx.causationId || current?.causationId || "",
    traceContext: ctx.traceContext || current?.traceContext || "",
  };
  return propagationStore.run(next, fn);
}
