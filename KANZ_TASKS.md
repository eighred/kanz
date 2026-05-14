# KANZ TASK BOARD

Task IDs are module-prefixed (`EVT`, `RISK`, `PRED`, `DATA`). Each item is scoped to 1–3 days. Epics are split into lettered subtasks.

---

## BACKLOG

### Event System

- [ ] **EVT-10** Define market data payload schemas — `MarketDataEvent` + `MarketDataBatch` · `kanz-schemas/proto/market/v1/`
- [ ] **EVT-11** Define domain/state payload schemas — portfolio, position, exposure events · `kanz-schemas/proto/domain/v1/`
- [ ] **EVT-12** Define command + command-outcome payload schemas — `kanz-schemas/proto/command/v1/`
- [ ] **EVT-13** Define lifecycle payload schemas — `ModelDeployed`, `ConfigChanged`, `ModeChanged` · `kanz-schemas/proto/lifecycle/v1/`
- [ ] **EVT-14** Define observation + data-quality payload schemas — `kanz-schemas/proto/observation/v1/`
- [ ] **EVT-15a** Codegen: configure `buf.gen.yaml` + Go plugin, publish Go module · `kanz-schemas/buf.gen.yaml`
- [ ] **EVT-15b** Codegen: add Python plugin, publish Python package · `kanz-schemas/buf.gen.yaml`
- [ ] **EVT-15c** Codegen: add TS plugin, publish TS package · `kanz-schemas/buf.gen.yaml`
- [ ] **EVT-16a** Schema registry: service skeleton + schema storage backend · `kanz/services/schema-registry/`
- [ ] **EVT-16b** Schema registry: ingest schemas from kanz-schemas CI on release · `kanz/services/schema-registry/`
- [ ] **EVT-16c** Schema registry: `resolve(payload_schema_ref)` endpoint + tests · `kanz/services/schema-registry/`
- [ ] **EVT-17a** Go client: connection mgmt + publish/subscribe wrappers (NATS + Kafka) · `kanz/pkg/bus/`
- [ ] **EVT-17b** Go client: envelope stamping + pre-publish validation · `kanz/pkg/bus/`
- [ ] **EVT-17c** Go client: trace/correlation/causation auto-propagation · `kanz/pkg/bus/`
- [ ] **EVT-17d** Go client: dedup window + idempotency handling · `kanz/pkg/bus/`
- [ ] **EVT-17e** Go client: DLQ routing + bounded retry · `kanz/pkg/bus/`
- [ ] **EVT-18a** Python client: connection + publish/subscribe wrappers · `kanz-py/kanz_bus/`
- [ ] **EVT-18b** Python client: envelope stamping + validation · `kanz-py/kanz_bus/`
- [ ] **EVT-18c** Python client: trace/correlation/causation propagation · `kanz-py/kanz_bus/`
- [ ] **EVT-18d** Python client: dedup + DLQ routing · `kanz-py/kanz_bus/`
- [ ] **EVT-19a** TS client: connection + subscribe wrappers · `kanz/ts/kanz-bus/`
- [ ] **EVT-19b** TS client: envelope parsing + validation · `kanz/ts/kanz-bus/`
- [ ] **EVT-19c** TS client: trace context propagation · `kanz/ts/kanz-bus/`
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

### Event System

- [ ] **EVT-06** Write schema evolution rules doc — field-number discipline, version-bump triggers, compatibility direction · `kanz-schemas/docs/schema-evolution.md`
- [ ] **EVT-07** Set up CI breaking-change + lint gate (`buf lint`, `buf breaking`); block PR on failure · `kanz-schemas/.github/workflows/schema-ci.yml`
- [ ] **EVT-08** Provision NATS JetStream — deployment manifests + stream/consumer config; pub/sub smoke test · `kanz/infra/nats/`
- [ ] **EVT-09** Provision Kafka — deployment manifests + topics, retention, compaction; produce/consume smoke test · `kanz/infra/kafka/`

---

## IN PROGRESS

### Event System

- [ ] **EVT-02** Define Envelope proto — files written (`envelope.proto` 19 fields + `QualityFlag`, `event_class.proto` enum, fully commented); `buf build && buf lint` verification pending (buf not installed locally) · `kanz-schemas/proto/envelope/v1/`

---

## DONE

### Event System

- [x] **EVT-01** Initialize kanz-schemas repo — `buf.yaml`, `proto/` + `docs/` skeleton, `CODEOWNERS` per subtree · `kanz-schemas/`
- [x] **EVT-03** Write envelope versioning & field-inclusion policy doc · `kanz-schemas/docs/envelope-policy.md`
- [x] **EVT-04** Write per-class handling rules doc · `kanz-schemas/docs/event-class-rules.md`
- [x] **EVT-05** Write subject/topic taxonomy spec · `kanz-schemas/docs/subject-taxonomy.md`
