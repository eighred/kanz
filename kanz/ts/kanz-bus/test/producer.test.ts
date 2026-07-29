import assert from "node:assert/strict";
import { test } from "node:test";

import { TimestampSchema, timestampFromDate } from "@bufbuild/protobuf/wkt";

import { EventClass } from "@eighred/kanz-schemas/envelope/v1/event_class_pb.js";

import {
  type Client,
  type Event,
  type Handler,
  type Message,
  Producer,
  ProducerConfig,
  unframe,
} from "../src/index.js";

class CaptureClient implements Client {
  readonly sent: Message[] = [];
  async publish(msg: Message): Promise<void> {
    this.sent.push(msg);
  }
  async subscribe(_s: string, _g: string, _h: Handler): Promise<void> {
    throw new Error("not implemented");
  }
  async close(): Promise<void> {}
}

function makeProducer(): { producer: Producer; client: CaptureClient } {
  const client = new CaptureClient();
  const producer = new Producer(client, {
    source: "test-svc/inst-1",
    producerVersion: "test-1.0.0",
  });
  return { producer, client };
}

function factEvent(): Event {
  const et = new Date("2026-05-15T12:00:00Z");
  return {
    subject: "market.equity.trade",
    eventType: "market.equity.trade",
    eventClass: EventClass.FACT,
    schemaVersion: 1,
    domain: "market",
    eventTime: et,
    partitionKey: "AAPL",
    payloadSchemaRef: "market.v1.MarketDataEvent:1",
    payload: { schema: TimestampSchema, message: timestampFromDate(et) },
  };
}

test("producer stamps FACT event", async () => {
  const { producer, client } = makeProducer();
  await producer.publish(factEvent());
  assert.equal(client.sent.length, 1);
  const sent = client.sent[0]!;
  assert.equal(sent.subject, "market.equity.trade");
  assert.deepEqual(sent.key, new TextEncoder().encode("AAPL"));

  const { envelope: env } = unframe(sent.body);
  assert.ok(env.eventId);
  assert.equal(env.correlationId, env.eventId); // root event
  assert.equal(env.idempotencyKey, env.eventId); // FACT
  assert.equal(env.envelopeVersion, 1);
  assert.equal(env.source, "test-svc/inst-1");
  assert.equal(env.producerVersion, "test-1.0.0");
  assert.equal(env.producerSequence, 1n);
  assert.ok(env.publishTime);
  assert.ok(env.ingestionTime);
});

test("producer propagates explicit causation", async () => {
  const { producer, client } = makeProducer();
  const e = factEvent();
  e.correlationId = "root-corr-id";
  e.causationId = "parent-event-id";
  await producer.publish(e);
  const { envelope: env } = unframe(client.sent[0]!.body);
  assert.equal(env.correlationId, "root-corr-id");
  assert.equal(env.causationId, "parent-event-id");
});

test("producer sequence increments per partition_key", async () => {
  const { producer, client } = makeProducer();
  for (const pk of ["AAPL", "AAPL", "MSFT", "AAPL"]) {
    const e = factEvent();
    e.partitionKey = pk;
    await producer.publish(e);
  }
  const seqs = client.sent.map((m) => unframe(m.body).envelope.producerSequence);
  assert.deepEqual(seqs, [1n, 2n, 1n, 3n]);
});

test("producer sequence is 0 without partition_key", async () => {
  const { producer, client } = makeProducer();
  const e = factEvent();
  e.partitionKey = "";
  await producer.publish(e);
  const { envelope: env } = unframe(client.sent[0]!.body);
  assert.equal(env.producerSequence, 0n);
});

test("COMMAND requires idempotency_key", async () => {
  const { producer } = makeProducer();
  const e = factEvent();
  e.eventClass = EventClass.COMMAND;
  e.eventType = "risk.command.rebalance";
  e.idempotencyKey = "";
  await assert.rejects(producer.publish(e), /idempotency_key/);
});

test("COMMAND preserves caller idempotency_key", async () => {
  const { producer, client } = makeProducer();
  const e = factEvent();
  e.eventClass = EventClass.COMMAND;
  e.idempotencyKey = "caller-key-123";
  await producer.publish(e);
  const { envelope: env } = unframe(client.sent[0]!.body);
  assert.equal(env.idempotencyKey, "caller-key-123");
  assert.notEqual(env.idempotencyKey, env.eventId);
});

test("non-COMMAND rejects mismatched idempotency_key", async () => {
  const { producer } = makeProducer();
  const e = factEvent();
  e.idempotencyKey = "not-the-event-id";
  await assert.rejects(producer.publish(e), /idempotency_key/);
});

test("producer rejects missing eventTime", async () => {
  const { producer } = makeProducer();
  const e = factEvent();
  (e as unknown as { eventTime: undefined }).eventTime = undefined;
  await assert.rejects(producer.publish(e), /eventTime/);
});

test("producer rejects missing payload", async () => {
  const { producer } = makeProducer();
  const e = factEvent();
  (e as unknown as { payload: undefined }).payload = undefined;
  await assert.rejects(producer.publish(e), /payload/);
});

test("Producer constructor rejects missing config / client", () => {
  assert.throws(
    () => new Producer(new CaptureClient(), { source: "", producerVersion: "" } as ProducerConfig),
    /source/,
  );
  assert.throws(
    () => new Producer(new CaptureClient(), { source: "x", producerVersion: "" } as ProducerConfig),
    /producerVersion/,
  );
  assert.throws(
    () =>
      new Producer(null as unknown as Client, {
        source: "x",
        producerVersion: "v",
      }),
    /client/,
  );
});

test("producer stamps broker-dedup header", async () => {
  const { producer, client } = makeProducer();
  await producer.publish(factEvent());
  const sent = client.sent[0]!;
  const { envelope: env } = unframe(sent.body);
  assert.ok(env.idempotencyKey);
  assert.equal(sent.headers?.["Nats-Msg-Id"], env.idempotencyKey);
});

test("root event correlation_id defaults to event_id", async () => {
  const { producer, client } = makeProducer();
  await producer.publish(factEvent());
  const { envelope: env } = unframe(client.sent[0]!.body);
  assert.equal(env.correlationId, env.eventId);
  assert.equal(env.causationId, "");
});
