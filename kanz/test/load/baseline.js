// SRE-01e — baseline capacity.
//
// Ramps request rate up until the LATENCY-01 p99 budget (or the error-rate
// budget) breaks: the throughput at the breaking point IS the baseline
// capacity. ramping-arrival-rate drives a target REQUEST RATE (open model), so
// the result is "RPS the system sustains within budget" — the number capacity
// planning and the SRE-01a SLOs are sized against — not "how fast N fixed VUs
// happen to go".
//
//   k6 run -e BASE_URL=https://gw.kanz.example baseline.js
//
// abortOnFail (config.js thresholds) stops the run the moment p99 or error rate
// breaches, so the last fully-passing stage is the capacity headline. Read it
// off `http_reqs` rate at abort, or the last stage before the threshold goes red.
import { check } from "k6";
import { Counter } from "k6/metrics";
import { readMix, budgetThresholds } from "./config.js";

const shed = new Counter("requests_shed"); // 429/503 load-shed, tracked apart from errors

export const options = {
  scenarios: {
    capacity_ramp: {
      executor: "ramping-arrival-rate",
      startRate: 50,
      timeUnit: "1s",
      preAllocatedVUs: 50,
      maxVUs: 2000,
      // Climb in steady steps; each plateau holds long enough for p99 to settle
      // before the next push, so the breaking point is real load, not a spike.
      stages: [
        { target: 100, duration: "1m" },
        { target: 250, duration: "2m" },
        { target: 500, duration: "2m" },
        { target: 1000, duration: "2m" },
        { target: 2000, duration: "2m" },
        { target: 4000, duration: "2m" },
      ],
    },
  },
  thresholds: budgetThresholds,
};

export default function () {
  for (const res of readMix()) {
    check(res, { "status 2xx": (r) => r.status >= 200 && r.status < 300 });
    if (res.status === 429 || res.status === 503) shed.add(1);
  }
}
