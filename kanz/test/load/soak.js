// SRE-01e — soak.
//
// Holds a steady, sub-capacity load for a long duration to catch what a short
// burst can't: memory/goroutine leaks, connection-pool exhaustion, GC pressure,
// p99 drift, and slow store growth. Constant-arrival-rate (open model) at ~60%
// of the baseline.js capacity — the system should hold the LATENCY-01 p99 budget
// flat for the whole window, not just at the start.
//
//   k6 run -e BASE_URL=https://gw.kanz.example -e RATE=300 -e DURATION=2h soak.js
//
// The tell is DRIFT: if p99 is fine at minute 5 and breaching at minute 90, the
// threshold catches the leak. Watch trend, not just the final number.
import { check } from "k6";
import { Trend } from "k6/metrics";
import {
  readMix,
  P99_BUDGET_MS,
  P95_BUDGET_MS,
  MAX_ERROR_RATE,
} from "./config.js";

// Separate trend so soak drift is visible distinct from the baseline run.
const soakLatency = new Trend("soak_req_duration", true);

const RATE = Number(__ENV.RATE || 300); // ~60% of measured baseline capacity
const DURATION = __ENV.DURATION || "1h";

export const options = {
  scenarios: {
    soak: {
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: Math.ceil(RATE * 0.5),
      maxVUs: RATE * 4,
    },
  },
  thresholds: {
    // Budget must hold for the WHOLE soak (no abortOnFail — we want the full
    // trend, and a CI soak fails on the threshold verdict at the end).
    http_req_duration: [`p(99)<${P99_BUDGET_MS}`, `p(95)<${P95_BUDGET_MS}`],
    http_req_failed: [`rate<${MAX_ERROR_RATE}`],
    soak_req_duration: [`p(99)<${P99_BUDGET_MS}`],
  },
};

export default function () {
  for (const res of readMix()) {
    check(res, { "status 2xx": (r) => r.status >= 200 && r.status < 300 });
    soakLatency.add(res.timings.duration);
  }
}
