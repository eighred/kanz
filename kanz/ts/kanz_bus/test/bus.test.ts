import assert from "node:assert/strict";
import { test } from "node:test";

import { KafkaClient, NATSClient } from "../src/index.js";

test("NATSClient rejects empty url", () => {
  assert.throws(() => new NATSClient({ url: "" }), /url required/);
});

test("KafkaClient rejects empty brokers", () => {
  assert.throws(() => new KafkaClient({ brokers: [] }), /broker/);
});

test("NATSClient.publish before connect throws", async () => {
  const c = new NATSClient({ url: "nats://localhost:4222" });
  await assert.rejects(
    c.publish({ subject: "x", body: new Uint8Array([1, 2, 3]) }),
    /not connected/,
  );
});

test("KafkaClient.publish before connect throws", async () => {
  const c = new KafkaClient({ brokers: ["localhost:9092"] });
  await assert.rejects(
    c.publish({ subject: "x", body: new Uint8Array([1, 2, 3]) }),
    /not connected/,
  );
});

test("NATSClient.close without connect is a no-op", async () => {
  const c = new NATSClient({ url: "nats://localhost:4222" });
  await c.close();
});

test("KafkaClient.close without connect is a no-op", async () => {
  const c = new KafkaClient({ brokers: ["localhost:9092"] });
  await c.close();
});

test("NATSClient applies config defaults", () => {
  const c = new NATSClient({ url: "nats://localhost:4222" }) as unknown as {
    cfg: {
      name: string;
      connectTimeoutMs: number;
      reconnectWaitMs: number;
      maxReconnectAttempts: number;
      publishTimeoutMs: number;
    };
  };
  assert.equal(c.cfg.name, "kanz");
  assert.equal(c.cfg.connectTimeoutMs, 10_000);
  assert.equal(c.cfg.reconnectWaitMs, 2_000);
  assert.equal(c.cfg.maxReconnectAttempts, -1);
  assert.equal(c.cfg.publishTimeoutMs, 5_000);
});

test("KafkaClient applies config defaults", () => {
  const c = new KafkaClient({ brokers: ["localhost:9092"] }) as unknown as {
    cfg: { clientId: string; publishTimeoutMs: number };
  };
  assert.equal(c.cfg.clientId, "kanz");
  assert.equal(c.cfg.publishTimeoutMs, 5_000);
});
