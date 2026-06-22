// Shared config for the SRE-01e load/soak harness. ONE place for the endpoints,
// the request mix, and — critically — the LATENCY-01 p99 budget, so the
// baseline run, the soak run, and the LATENCY-01a CI gate all assert the same
// number.
import http from "k6/http";

// The LATENCY-01 / ORCH-01f budget: read-path p99 ≤ 500ms. This is the same
// 500ms the risk-engine canary (CICD-01e) and the api-gateway query-latency SLO
// (SRE-01a) use — load testing must not invent a different target.
export const P99_BUDGET_MS = 500;
export const P95_BUDGET_MS = 250;
// Availability under load: error rate ≤ 0.1% (the SRE-01a gateway SLO). A 429/503
// from quota/admission is load-shed, counted via a separate metric below.
export const MAX_ERROR_RATE = 0.001;

export const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
export const PORTFOLIO = __ENV.PORTFOLIO || "PF1";

// Optional auth: the gateway edge chain (API-01d signing + AUTH-01) may require
// a bearer token / signed headers. Supply via env; absent in a plaintext dev
// gateway. Keep secrets out of the repo — pass at invocation.
const TOKEN = __ENV.TOKEN || "";

export function headers() {
  const h = { "Content-Type": "application/json" };
  if (TOKEN) h["Authorization"] = `Bearer ${TOKEN}`;
  return h;
}

// One iteration = the read-path request mix a client actually issues. Exposure
// and measures dominate; scenario is heavier and rarer. Returns the responses
// so callers can check them.
export function readMix() {
  const asOf = new Date().toISOString();
  const reqs = [
    ["GET", `${BASE_URL}/v1/portfolios/${PORTFOLIO}/exposure`, null],
    ["GET", `${BASE_URL}/v1/portfolios/${PORTFOLIO}/measures?as_of=${asOf}&measure=VaR99&measure=Delta`, null],
  ];
  // ~1 in 5 iterations also runs a scenario (the expensive POST).
  if (Math.random() < 0.2) {
    const body = JSON.stringify({
      shocks: [{ parallelShift: { pct: { coefficient: "-5", exponent: -2 } } }],
    });
    reqs.push(["POST", `${BASE_URL}/v1/portfolios/${PORTFOLIO}/scenario`, body]);
  }
  const params = { headers: headers() };
  return reqs.map(([method, url, body]) =>
    http.request(method, url, body, params)
  );
}

// Shared thresholds enforcing the budgets above. abortOnFail lets the baseline
// run stop at the capacity knee; the CI gate (LATENCY-01a) inherits these.
export const budgetThresholds = {
  http_req_duration: [
    { threshold: `p(99)<${P99_BUDGET_MS}`, abortOnFail: true },
    `p(95)<${P95_BUDGET_MS}`,
  ],
  http_req_failed: [{ threshold: `rate<${MAX_ERROR_RATE}`, abortOnFail: true }],
};
