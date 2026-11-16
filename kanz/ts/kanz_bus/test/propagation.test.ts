import assert from "node:assert/strict";
import { setTimeout as delay } from "node:timers/promises";
import { test } from "node:test";

import {
  getCausationId,
  getCorrelationId,
  getTraceContext,
  withPropagation,
} from "../src/index.js";

test("defaults are empty", () => {
  assert.equal(getCorrelationId(), "");
  assert.equal(getCausationId(), "");
  assert.equal(getTraceContext(), "");
});

test("withPropagation sets fields for the duration and resets after", () => {
  withPropagation(
    { correlationId: "corr", causationId: "caus", traceContext: "trace" },
    () => {
      assert.equal(getCorrelationId(), "corr");
      assert.equal(getCausationId(), "caus");
      assert.equal(getTraceContext(), "trace");
    },
  );
  assert.equal(getCorrelationId(), "");
  assert.equal(getCausationId(), "");
  assert.equal(getTraceContext(), "");
});

test("withPropagation partial set leaves other fields empty", () => {
  withPropagation({ correlationId: "corr" }, () => {
    assert.equal(getCorrelationId(), "corr");
    assert.equal(getCausationId(), "");
    assert.equal(getTraceContext(), "");
  });
});

test("withPropagation empty fields do not overwrite outer values", () => {
  withPropagation({ correlationId: "outer" }, () => {
    // Inner block with empty correlationId must NOT clobber outer's value —
    // matches Go's "non-empty only" stashing rule.
    withPropagation({ causationId: "caus" }, () => {
      assert.equal(getCorrelationId(), "outer");
      assert.equal(getCausationId(), "caus");
    });
    assert.equal(getCorrelationId(), "outer");
  });
});

test("withPropagation nested override restores outer", () => {
  withPropagation({ correlationId: "outer" }, () => {
    withPropagation({ correlationId: "inner" }, () => {
      assert.equal(getCorrelationId(), "inner");
    });
    assert.equal(getCorrelationId(), "outer");
  });
});

test("propagation survives await", async () => {
  await withPropagation({ correlationId: "from-outside" }, async () => {
    await delay(1);
    assert.equal(getCorrelationId(), "from-outside");
  });
});

test("propagation copied into a child task", async () => {
  // setTimeout-via-Promise creates a new microtask continuation; Node's
  // AsyncLocalStorage carries the store across it — same property as
  // asyncio.create_task copying the contextvars Context.
  let captured = "";
  await withPropagation({ correlationId: "from-parent" }, async () => {
    await delay(1).then(() => {
      captured = getCorrelationId();
    });
  });
  assert.equal(captured, "from-parent");
});
