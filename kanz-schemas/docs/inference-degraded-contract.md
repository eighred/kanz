# Inference Degraded-Mode Contract

Governs how the prediction layer behaves when it cannot serve a fresh,
fully-trusted prediction — and what consumers MUST do with the response.

Companion to `proto/inference/v1/prediction.proto` (PRED-01) and the
hybrid streaming + sync-gRPC architecture described in KANZ_BRAIN.

## 1. The core rule

The prediction layer **never returns "no prediction"** under degraded
conditions. It always returns a `PredictionEnvelope` and sets `mode`
honestly — `NORMAL` for the full-trust path, `DEGRADED` for everything
else. The consumer branches on `mode` BEFORE acting.

Black-holing a request (refusing to respond, returning an error) is a
correctness regression: the caller is left blind, can't distinguish "the
model is silent" from "the model said no", and has no signal to fall
back on its own logic. Degraded-with-explicit-flag is always strictly
better than no-response.

## 2. Three degraded triggers

These are the only conditions under which the inference layer SHALL
set `mode = PREDICTION_MODE_DEGRADED`:

### 2.1 Inference unavailable

The model server is down, the gRPC channel is broken, or the bus
consumer-lag has exceeded the operating budget. The inference layer
returns the **last-known-good prediction** for the subject from its
cache (PRED-04) and sets `degraded_reason` to one of:

- `inference_unavailable` — model server unreachable
- `bus_lag_exceeded` — async path back-pressured beyond budget
- `circuit_open` — sync-path circuit breaker tripped (PRED-07)
- `pool_saturated` — interactive worker pool is at admission limit
  (PRED-08); the request was admit-rejected rather than queued
  indefinitely

If no cache entry exists for the subject (cold cache, first-ever
request), the layer returns `value = 0`, `confidence = 0`,
`degraded_reason = "no_cached_prediction"`. The consumer treats this as
"no information, do not act."

### 2.2 Inference slow

Sync gRPC calls exceeding the per-call timeout budget (PRED-07) fall
back to cache rather than blocking the caller indefinitely. The
streaming path's equivalent is consumer-lag scaling (PRED-14) —
slowness routes back to 2.1 once the lag exceeds budget.

`degraded_reason = "inference_timeout"` with the timeout value in
the explanation map (e.g. `{"budget_ms": "50"}`).

### 2.3 Low confidence

The model produced a prediction but its calibrated confidence is below
the per-model threshold. The threshold is the model's contract (set in
the model artifact metadata, PRED-09), not a global constant — a
high-conviction signal model may set 0.8; a probability-of-default
model may set 0.3.

`degraded_reason = "low_confidence"`; `confidence` field carries the
actual value. A model that does NOT expose calibrated confidence is
treated as ALWAYS low-confidence — sets `confidence = 0` and `mode =
DEGRADED` on every response (see §1 — "unknown confidence" must register
as "no confidence", not be silently mapped to high).

## 3. Producer obligations

The inference layer (PRED-04 / PRED-08) MUST:

- Always return a `PredictionEnvelope` — never black-hole.
- Set `mode` honestly: `DEGRADED` if any §2 trigger fires, `NORMAL`
  otherwise. `UNSPECIFIED` is never set by a producer.
- Set `degraded_reason` to a stable identifier from §2 (machine-
  readable; downstream alerts match on it). Free-form context goes in
  `explanation` as a sidecar map, not in `degraded_reason`.
- Emit one `observation.v1.DecisionLog` per degraded response with the
  same `degraded_reason` so the audit trail captures it independently
  of whether the consumer logged it.
- For the streaming path, set the envelope `quality_flags` to include
  `QUALITY_FLAG_DEGRADED` when `mode == DEGRADED` — so payload-blind
  observability tooling sees the signal without parsing the payload.

## 4. Consumer obligations

Consumers of `PredictionEnvelope` MUST:

- Branch on `mode` BEFORE reading `value`. A consumer that ignores
  `mode` and acts on the `value` field is using degraded predictions as
  if they were fresh — a correctness bug.
- Have a documented fallback for `mode == DEGRADED` per use case. A
  hot-path consumer may refuse to trade on degraded signals; a
  reporting consumer may surface them with a UI flag; a backtest
  consumer may ignore them entirely.
- Treat `mode == UNSPECIFIED` as `DEGRADED` (defensive — a producer
  that fails to set `mode` is broken, but the consumer should not
  silently treat the response as full-trust).
- Never elevate a `DEGRADED` response to `NORMAL` based on the value
  field alone. The producer made the trust call; consumers don't
  override it.

## 5. Mode is per-response, not per-subject

A subject can receive `NORMAL` predictions and `DEGRADED` ones
interleaved — the inference layer makes the trust call PER request,
not per subject. A model that just rolled back may serve some
subjects normally (cached predictions from the previous version are
still trustworthy) and others degraded (no cache for newly-active
subjects). Consumers must not assume "this subject is in degraded
mode" — that state is per-response.

## 6. Cross-path uniformity

Both the streaming path (bus consumer) and the sync gRPC path (PRED-06
/ PRED-07) follow this contract identically. A streaming consumer and a
sync caller seeing the same subject at the same as_of MUST observe the
same `mode` + `degraded_reason` — the trust decision is made by the
inference layer, not the transport.

The implementation reuses one decision function across both paths
(PRED-04 + PRED-07 share the same trigger logic). Divergence between
the two would let a consumer route around degraded mode by switching
paths, defeating the contract.

## 7. What this doc does NOT govern

- Specific timeout values, cache TTLs, or threshold choices — those are
  per-deployment operational tuning, not contract.
- Feature-staleness signalling — features have their own freshness
  conventions (DATA-02/03), the prediction layer reads them but does
  not redefine them.
- Model-version pinning or shadow-traffic policy — those are PRED-09
  and PRED-10 concerns.
- The risk engine's `DEGRADED` mode (RISK-11). Prediction and risk
  degraded modes are independent — a risk query can be NORMAL even
  while its inputs include DEGRADED predictions, and vice versa.
