# KANZ TASK BOARD

Task IDs are module-prefixed (`EVT`, `RISK`, `PRED`, `DATA`). Each item is scoped to 1–3 days. Epics are split into lettered subtasks.

---

## BACKLOG

### Event System

- [ ] **EVT-20a** Replay: Kafka log reader (offset/timestamp range → events) · `kanz/tools/replay/`
- [ ] **EVT-20b** Replay: isolated consumer-group / `replay.{run_id}.*` namespace wiring · `kanz/tools/replay/`
- [ ] **EVT-20c** Replay: `REPLAYED` flag stamping + live-sink hard-reject enforcement · `kanz/tools/replay/`
- [ ] **EVT-21a** Contract tests: envelope invariants (required fields, bad envelopes rejected) · `kanz/test/contract/`
- [ ] **EVT-21b** Contract tests: serialization round-trip parity (Go/Python/TS) · `kanz/test/contract/`
- [ ] **EVT-21c** Contract tests: schema compatibility both directions · `kanz/test/contract/`
- [ ] **EVT-21d** Contract tests: replay determinism (same log → identical output) · `kanz/test/contract/`

### Risk Engine

- [ ] **RISK-01** Define risk-engine module interface — narrow, versioned Go interface · `kanz/internal/risk/api/`
- [ ] **RISK-02** Add import-boundary architecture test (fails build on illegal imports) · `kanz/test/arch/`
- [ ] **RISK-03** Define core domain model types — portfolio, position, exposure, measure · `kanz/internal/risk/domain/`
- [ ] **RISK-04** Implement state ingestion consumer (domain/state events → engine) · `kanz/internal/risk/ingest/`
- [ ] **RISK-05** Implement per-aggregate ordered, idempotent state application · `kanz/internal/risk/state/`
- [ ] **RISK-06** Implement exposure computation · `kanz/internal/risk/compute/exposure.go`
- [ ] **RISK-07** Implement first concrete risk measure set (VaR + sensitivities) · `kanz/internal/risk/compute/`
- [ ] **RISK-08** Implement uncertainty propagation through measure computations · `kanz/internal/risk/compute/uncertainty.go`
- [ ] **RISK-09** Implement scenario / what-if engine · `kanz/internal/risk/scenario/`
- [ ] **RISK-10** Emit risk-output events (measure, value, uncertainty, source refs) · `kanz/internal/risk/publish/`
- [ ] **RISK-11** Implement degraded mode (cached/last-known + staleness tag) · `kanz/internal/risk/degraded.go`
- [ ] **RISK-12** Unit + property tests for measure correctness · `kanz/internal/risk/compute/`
- [ ] **RISK-13** Tests for degraded-mode behavior + uncertainty propagation · `kanz/internal/risk/`

### Go ↔ Python Prediction Layer

- [ ] **PRED-01** Define feature-vector + prediction-envelope proto schemas · `kanz-schemas/proto/inference/v1/`
- [ ] **PRED-02** Document degraded-mode contract (inference unavailable/slow/low-confidence) · `kanz-schemas/docs/inference-degraded-contract.md`
- [ ] **PRED-03** Implement Go-side feature event publishing · `kanz/internal/prediction/feature_publish.go`
- [ ] **PRED-04** Implement Python streaming inference worker (bus consumer) · `kanz-py/kanz_inference/streaming/`
- [ ] **PRED-05** Implement prediction-event publishing from Python workers · `kanz-py/kanz_inference/publish.py`
- [ ] **PRED-06** Define gRPC interface for interactive inference · `kanz-schemas/proto/inference/v1/inference_service.proto`
- [ ] **PRED-07** Implement Go-side sync inference client (timeout, circuit breaker, degraded fallback) · `kanz/internal/prediction/sync_client.go`
- [ ] **PRED-08** Implement isolated interactive inference worker pool + admission control · `kanz-py/kanz_inference/interactive/`
- [ ] **PRED-09** Implement model versioning + artifact registry · `kanz-py/kanz_inference/registry/`
- [ ] **PRED-10** Implement shadow / canary inference path · `kanz-py/kanz_inference/shadow/`
- [ ] **PRED-11** Define feature-store boundary interface (trivial first impl) · `kanz-py/kanz_inference/featurestore/`
- [ ] **PRED-12** Contract tests: feature/prediction schema versioning · `kanz/test/contract/inference/`
- [ ] **PRED-13** Tests: degraded-mode fallback + sync timeout/circuit-breaker · `kanz/internal/prediction/`
- [ ] **PRED-14** Tests: streaming worker backpressure (consumer-lag scaling) · `kanz-py/tests/`

### Data Integrity Layer

- [ ] **DATA-01** Implement `producer_sequence` gap detection · `kanz/internal/integrity/gap.go`
- [ ] **DATA-02** Implement staleness monitoring (`event_time` vs `ingestion_time`) · `kanz/internal/integrity/staleness.go`
- [ ] **DATA-03** Implement watermarking + late-data routing (`LATE` flag) · `kanz/internal/integrity/watermark.go`
- [ ] **DATA-04** Implement input distribution drift detection · `kanz/internal/integrity/drift.go`
- [ ] **DATA-05** Implement NATS-vs-Kafka reconciliation for correctness-sensitive consumers · `kanz/internal/integrity/reconcile.go`
- [ ] **DATA-06** Wire `quality_flags` emission across ingestion + producers · `kanz/internal/integrity/flags.go`
- [ ] **DATA-07** Emit data-quality events (gap, staleness, drift) to the bus · `kanz/internal/integrity/publish.go`
- [ ] **DATA-08** Build data-observability dashboards (freshness, completeness, drift) · `kanz/infra/observability/dashboards/`
- [ ] **DATA-09** Configure alerting on data-quality event thresholds · `kanz/infra/observability/alerts/`
- [ ] **DATA-10** Tests: gap/staleness/drift detection under simulated bad data · `kanz/internal/integrity/`
- [ ] **DATA-11** Tests: late-data path + reconciliation correctness · `kanz/internal/integrity/`

---

## TODO

_(none)_

---

## IN PROGRESS

_(none)_

---

## DONE

### Event System

- [x] **EVT-01** Initialize kanz-schemas repo — `buf.yaml`, `proto/` + `docs/` skeleton, `CODEOWNERS` per subtree · `kanz-schemas/`
- [x] **EVT-02** Define Envelope proto — `envelope.proto` (19 fields + `QualityFlag`), `event_class.proto` enum; `buf build` + `buf lint` pass · `kanz-schemas/proto/envelope/v1/`
- [x] **EVT-03** Write envelope versioning & field-inclusion policy doc · `kanz-schemas/docs/envelope-policy.md`
- [x] **EVT-04** Write per-class handling rules doc · `kanz-schemas/docs/event-class-rules.md`
- [x] **EVT-05** Write subject/topic taxonomy spec · `kanz-schemas/docs/subject-taxonomy.md`
- [x] **EVT-06** Write schema evolution rules doc — field-number discipline, version-bump triggers, compatibility direction · `kanz-schemas/docs/schema-evolution.md`
- [x] **EVT-07** Set up CI breaking-change + lint gate (`buf lint`, `buf breaking`); block PR on failure · `kanz-schemas/.github/workflows/schema-ci.yml`
- [x] **EVT-08** Provision NATS JetStream — k8s manifests (3-node JetStream cluster), idempotent stream/consumer bootstrap Job, pub/sub smoke test · `kanz/infra/nats/`
- [x] **EVT-09** Provision Kafka — k8s manifests (3-node KRaft cluster), idempotent topic/retention/compaction bootstrap Job, produce/consume smoke test · `kanz/infra/kafka/`
- [x] **EVT-10** Define market data payload schemas — `MarketDataEvent` + `MarketDataBatch` (Trade/Quote/Bar oneof); new shared `common.v1.Decimal` · `kanz-schemas/proto/market/v1/`, `kanz-schemas/proto/common/v1/`
- [x] **EVT-11** Define domain/state payload schemas — `PortfolioState`, `PositionState`, `PortfolioSnapshot`, `ExposureState`, `ExposureSet`; new shared `common.v1` `Money` + `LogPosition` · `kanz-schemas/proto/domain/v1/`, `kanz-schemas/proto/common/v1/`
- [x] **EVT-12** Define command + command-outcome payload schemas — generic `CommandMetadata` (embedded by concrete commands) + universal `CommandOutcome` / `CommandOutcomeStatus` · `kanz-schemas/proto/command/v1/`
- [x] **EVT-13** Define lifecycle payload schemas — `ModelDeployed`, `ConfigChanged`, `ModeChanged` (+ `DeploymentRole`, `OperatingMode` enums) · `kanz-schemas/proto/lifecycle/v1/`
- [x] **EVT-14** Define observation + data-quality payload schemas — `MetricObservation`, `DecisionLog`; `DataQualityEvent` (gap/staleness/drift oneof) + `Severity` · `kanz-schemas/proto/observation/v1/`
- [x] **EVT-15a** Codegen: `buf.gen.yaml` + Go plugin (managed-mode `go_package`); tag-triggered `schema-release.yml` publishes the Go module to companion repo `kanz-schemas-go` · `kanz-schemas/buf.gen.yaml`
- [x] **EVT-15b** Codegen: Python plugins (`protocolbuffers/python` + `pyi` remote); `schema-release.yml` `python` job builds the `kanz-schemas` package from `packaging/python/pyproject.toml` and publishes to the package index · `kanz-schemas/buf.gen.yaml`
- [x] **EVT-15c** Codegen: TS plugin (`bufbuild/es` remote, `target=js+dts`); `schema-release.yml` `ts` job builds the `@kanz-eng/kanz-schemas` package from `packaging/ts/package.json` and publishes to the npm registry · `kanz-schemas/buf.gen.yaml`
- [x] **EVT-16a** Schema registry: service skeleton (stdlib HTTP, `/healthz` + `/readyz`, slog, pgxpool) + storage backend (`Storage` interface with idempotent `Put`/`Get`/`Ping`, Postgres impl + in-memory fake for tests, `schemas` table keyed on `(schema_id, version)`) · `kanz/services/schema-registry/`
- [x] **EVT-16b** Schema registry ingest: `Storage.Register` (server-assigned version, idempotent on sha256 fingerprint, serializable tx in Postgres for atomic latest+1) + `POST /schemas/{schema_id}` HTTP endpoint. CI client `kanz-schemas/tools/registry-ingest/` reads the `buf build` FileDescriptorSet, slices a per-message subset (file + transitive imports), POSTs each. New `registry-ingest` job in `schema-release.yml` runs on tag push · `kanz/services/schema-registry/`, `kanz-schemas/tools/registry-ingest/`
- [x] **EVT-16c** Schema registry resolve: `GET /schemas/{ref}` returning raw stored descriptor with metadata in `X-Schema-*` headers + `ETag` and `Cache-Control: public, max-age=31536000, immutable` (rows are immutable once published); `404` for unknown refs, `400` for malformed; httptest-based tests cover happy path / 404 / malformed / version-0 / non-numeric version. Register handler now rejects `schema_id` containing `:` so resolve URLs stay unambiguous · `kanz/services/schema-registry/`
- [x] **EVT-17a** Go bus client foundation: `kanz/pkg/bus` package with `Message {Subject, Key, Body, Headers}` and unified `Client` interface (`Publisher` + `Subscriber` + `Close`). `NATSClient` over `nats.go` + JetStream v2 (durable AckExplicit consumer per (subject, group)); `KafkaClient` over `segmentio/kafka-go` (Writer with Hash balancer + RequireAll + Snappy + `AllowAutoTopicCreation=false`; per-(topic, group) Reader with manual per-message commit). `Kanz-Partition-Key` header bridges `Message.Key` over NATS (stripped from user-visible Headers on receive). Unit tests for config validation; integration tests gated on `TEST_NATS_URL` / `TEST_KAFKA_BROKERS` provision their own ephemeral stream/topic · `kanz/pkg/bus/`
- [x] **EVT-17b** Go bus client stamping + validation: `Producer` (over a `Client`) stamps auto-fields (event_id UUIDv7, publish_time, ingestion_time default-to-now, correlation_id default-to-event_id for roots, idempotency_key derivation per event-class rules, per-(event_type, partition_key) monotonic producer_sequence, source/producer_version from `ProducerConfig`, envelope_version=1). New `envelope.v1.EventFrame {Envelope envelope = 1; bytes payload = 2}` proto defines the wire frame so Envelope stays metadata-only and payload-blind tooling can deserialize the Envelope without the payload schema. `Validate` enforces every required-field rule from envelope-policy §7 + event-class-rules (FACT idempotency_key == event_id; live publish rejects QUALITY_FLAG_REPLAYED; producer_sequence==0 when partition_key empty). `Unframe` is the consumer-side counterpart. Table-driven tests across stamping behaviour, sequence semantics, and every validation rule · `kanz/pkg/bus/`, `kanz-schemas/proto/envelope/v1/event_frame.proto`
- [x] **EVT-17c** Go bus client lineage propagation: `Consumer` wraps a `Subscriber`, unframes + validates each inbound message, and stashes `correlation_id`, `event_id` (as the next event's `causation_id`), and `trace_context` onto the handler's `context.Context` via `With*`/`*FromContext` helpers. `Producer.stamp` reads those fields when the corresponding `Event` field is empty (precedence: explicit > ctx > root default). End-to-end test asserts a Consumer→Producer chain inherits all three fields; unit tests cover unframe failure, validation failure, handler-error surfacing, empty-trace skip, and explicit-beats-ctx precedence · `kanz/pkg/bus/`
- [x] **EVT-17d** Go bus client dedup: two reinforcing layers. **Publish side** — Producer stamps `Nats-Msg-Id: <idempotency_key>` on every outbound `bus.Message`; NATS JetStream uses this for broker-side dedup within its 2m window (EVT-08), Kafka carries it as a benign user header. **Consumer side** — bounded sliding `DedupWindow` (`Seen`/`Record`, 2m / 10k default; functional option `WithDedupWindow` to override or disable). Pipeline: `Unframe → Validate → Seen ⇒ skip ack` else `dispatch → Record on success`; failure leaves the window untouched so retries work. Methods are nil-safe so the zero-config (disabled) path needs no nil-checks. Tests cover Seen/Record split, TTL expiry, capacity eviction (oldest first), nil-safety, dedup-on-redelivery, no-record-on-handler-failure, dedup-disabled, distinct-keys-distinct-dispatch, and publish-side header stamping · `kanz/pkg/bus/`
- [x] **EVT-17e** Go bus client retry + DLQ: `RetryConfig {MaxAttempts, InitialBackoff, MaxBackoff}` drives an in-handler retry loop with exponential backoff (capped, ctx-cancelable; no jitter at this layer). `WithRetry(cfg)` and `WithDLQ(Publisher)` are independent options — either composable on its own. Terminal failures (retry exhausted, or `Unframe` / `Validate` failure before dispatch) republish the original message to `dlq.<original-subject>` with `Kanz-DLQ-{Original-Subject,Attempts,Error}` headers attached and the original wire body/headers preserved, then ack. DLQ-publish failure surfaces so the broker holds the message. DLQ recording into the dedup window treats DLQ as terminal so subsequent duplicates skip. Tests cover retry-until-success, retry-then-surface (no DLQ), retry-then-DLQ, unframe→DLQ, validate→DLQ, DLQ-records-dedup, ctx-cancel-during-backoff, and DLQ-publish-failure surfaces · `kanz/pkg/bus/`
- [x] **EVT-18a** Python bus client foundation: new `kanz-py` package (`kanz-bus` on PyPI). `kanz_bus.bus` defines `Message`, `Handler`, and `Publisher`/`Subscriber`/`Client` Protocols mirroring the Go shape. `NATSClient` over `nats-py` (JetStream pull-subscribe loop with timeout-retry, durable per (subject, group), handler exception ⇒ `nak`); `KafkaClient` over `aiokafka` (Producer `acks="all"` + Snappy, per-call Consumer with `enable_auto_commit=False` and manual commit on success). Async-first API with `async with` context-manager lifecycle and `asyncio.CancelledError` as the cancel signal. `Kanz-Partition-Key` header bridges `Message.key` over NATS (stripped from user-visible headers on receive) — same wire format as Go. Unit tests for config + lifecycle; integration tests gated on `TEST_NATS_URL` / `TEST_KAFKA_BROKERS` provision their own ephemeral stream/topic · `kanz-py/`
- [x] **EVT-18b** Python bus client stamping + validation: `Producer` (over a `Client`) + `ProducerConfig{source, producer_version}` + `Event` dataclass mirror the Go EVT-17b shape — wire-identical `EventFrame` framing, same `Nats-Msg-Id` header, same per-(event_type, partition_key) monotonic `producer_sequence`. Inline UUIDv7 implementation (Python 3.13's stdlib `uuid.uuid7` is too new for the 3.10+ floor; ~10 lines of bit-packing avoids an extra dep). `validate(envelope)` enforces every required-field rule from envelope-policy §7 + the FACT idempotency-key invariant + live-publish rejection of `QUALITY_FLAG_REPLAYED`. `unframe(body)` is the consumer-side helper. New deps: `kanz-schemas>=0.1.0`, `protobuf>=5.29`. Parameterized validate tests cover every required field; producer tests cover stamping, sequence semantics, COMMAND idempotency, FACT mismatch rejection, None-event-time/payload rejection, and broker-dedup-header stamping · `kanz-py/`
- [x] **EVT-18c** Python bus client lineage propagation: lineage rides `contextvars.ContextVar` (PEP 567) — the Python analog of Go's `context.Context`. `kanz_bus.propagation` exposes `propagation_context(*, correlation_id="", causation_id="", trace_context="")` (sync context manager that sets non-empty fields and resets via the contextvars Token on exit, even in async code) and three `get_*` readers. `Consumer` wraps a `Subscriber`, unframes + validates each delivery, then stashes `env.correlation_id`, `env.event_id` (as the *next* event's causation_id), and non-empty `env.trace_context` for the handler's duration — so `Producer.publish` inside the handler auto-inherits via the contextvars. `EventHandler = Callable[[Envelope, bytes], Awaitable[None]]` has no explicit ctx arg (idiomatic Python; lineage is implicit). `Producer.stamp` precedence is explicit Event field > contextvar > root default. End-to-end Consumer→Producer chain test asserts inheritance; unit tests cover contextvar set/reset/nest semantics, empty-arg non-overwriting, asyncio.create_task copy semantics, and unframe/validate failure surfacing · `kanz-py/`
- [x] **EVT-18d** Python bus client dedup + retry + DLQ: combines Go EVT-17d+17e into one subtask per the Python plan. `DedupWindow{ttl_seconds, max_entries}` (default 2m/10k, `ttl_seconds<=0` ⇒ disabled-mode no-op instance — Python's nil-safe-method analog). `RetryConfig{max_attempts, initial_backoff_seconds, max_backoff_seconds}` exponential backoff capped at `max_backoff_seconds`; ctx-cancelable via `asyncio.sleep`. `ConsumerConfig{dedup_window, retry, dlq}` dataclass — composable; any field omitted defaults to the safe value. Pipeline: `unframe → validate → seen ⇒ skip ack` else `retry-loop(handler) → record on success / DLQ-then-record on exhausted`. Pre-dispatch failures (`unframe`/`validate`) route to DLQ with `Kanz-DLQ-Attempts: 0`. DLQ subject = `dlq.<original>`, `Kanz-DLQ-{Original-Subject,Attempts,Error}` headers + original body preserved. DLQ publish failure surfaces (broker holds the message). `asyncio.CancelledError` propagates out of the retry loop without being swallowed by `except Exception`. Tests: dedup seen/record + TTL + eviction + disabled-mode; retry backoff math; consumer retry-until-success / surface-after-retry / route-to-DLQ-after-retries / unframe→DLQ / validate→DLQ / dedup-on-redelivery / no-record-on-failure / dedup-disabled / DLQ-records-dedup / DLQ-publish-failure-surfaces · `kanz-py/`
- [x] **EVT-19a** TS bus client foundation: new `kanz/ts/kanz-bus/` package (`@kanz-eng/kanz-bus` on npm). NodeNext ESM, strict TS 5.6, `node:test` via `tsx` (zero runtime test deps). `src/bus.ts` defines `Message {subject, body, key?, headers?}`, `Handler = (Message) => Promise<void>`, and `Publisher`/`Subscriber`/`Client` interfaces mirroring the Go/Python shape. `NATSClient` over `nats@2` (JetStream consumer via `js.consumers.get(stream, group)` with idempotent durable-consumer upsert at AckExplicit, modern `consume()` async-iterator loop, handler throw ⇒ `nak`); `KafkaClient` over `kafkajs@2` + `kafkajs-snappy` (single shared `Producer` with `allowAutoTopicCreation: false` + `acks: -1` + Snappy compression matching the Go writer; per-Subscribe `Consumer` with `autoCommit: false` and manual per-message `commitOffsets` of `offset+1` on success). `AbortSignal` is the TS analog of Go ctx / Python `CancelledError` — `subscribe` resolves when signal aborts. `Kanz-Partition-Key` header bridges `Message.key` over NATS (stripped from user-visible headers on receive) — wire-identical to Go/Python. Unit tests for config validation, default application, lifecycle (`publish` before `connect` throws, `close` without `connect` is a no-op) · `kanz/ts/kanz-bus/`
- [x] **EVT-19b** TS bus client stamping + validation: `Producer` (over a `Client`) + `ProducerConfig{source, producerVersion}` + `Event` interface mirror Go EVT-17b / Python EVT-18b — wire-identical `EventFrame` framing, same `Nats-Msg-Id` header, same per-(event_type, partition_key) monotonic `producerSequence` (`bigint`, since uint64 in Protobuf-ES v2 surfaces as bigint). New deps: `@bufbuild/protobuf@^2.2.3` + `@kanz-eng/kanz-schemas@^0.1.0`. Inline UUIDv7 via `node:crypto`'s `randomBytes` (~10 lines) — `crypto.randomUUID` is v4-only as of Node 24. `validate(env)` throws `Error` (TS idiom) where Go returns error / Python raises ValueError — same rules, parallel messages. `unframe(body)` deserializes the consumer-side `EventFrame`. `PayloadWithSchema<Desc>` bundles a protobuf descriptor + its `MessageShape<Desc>` so Producer.publish can serialize the payload with the right schema while preserving type safety. Minimal `propagation.ts` ships the three getters (`getCorrelationId/CausationId/TraceContext`) backed by `AsyncLocalStorage`; the setter API + Consumer integration land in EVT-19c. Producer.publish currently auto-inherits from any pre-populated async-store, falling back to root defaults. Tests: full Python-mirror suite for validate (canonical + every required field + FACT-mismatch + REPLAYED + sequence-without-pk + COMMAND-with-caller-key); producer (root-stamping, explicit causation, sequence-per-pk, sequence-zero-without-pk, COMMAND-requires-idempotency, COMMAND-preserves-caller-key, FACT-rejects-mismatch, missing-eventTime, missing-payload, constructor validation, Nats-Msg-Id stamping, root correlation defaults to event_id); uuidv7 canonical-form/sortability/distinctness · `kanz/ts/kanz-bus/`
- [x] **EVT-19c** TS bus client lineage propagation: `withPropagation(ctx, fn)` wraps `AsyncLocalStorage.run` with the same "non-empty fields only" rule as Go's `WithTraceContext` skip-on-empty and Python's `propagation_context` partial-set. TS API shape is a function-taking wrapper (not a context manager / decorator) because `AsyncLocalStorage.run(store, callback)` already binds the store to a callback — the wrapper just computes `next = {explicit || outer || ""}` per-field and forwards. `Consumer` wraps a `Subscriber`, runs `unframe → validate → withPropagation({correlationId, causationId: env.eventId, traceContext}, () => handler(env, payload))`; pre-dispatch errors propagate out so the underlying NATSClient/KafkaClient triggers broker redelivery (nak / skip-commit) — bounded retry + DLQ are outside EVT-19 scope per the task board, so handlers must be idempotent (event-class-rules §1) and broker redelivery is the only recovery primitive. `EventHandler = (Envelope, Uint8Array) => Promise<void>` has no explicit context arg — lineage is implicit via the async-store, same idiom as Python's `EventHandler`. End-to-end Consumer→Producer chain test asserts inheritance of correlation/causation/trace; tests also cover explicit-beats-store precedence, empty-trace-doesn't-clobber-outer, unframe/validate failure surfacing with handler-not-called, async-store reset after handler, and propagation persisting across `await` + a `.then()`-chained child task (TS analog of asyncio.create_task copy semantics). EVT-19 epic complete: TS client is now end-to-end runnable and wire-compatible with Go EVT-17 + Python EVT-18 · `kanz/ts/kanz-bus/`
