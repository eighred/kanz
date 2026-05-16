import assert from "node:assert/strict";
import { test } from "node:test";

import { uuidv7 } from "../src/index.js";

test("uuidv7 has canonical form and version/variant bits", () => {
  const id = uuidv7();
  assert.match(
    id,
    /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
  );
});

test("uuidv7 values are time-sortable across calls", async () => {
  const a = uuidv7();
  await new Promise((r) => setTimeout(r, 2));
  const b = uuidv7();
  // Time-prefix dominates the first 48 bits, so lexicographic order = time order.
  assert.ok(a < b, `${a} < ${b}`);
});

test("uuidv7 returns distinct values when called rapidly", () => {
  const ids = new Set<string>();
  for (let i = 0; i < 100; i++) ids.add(uuidv7());
  assert.equal(ids.size, 100);
});
