import assert from "node:assert/strict";
import { test } from "node:test";

import { create, toBinary } from "@bufbuild/protobuf";
import { TimestampSchema, timestampFromDate } from "@bufbuild/protobuf/wkt";

import type { Envelope } from "@kanz-eng/kanz-schemas/envelope/v1/envelope_pb.js";
import { EnvelopeSchema } from "@kanz-eng/kanz-schemas/envelope/v1/envelope_pb.js";
import { EventClass } from "@kanz-eng/kanz-schemas/envelope/v1/event_class_pb.js";
import { EventFrameSchema } from "@kanz-eng/kanz-schemas/envelope/v1/event_frame_pb.js";

import {
  Consumer,
  type Event,
  type Handler,
  type Message,
  Producer,
  type Subscriber,
  getCausationId,
  getCorrelationId,
  getTraceContext,
  unframe,
  withPropagation,
} from "../src/index.js";

class OneShotSub implements Subscriber {
  constructor(private readonly msg: Message) {}
  async subscribe(_s: string, _g: string, handler: Handler): Promise<void> {
    await handler(this.msg);
  }
}

class CaptureClient {
  readonly sent: Message[] = [];
  async publish(msg: Message): Promise<void> {
    this.sent.push(msg);
  }
  async subscribe(): Promise<void> {
    throw new Error("not implemented");
  }
  async close(): Promise<void> {}
}

function envelope(
  eventId = "evt-inbound",
  correlationId = "evt-inbound",
  traceContext = "",
): Envelope {
  const ts = timestampFromDate(new Date("2026-05-15T12:00:00Z"));
  return create(EnvelopeSchema, {
    eventId,
    eventType: "x.y.z",
    schemaVersion: 1,
    envelopeVersion: 1,
    eventClass: EventClass.FACT,
    domain: "market",
    eventTime: ts,
    ingestionTime: ts,
    publishTime: ts,
    correlationId,
    source: "svc/inst",
    producerVersion: "1.0",
    idempotencyKey: eventId,
    payloadSchemaRef: "x.y.z:1",
    traceContext,
  });
}

function frame(env: Envelope, payload: Uint8Array = new Uint8Array()): Uint8Array {
  return toBinary(EventFrameSchema, create(EventFrameSchema, { envelope: env, payload }));
}

test("Consumer stashes propagation on async-store", async () => {
  const env = envelope("evt-A", "corr-1", "00-trace-01");
  const sub = new OneShotSub({ subject: "x", body: frame(env, new Uint8Array([0x70])) });
  const c = new Consumer(sub);

  let captured: {
    corr: string;
    caus: string;
    trace: string;
    eventId: string;
    payload: Uint8Array;
  } | undefined;

  await c.subscribe("x", "g", async (recvEnv, payload) => {
    captured = {
      corr: getCorrelationId(),
      caus: getCausationId(),
      trace: getTraceContext(),
      eventId: recvEnv.eventId,
      payload,
    };
  });

  assert.ok(captured);
  assert.equal(captured.eventId, "evt-A");
  assert.deepEqual(captured.payload, new Uint8Array([0x70]));
  assert.equal(captured.corr, "corr-1");
  assert.equal(captured.caus, "evt-A"); // causation = inbound event_id
  assert.equal(captured.trace, "00-trace-01");
});

test("Consumer empty trace is not stashed (outer wins)", async () => {
  // Verifies the "empty does not overwrite" rule by wrapping the consume
  // in an outer withPropagation. The inbound envelope's empty trace
  // must not clobber the outer trace.
  const env = envelope("evt-A", "corr-1", ""); // empty trace
  const sub = new OneShotSub({ subject: "x", body: frame(env) });
  const c = new Consumer(sub);

  let capturedTrace = "";
  await withPropagation({ traceContext: "outer-trace" }, async () => {
    await c.subscribe("x", "g", async () => {
      capturedTrace = getTraceContext();
    });
  });
  assert.equal(capturedTrace, "outer-trace");
});

test("Consumer resets async-store after handler", async () => {
  const env = envelope("evt-A", "corr-1", "00-trace-01");
  const sub = new OneShotSub({ subject: "x", body: frame(env) });
  const c = new Consumer(sub);

  await c.subscribe("x", "g", async () => {});

  assert.equal(getCorrelationId(), "");
  assert.equal(getCausationId(), "");
  assert.equal(getTraceContext(), "");
});

test("Consumer unframe failure surfaces, handler not called", async () => {
  const sub = new OneShotSub({ subject: "x", body: new Uint8Array([0xff, 0xff, 0xff]) });
  const c = new Consumer(sub);
  let called = false;
  await assert.rejects(
    c.subscribe("x", "g", async () => {
      called = true;
    }),
    /frame/,
  );
  assert.equal(called, false);
});

test("Consumer validation failure surfaces, handler not called", async () => {
  const env = envelope();
  env.eventId = ""; // invalidates
  const sub = new OneShotSub({ subject: "x", body: frame(env) });
  const c = new Consumer(sub);
  let called = false;
  await assert.rejects(
    c.subscribe("x", "g", async () => {
      called = true;
    }),
    /event_id/,
  );
  assert.equal(called, false);
});

test("Consumer → Producer chain inherits correlation/causation/trace", async () => {
  const inbound = envelope("evt-A", "corr-root", "00-trace-01");
  const sub = new OneShotSub({
    subject: "x",
    body: frame(inbound, new Uint8Array([0x61])),
  });
  const c = new Consumer(sub);

  const cc = new CaptureClient();
  const p = new Producer(cc, { source: "test/inst", producerVersion: "v1" });

  const et = new Date("2026-05-15T12:00:00Z");
  await c.subscribe("x", "g", async () => {
    const ev: Event = {
      subject: "y",
      eventType: "downstream.event",
      eventClass: EventClass.FACT,
      schemaVersion: 1,
      domain: "market",
      eventTime: et,
      partitionKey: "k",
      payloadSchemaRef: "downstream.v1:1",
      payload: { schema: TimestampSchema, message: timestampFromDate(et) },
    };
    await p.publish(ev);
  });

  assert.equal(cc.sent.length, 1);
  const { envelope: out } = unframe(cc.sent[0]!.body);
  assert.equal(out.correlationId, "corr-root"); // inherited
  assert.equal(out.causationId, "evt-A"); // = inbound event_id
  assert.equal(out.traceContext, "00-trace-01"); // inherited
});

test("Consumer explicit Event field beats async-store value", async () => {
  const inbound = envelope("evt-A", "corr-root", "00-trace-01");
  const sub = new OneShotSub({ subject: "x", body: frame(inbound) });
  const c = new Consumer(sub);

  const cc = new CaptureClient();
  const p = new Producer(cc, { source: "test/inst", producerVersion: "v1" });

  const et = new Date("2026-05-15T12:00:00Z");
  await c.subscribe("x", "g", async () => {
    await p.publish({
      subject: "y",
      eventType: "downstream.event",
      eventClass: EventClass.FACT,
      schemaVersion: 1,
      domain: "market",
      eventTime: et,
      partitionKey: "k",
      payloadSchemaRef: "downstream.v1:1",
      payload: { schema: TimestampSchema, message: timestampFromDate(et) },
      correlationId: "explicit-corr",
      causationId: "explicit-causation",
      traceContext: "explicit-trace",
    });
  });

  const { envelope: out } = unframe(cc.sent[0]!.body);
  assert.equal(out.correlationId, "explicit-corr");
  assert.equal(out.causationId, "explicit-causation");
  assert.equal(out.traceContext, "explicit-trace");
});

test("Consumer constructor rejects null subscriber", () => {
  assert.throws(
    () => new Consumer(null as unknown as Subscriber),
    /subscriber/,
  );
});
