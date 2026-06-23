// LATENCY-01a — CI smoke gate.
//
// A SHORT (~60s), low-rate constant-arrival run that asserts the same
// LATENCY-01 p99/error budgets as the full baseline (config.js budgetThresholds,
// abortOnFail). This is the per-PR regression gate: it runs against the
// ephemeral stack (kanz/test/load/docker-compose.yml) and fails the pipeline the
// moment p99 or the error rate breaches budget — no separate assertion to keep
// in sync.
//
// Why a smoke, not baseline.js: the capacity ramp takes ~11 min and pushes to
// 4000 rps to find the knee — meaningful against a real deployed env, but slow
// and p99-noisy as a per-PR gate on a shared runner. The smoke holds a modest,
// sustainable rate just long enough to catch a real read-path latency
// regression. The full ramp stays a manual/scheduled workflow_dispatch run; the
// in-process micro-benchmark guard is LATENCY-01d (`testing.B`).
//
//   k6 run -e BASE_URL=http://localhost:8080 smoke.js
import { check } from "k6";
import { readMix, budgetThresholds } from "./config.js";

// Modest, sustainable load — well under the toy-stack knee, so a passing run
// reflects code latency, not the CI runner's capacity ceiling. Overridable for
// a heavier manual smoke.
const RATE = Number(__ENV.SMOKE_RATE || 30); // requests/s
const DURATION = __ENV.SMOKE_DURATION || "1m";

export const options = {
  scenarios: {
    smoke: {
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: Math.ceil(RATE),
      maxVUs: RATE * 4,
      // Brief grace so the connection warm-up / first-RPC dial cost doesn't
      // count against the p99 the gate asserts.
      gracefulStop: "5s",
    },
  },
  thresholds: budgetThresholds,
};

export default function () {
  for (const res of readMix()) {
    check(res, { "status 2xx": (r) => r.status >= 200 && r.status < 300 });
  }
}
