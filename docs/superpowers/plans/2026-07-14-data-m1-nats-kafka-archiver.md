# DATA-M1 — NATS→Kafka Archiver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Write the missing producer that makes Kafka the durable log of record it is already documented, provisioned, DR-replicated and consumed as — so the platform stops losing every event older than 24 hours.

**Architecture:** A new per-tenant service `services/archiver` runs one durable NATS consumer per in-scope stream, maps each envelope to its Kafka topic (`{tenant}.{domain}.{entity}`, derived from the envelope's `event_type`), produces the envelope bytes **verbatim** keyed on `partition_key`, and acks NATS **only after Kafka acknowledges**. Single writer (`replicas: 1`, `Recreate`) so per-key order is correct by construction; a gap is deferred work, not loss, because NATS retains 24–168h and the pod catches up on restart. Alongside it, the Kafka topic topology is rebuilt from ground truth and locked in place by a cross-language arch test.

**Tech Stack:** Go 1.23+, `pkg/bus` (NATS JetStream + `segmentio/kafka-go` via `bus.KafkaClient`), protobuf envelopes (`kanz-schemas-go/envelope/v1`), Prometheus metrics via `pkg/bus.BusMetrics`, Kubernetes + Vault CSI, GitHub Actions with Kafka + NATS service containers.

**Spec:** `docs/superpowers/specs/2026-07-14-data-m1-nats-kafka-archiver-design.md`

## Global Constraints

- **The archiver never rewrites an envelope.** Produce `msg.Body` verbatim. It is a transport, not a producer; a component that re-stamps the log of record on its way into the log of record cannot be trusted as a record of what happened.
- **Ack only after Kafka acknowledges.** Any failure — mapping, produce, tenant mismatch — returns a non-nil error from the `bus.Handler`, which NACKs and redelivers. Never ack-then-produce.
- **Fail closed, everywhere.** Unmappable event, unknown tenant, missing topic ⇒ error + NACK + loud log. Never fall back to the un-prefixed `__system__` topic (that breaches the Kafka PREFIXED-ACL isolation boundary).
- **Delivery is at-least-once.** `event_id` is the dedup key, carried as the Kafka header `Kanz-Event-Id`. Do **not** add a dedup window on the consumer — dropping a redelivery that was never produced is exactly the data loss this task exists to end.
- **Single writer per stream.** Never scale one stream's consumer to >1 replica; two producers can invert per-key order in Kafka, and a reordered log rebuilds a *different* book.
- Go module root is `kanz/`. Build with `GOFLAGS=-mod=mod`. All Go commands run from `kanz/`.
- Env var prefix is `ARCHIVER_`. Service refuses to start (`os.Exit(2)`) if `ARCHIVER_TENANT`, `ARCHIVER_NATS_URL` or `ARCHIVER_KAFKA_BROKERS` is unset.

### Bus API you will use (exact signatures)

```go
// Dial
func bus.DialNATS(ctx context.Context, cfg bus.NATSConfig) (*bus.NATSClient, error) // NATSConfig{URL, Name}
func bus.DialKafka(cfg bus.KafkaConfig) (*bus.KafkaClient, error)                   // KafkaConfig{Brokers, ClientID}

// Subscribe — raw wire level. Returning a non-nil error NACKs (NATS redelivers).
func (c *bus.NATSClient) Subscribe(ctx context.Context, subject, group string, h bus.Handler) error
type bus.Handler func(ctx context.Context, msg bus.Message) error

// Publish — msg.Subject IS the Kafka topic; msg.Key IS the Kafka message key.
func (k *bus.KafkaClient) Publish(ctx context.Context, msg bus.Message) error
type bus.Message struct { Subject string; Key []byte; Body []byte; Headers map[string]string }

// Parse the envelope out of a wire body (does not copy/modify Body).
func bus.Unframe(body []byte) (*envelopepb.Envelope, []byte, error)

// Metrics
func bus.NewBusMetrics(reg prometheus.Registerer) *bus.BusMetrics
func (m *bus.BusMetrics) SetPending(subject, group string, pending float64)
```

**Why the raw `Subscriber` and not `bus.Consumer`:** `bus.Consumer` unframes into `(env, payload)` and offers a dedup window. Re-marshalling would violate the verbatim constraint, and its dedup window could silently drop a redelivery that was never produced. The archiver uses the raw handler and unframes only to *read routing fields*.

**Envelope fields used:** `GetEventType()` (full 3-segment logical name), `GetEventClass()`, `GetTenantId()`, `GetPartitionKey()`, `GetEventId()`. Snapshot enum: `envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT`.

---

## File Structure

| File | Responsibility |
|---|---|
| `kanz/services/archiver/internal/topic/topic.go` | **Pure mapping.** Envelope → Kafka topic. No I/O. |
| `kanz/services/archiver/internal/topic/topic_test.go` | Unit tests for every mapping + fail-closed case. |
| `kanz/services/archiver/internal/archive/archiver.go` | The fold: subscribe → map → produce → ack. |
| `kanz/services/archiver/internal/archive/archiver_test.go` | Unit tests with fakes, incl. ack-only-after-produce. |
| `kanz/services/archiver/internal/archive/archiver_integration_test.go` | Real NATS + real Kafka: payoff, ordering, outage, catch-up, red twin. |
| `kanz/services/archiver/internal/config/config.go` | Env config; fail closed on missing required vars. |
| `kanz/services/archiver/internal/config/config_test.go` | Fail-closed tests. |
| `kanz/services/archiver/cmd/archiver/main.go` | Composition root. |
| `kanz/services/archiver/Dockerfile` | Distroless image. |
| `kanz/test/arch/kafka_topology_test.go` | **The durable guard.** Cross-language: every subject has a topic. |
| `kanz/infra/kafka/topics-job.yaml` | Topic topology rebuilt from ground truth. |
| `kanz/infra/deploy/archiver-deploy.yaml` | Deployment (replicas 1, Recreate) + Vault CSI. |
| `kanz/infra/security/runtime/network-policies.yaml` | Archiver egress: NATS + Kafka only. |
| `.github/workflows/kanz-ci.yml`, `.github/workflows/build.yml` | CI: test with Kafka+NATS containers; build image. |

---

## Task 1: The topic mapping

The pure unit everything else depends on. Built first, fully tested, no I/O.

**Files:**
- Create: `kanz/services/archiver/internal/topic/topic.go`
- Test: `kanz/services/archiver/internal/topic/topic_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `topic.For(env *envelopepb.Envelope, tenant string) (string, error)` — the only export. `tenant` is the archiver's configured tenant (`ARCHIVER_TENANT`).

- [ ] **Step 1: Write the failing test**

Create `kanz/services/archiver/internal/topic/topic_test.go`:

```go
package topic_test

import (
	"strings"
	"testing"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/services/archiver/internal/topic"
)

func env(eventType, tenant string, class envelopepb.EventClass) *envelopepb.Envelope {
	return &envelopepb.Envelope{EventType: eventType, TenantId: tenant, EventClass: class}
}

const fact = envelopepb.EventClass_EVENT_CLASS_FACT
const snapshot = envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT

func TestFor_TenantPrefixed(t *testing.T) {
	got, err := topic.For(env("order.order.submitted", "acme", fact), "acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if want := "acme.order.order"; got != want {
		t.Fatalf("topic = %q, want %q", got, want)
	}
}

// __system__ carries cross-cutting platform events and keeps the UN-PREFIXED
// legacy topic names (subject-taxonomy.md §6).
func TestFor_SystemTenantIsUnprefixed(t *testing.T) {
	got, err := topic.For(env("platform.mode.changed", "__system__", fact), "__system__")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if want := "platform.mode"; got != want {
		t.Fatalf("topic = %q, want %q", got, want)
	}
}

// A snapshot needs COMPACTION while its sibling FACTs need time-retention, so it
// goes to a separate compacted topic (subject-taxonomy.md §5).
func TestFor_SnapshotGoesToCompactedSibling(t *testing.T) {
	got, err := topic.For(env("risk.position.changed", "acme", snapshot), "acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if want := "acme.risk.position.snapshot"; got != want {
		t.Fatalf("topic = %q, want %q", got, want)
	}
}

// FAIL CLOSED. Each of these must be an error — an error NACKs, and a NACK keeps
// the event safe in NATS. A guess would misfile it forever.
func TestFor_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		e      *envelopepb.Envelope
		tenant string
		want   string // substring the error must name
	}{
		{"cross-tenant leak", env("order.order.submitted", "evil", fact), "acme", "tenant"},
		{"empty tenant on envelope", env("order.order.submitted", "", fact), "acme", "tenant"},
		{"two segments", env("order.submitted", "acme", fact), "acme", "event_type"},
		{"four segments", env("a.b.c.d", "acme", fact), "acme", "event_type"},
		{"empty event_type", env("", "acme", fact), "acme", "event_type"},
		{"reserved replay prefix", env("replay.run1.order", "acme", fact), "acme", "reserved"},
		{"reserved dlq prefix", env("dlq.order.submitted", "acme", fact), "acme", "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := topic.For(tc.e, tc.tenant)
			if err == nil {
				t.Fatalf("expected an error, got topic %q — this would misfile the event", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/topic/ -v`
Expected: FAIL — package `topic` does not exist ("no Go files" / cannot find package).

- [ ] **Step 3: Implement the mapping**

Create `kanz/services/archiver/internal/topic/topic.go`:

```go
// Package topic maps an envelope onto the Kafka topic that archives it.
//
// The name is derived from the envelope's EVENT_TYPE, not from the NATS subject
// it arrived on: event_type carries the full three-segment logical name
// ({domain}.{entity}.{event_type}) and the bus client already enforces that the
// envelope's `domain` equals its first segment. The subject is a transport
// detail; the logical name is the contract (kanz-schemas/docs/subject-taxonomy.md).
//
// Every failure here is a REFUSAL, never a guess. The caller turns an error into
// a NACK, which leaves the event safe in NATS until someone fixes the cause. A
// fallback topic would misfile the event permanently — and cross-filing one
// tenant's events into another's topic (or into the shared __system__ topic)
// breaches the Kafka PREFIXED-ACL isolation boundary (MT-01c).
package topic

import (
	"fmt"
	"strings"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// SystemTenant carries cross-cutting platform events and pre-tenancy events. Its
// topics keep the UN-PREFIXED legacy names (subject-taxonomy.md §6).
const SystemTenant = "__system__"

// reserved leading segments that are not part of the {domain}.{entity} space.
var reserved = map[string]bool{"replay": true, "dlq": true}

// For returns the Kafka topic that archives env, for an archiver configured to
// serve the given tenant.
//
//	order.order.submitted   tenant acme       -> acme.order.order
//	platform.mode.changed   tenant __system__ -> platform.mode
//	risk.position.changed   STATE_SNAPSHOT    -> acme.risk.position.snapshot
func For(env *envelopepb.Envelope, tenant string) (string, error) {
	if tenant == "" {
		return "", fmt.Errorf("archiver tenant is empty: refusing to route %q", env.GetEventType())
	}
	if got := env.GetTenantId(); got != tenant {
		return "", fmt.Errorf("cross-tenant event: envelope tenant_id %q on an archiver serving tenant %q (event %q)",
			got, tenant, env.GetEventId())
	}

	parts := strings.Split(env.GetEventType(), ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("malformed event_type %q: want exactly three segments {domain}.{entity}.{event_type}",
			env.GetEventType())
	}
	if reserved[parts[0]] {
		return "", fmt.Errorf("reserved leading segment %q in event_type %q: replay/dlq are not archivable",
			parts[0], env.GetEventType())
	}

	name := parts[0] + "." + parts[1]
	if env.GetEventClass() == envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT {
		// A snapshot is STATE: it needs compaction, while its sibling FACTs need
		// time-retention. One topic cannot be both.
		name += ".snapshot"
	}
	if tenant == SystemTenant {
		return name, nil
	}
	return tenant + "." + name, nil
}
```

- [ ] **Step 4: Run the tests — verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/topic/ -v`
Expected: PASS — all subtests green.

- [ ] **Step 5: Commit**

```bash
git add kanz/services/archiver/internal/topic/
git commit -m "feat(archiver): envelope -> Kafka topic mapping, fail-closed"
```

---

## Task 2: The topic topology + the cross-language arch test

The archiver is useless without topics to produce into (Kafka auto-create is disabled). This task rebuilds the topology from ground truth and locks it there.

**Files:**
- Create: `kanz/test/arch/kafka_topology_test.go`
- Modify: `kanz/infra/kafka/topics-job.yaml` (the heredoc topic table, currently lines ~38–51)

**Interfaces:**
- Consumes: `moduleRoot(t)`, `declaredSubjects(t, root)` — existing unexported helpers in package `arch` (`test/arch/risk_boundary_test.go`, `test/arch/subject_topology_test.go`). Same package, so call them directly.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the failing test**

Create `kanz/test/arch/kafka_topology_test.go`:

```go
package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY SUBJECT THE CODE PUBLISHES MUST HAVE A KAFKA TOPIC TO LAND IN.
//
// The exact analogue of TestEverySubjectIsCarriedByAStream, for the OTHER half of
// the backbone — and the reason that half rotted undetected. Kafka is the durable
// log of record; auto-create is DISABLED, so a subject with no provisioned topic
// is not a soft failure, it is an archiver that fails closed on that event forever.
//
// When this was first written, only 4 of the 16 {domain}.{entity} names the code
// actually publishes on had a topic. Nine PROVISIONED topics had no publisher at
// all: they were written from the taxonomy doc's EXAMPLES rather than from the code.
//
// CROSS-LANGUAGE ON PURPOSE. kanz-py publishes inference.prediction.scored and
// data.feature.drift_detected. A guard that walked only the Go AST would pronounce
// the topology safe while being structurally blind to every subject the Python side
// publishes — which is the identical failure mode AI-M1 found, where Go required
// tenant_id on the live path and the Python producer had no tenant concept at all,
// so nothing kanz-py published had ever been consumable by any Go service.
//
// The reverse direction is deliberately NOT asserted: a provisioned topic with no
// publisher is dead weight, not a fault, and failing on one would block
// provisioning a topic ahead of the code that fills it.
func TestEverySubjectHasAKafkaTopic(t *testing.T) {
	root := moduleRoot(t) // .../kanz
	repo := filepath.Dir(root)

	topics := provisionedTopics(t, filepath.Join(root, "infra", "kafka", "topics-job.yaml"))
	if len(topics) == 0 {
		t.Fatal("no topics found in topics-job.yaml — has the table format changed?")
	}

	subjects := declaredSubjects(t, root)                              // Go
	subjects = append(subjects, pythonSubjects(t, filepath.Join(repo, "kanz-py"))...) // Python
	if len(subjects) == 0 {
		t.Fatal("no subjects found — this test would pass vacuously")
	}

	missing := map[string]bool{}
	for _, s := range subjects {
		parts := strings.Split(s, ".")
		if len(parts) != 3 {
			continue // not a {domain}.{entity}.{event_type} logical name
		}
		if parts[0] == "replay" || parts[0] == "dlq" {
			continue // reserved, never archived
		}
		name := parts[0] + "." + parts[1]
		if !topics[name] {
			missing[name] = true
		}
	}
	if len(missing) > 0 {
		var out []string
		for m := range missing {
			out = append(out, m)
		}
		sort.Strings(out)
		t.Fatalf("these {domain}.{entity} names are published by the code but have NO Kafka topic "+
			"in infra/kafka/topics-job.yaml:\n\n  %s\n\nKafka auto-create is disabled, so the archiver "+
			"fails closed on every one of these events. Provision them.", strings.Join(out, "\n  "))
	}
}

// provisionedTopics reads the topic names from the heredoc table in topics-job.yaml.
// Table rows are: `name  partitions  cleanup  retention_ms  dlq`.
var topicRow = regexp.MustCompile(`(?m)^\s{4}([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)\s+\d+\s+(?:delete|compact)\s`)

func provisionedTopics(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read topics-job.yaml: %v", err)
	}
	out := map[string]bool{}
	for _, m := range topicRow.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	return out
}

// pythonSubjects scans kanz-py for three-segment subject literals. The Python side
// has no AST walker and does not need one for this: subjects are written as plain
// string literals (kanz_inference/publish.py, governance/drift_trigger.py).
var pySubject = regexp.MustCompile(`"([a-z][a-z0-9]*(?:\.[a-z0-9_]+){2})"`)

func pythonSubjects(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "__pycache__", ".venv", "tests":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".py") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range pySubject.FindAllStringSubmatch(string(b), -1) {
			if strings.Contains(m[1], ".v1.") {
				continue // proto type ref, not a subject
			}
			out = append(out, m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk kanz-py: %v", err)
	}
	return out
}
```

Note: `filepath.WalkDir` takes an `fs.DirEntry`; `os.DirEntry` is an alias for it, so the signature above compiles. If the linter objects, import `io/fs` and use `fs.DirEntry`.

- [ ] **Step 2: Run the test — verify it fails, and READ what it names**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestEverySubjectHasAKafkaTopic -v`
Expected: FAIL, listing the 12 unprovisioned names:
`accounting.balance`, `compliance.breach`, `compliance.mandate`, `market.book`, `market.crypto`, `order.order`, `platform.authz`, `platform.compliance`, `platform.mode`, `risk.position`, `settlement.instruction`, `strategy.signal`.

This failure IS the bug. Do not skip reading it.

- [ ] **Step 3: Rebuild the topic table from ground truth**

In `kanz/infra/kafka/topics-job.yaml`, replace the entire heredoc table (the block between `<<'EOF'` and `EOF`) with:

```
    # name                     partitions  cleanup  retention_ms  dlq
    #
    # GROUND TRUTH, NOT THE TAXONOMY DOC'S EXAMPLES. This table used to name
    # execution.order, market.equity, market.option, data.market_stream,
    # observability.model, platform.model and platform.config — NOT ONE of which is
    # published by any service. Meanwhile the entire money path (orders, fills,
    # accounting, settlement, mandates, positions) had no topic at all. Auto-create
    # is disabled, so those events had nowhere to land. TestEverySubjectHasAKafkaTopic
    # now fails the build if this table and the code ever drift apart again.
    #
    # STATE, NOT EVENTS -> compact, keyed by partition_key. A mandate that ages out
    # of the log is a control that comes back DISARMED after a restart (EXEC-M13);
    # a position that ages out is a book that rebuilds wrong. Their NATS streams
    # already know this (--max-msgs-per-subject=1, no max-age); Kafka must not be
    # the place the lesson is forgotten.
    compliance.mandate         3   compact  -1          no
    risk.position              6   compact  -1          no

    # MONEY AND OBLIGATIONS -> 30d + a DLQ. 30 days is what the ARCHIVER buys;
    # permanence is the lakehouse's job (DATA-M2), not a retention setting here.
    order.order                6   delete   2592000000  yes
    strategy.signal            6   delete   2592000000  yes
    accounting.balance         6   delete   2592000000  yes
    settlement.instruction     3   delete   2592000000  yes
    compliance.breach          3   delete   2592000000  yes
    risk.portfolio             6   delete   2592000000  yes

    # PLATFORM LIFECYCLE -> 30d + a DLQ. Authz/mode/compliance-config changes are
    # the audit trail of who was allowed to do what, and when.
    platform.authz             3   delete   2592000000  yes
    platform.compliance        3   delete   2592000000  yes
    platform.mode              3   delete   2592000000  yes

    # INFERENCE / DATA QUALITY. inference.prediction and data.feature are published
    # by kanz-py, not Go — see the arch test's cross-language note.
    inference.feature          6   delete   604800000   yes
    inference.prediction       6   delete   2592000000  yes
    data.feature               3   delete   2592000000  yes

    # MARKET. Provisioned but NOT ARCHIVED by DATA-M1 (see the spec's scope): ticks
    # are the highest-volume streams by far and are re-fetchable from the venue,
    # unlike a fill. Archiving them is its own capacity and cost decision.
    market.book                12  delete   604800000   yes
    market.crypto              12  delete   604800000   yes
```

- [ ] **Step 4: Run the test — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestEverySubjectHasAKafkaTopic -v`
Expected: PASS.

- [ ] **Step 5: Verify the guard actually bites (mutation check)**

Temporarily delete the `order.order` row from the table and re-run:

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestEverySubjectHasAKafkaTopic`
Expected: **FAIL**, naming `order.order`.

Then restore the row and re-run — expect PASS. A guard that cannot fail is decoration; you must see it fail once.

- [ ] **Step 6: Run the whole arch package (nothing else regressed)**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/`
Expected: PASS (broker-gated tests skip without a live broker).

- [ ] **Step 7: Commit**

```bash
git add kanz/test/arch/kafka_topology_test.go kanz/infra/kafka/topics-job.yaml
git commit -m "feat(infra,arch): rebuild the Kafka topic topology from ground truth; guard it cross-language"
```

---

## Task 3: The archiver fold

**Files:**
- Create: `kanz/services/archiver/internal/archive/archiver.go`
- Test: `kanz/services/archiver/internal/archive/archiver_test.go`

**Interfaces:**
- Consumes: `topic.For(env, tenant) (string, error)` from Task 1.
- Produces:
  - `archive.Publisher` interface — `Publish(ctx, bus.Message) error` (satisfied by `*bus.KafkaClient`).
  - `archive.Subscriber` interface — `Subscribe(ctx, subject, group string, h bus.Handler) error` (satisfied by `*bus.NATSClient`).
  - `archive.New(cfg archive.Config) *archive.Archiver`
  - `archive.Config{Tenant string; Group string; Subjects []string; Kafka Publisher; NATS Subscriber; Logger *slog.Logger}` — **no `Metrics` field yet**; Task 6 adds `Metrics *Metrics`. Do not add it here, and do not reference `bus.BusMetrics`.
  - `(*Archiver).Run(ctx) error` — subscribes every subject; blocks until ctx is done.
  - `(*Archiver).Handle(ctx, msg bus.Message) error` — exported so tests drive one message without a broker.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/archiver/internal/archive/archiver_test.go`:

```go
package archive_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/archiver/internal/archive"
)

type fakeKafka struct {
	got  []bus.Message
	fail error
}

func (f *fakeKafka) Publish(_ context.Context, m bus.Message) error {
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, m)
	return nil
}

func body(t *testing.T, e *envelopepb.Envelope) []byte {
	t.Helper()
	b, err := proto.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newArchiver(k archive.Publisher) *archive.Archiver {
	return archive.New(archive.Config{
		Tenant: "acme",
		Group:  "archiver",
		Kafka:  k,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestHandle_ProducesVerbatimToTheMappedTopic(t *testing.T) {
	e := &envelopepb.Envelope{
		EventType:    "order.order.submitted",
		TenantId:     "acme",
		EventId:      "evt-1",
		PartitionKey: "portfolio-7",
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	raw := body(t, e)
	k := &fakeKafka{}

	if err := newArchiver(k).Handle(context.Background(), bus.Message{Subject: "order.order.submitted", Body: raw}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(k.got) != 1 {
		t.Fatalf("published %d messages, want 1", len(k.got))
	}
	m := k.got[0]
	if m.Subject != "acme.order.order" {
		t.Errorf("topic = %q, want %q", m.Subject, "acme.order.order")
	}
	// The KEY is what preserves per-entity order across partitions.
	if string(m.Key) != "portfolio-7" {
		t.Errorf("key = %q, want %q", m.Key, "portfolio-7")
	}
	// VERBATIM: the archiver is a transport, not a producer.
	if string(m.Body) != string(raw) {
		t.Error("body was rewritten — the archiver must never re-stamp the log of record")
	}
	if m.Headers["Kanz-Event-Id"] != "evt-1" {
		t.Errorf("Kanz-Event-Id = %q, want evt-1 (the downstream dedup key)", m.Headers["Kanz-Event-Id"])
	}
}

// THE CENTRAL GUARANTEE. A failed produce must NACK, so NATS redelivers. Acking a
// message we failed to archive is silent, permanent data loss — the exact bug this
// service exists to end.
func TestHandle_KafkaFailureNacks(t *testing.T) {
	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "acme", EventId: "evt-1"}
	k := &fakeKafka{fail: errors.New("kafka down")}

	err := newArchiver(k).Handle(context.Background(), bus.Message{Body: body(t, e)})
	if err == nil {
		t.Fatal("Handle returned nil on a failed produce — NATS would ACK and the event would be LOST FOREVER")
	}
}

// An unmappable event must NACK too, never fall back to a default topic.
func TestHandle_UnmappableNacksAndPublishesNothing(t *testing.T) {
	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "someone-else", EventId: "evt-1"}
	k := &fakeKafka{}

	if err := newArchiver(k).Handle(context.Background(), bus.Message{Body: body(t, e)}); err == nil {
		t.Fatal("cross-tenant event was accepted — expected a refusal")
	}
	if len(k.got) != 0 {
		t.Fatalf("published %d messages for an unmappable event, want 0", len(k.got))
	}
}

func TestHandle_UndecodableBodyNacks(t *testing.T) {
	k := &fakeKafka{}
	if err := newArchiver(k).Handle(context.Background(), bus.Message{Body: []byte("not a protobuf envelope")}); err == nil {
		t.Fatal("garbage body was accepted — expected a refusal")
	}
}
```

- [ ] **Step 2: Run the test — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/archive/ -v`
Expected: FAIL — package `archive` does not exist.

- [ ] **Step 3: Implement the archiver**

Create `kanz/services/archiver/internal/archive/archiver.go`:

```go
// Package archive is the NATS→Kafka archiver (DATA-M1): the producer that makes
// Kafka the durable log of record it was always documented to be.
//
// Before this existed, bus.DialKafka had exactly ONE non-test caller in the
// repository — lake-sink, and it SUBSCRIBES. Nothing had ever written an event to
// Kafka. The EXECUTION stream has a 24h max-age, so the platform's history —
// realized P&L since inception, the audit trail, every fill older than yesterday —
// was retained nowhere at all.
//
// Two invariants hold this together, and both are about the same thing:
//
//   - ACK ONLY AFTER KAFKA ACKNOWLEDGES. Every failure returns an error, which
//     NACKs, which redelivers. Acking an event we failed to archive is silent,
//     permanent loss. That makes delivery AT-LEAST-ONCE: a redelivery after a
//     successful produce whose ack was lost writes a duplicate, and event_id (on
//     the Kanz-Event-Id header) is the dedup key downstream. Duplicates are
//     recoverable; a hole is not.
//
//   - THE ENVELOPE IS ARCHIVED VERBATIM. The archiver never re-stamps, re-validates
//     or re-serializes. It is a transport, not a producer — a component that
//     rewrites the log of record on its way into the log of record cannot be trusted
//     as a record of what happened. It unframes ONLY to read routing fields.
//
// SINGLE WRITER, DELIBERATELY. One replica per stream (Recreate, not RollingUpdate).
// Two producers can invert per-key order in Kafka, and a reordered log rebuilds a
// DIFFERENT book downstream — a subtler failure than losing it. A gap here is not
// loss the way it was for webhook-ingest (EXEC-M22): NATS retains 24h (168h on the
// money streams) and a restarted pod catches up. The real failure mode is FALLING
// BEHIND, which is why lag is a first-class metric.
package archive

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/archiver/internal/topic"
)

// HeaderEventID carries the envelope's event_id so a downstream consumer can dedup
// an at-least-once redelivery WITHOUT parsing the payload.
const HeaderEventID = "Kanz-Event-Id"

// Publisher is the Kafka side. Satisfied by *bus.KafkaClient.
type Publisher interface {
	Publish(ctx context.Context, msg bus.Message) error
}

// Subscriber is the NATS side. Satisfied by *bus.NATSClient. A handler returning a
// non-nil error NACKs the message.
type Subscriber interface {
	Subscribe(ctx context.Context, subject, group string, h bus.Handler) error
}

// Config configures an Archiver.
type Config struct {
	// Tenant is the tenant this archiver serves (ARCHIVER_TENANT). An envelope
	// carrying any other tenant_id is refused, not misfiled.
	Tenant string
	// Group is the durable consumer name.
	Group string
	// Subjects are the NATS subject patterns to archive (one durable per subject).
	Subjects []string
	Kafka    Publisher
	NATS     Subscriber
	Logger   *slog.Logger
	// Metrics is added in Task 6. Leave it out for now.
}

// Archiver drains NATS subjects into Kafka topics.
type Archiver struct {
	cfg Config
}

// New builds an Archiver.
func New(cfg Config) *Archiver { return &Archiver{cfg: cfg} }

// Run subscribes every configured subject and blocks until ctx is done.
func (a *Archiver) Run(ctx context.Context) error {
	for _, subject := range a.cfg.Subjects {
		if err := a.cfg.NATS.Subscribe(ctx, subject, a.cfg.Group, a.Handle); err != nil {
			return fmt.Errorf("archiver: subscribe %q: %w", subject, err)
		}
		a.cfg.Logger.Info("archiving", "subject", subject, "group", a.cfg.Group, "tenant", a.cfg.Tenant)
	}
	<-ctx.Done()
	return nil
}

// Handle archives one message. A non-nil return NACKs it — which is the point: the
// event stays in NATS until it is safely in Kafka.
func (a *Archiver) Handle(ctx context.Context, msg bus.Message) error {
	env, _, err := bus.Unframe(msg.Body)
	if err != nil {
		// Not decodable ⇒ we cannot even name it. NACK; do not drop.
		a.cfg.Logger.Error("archiver: undecodable envelope", "subject", msg.Subject, "err", err)
		return fmt.Errorf("archiver: unframe: %w", err)
	}

	name, err := topic.For(env, a.cfg.Tenant)
	if err != nil {
		a.cfg.Logger.Error("archiver: refusing to route event",
			"subject", msg.Subject, "event_id", env.GetEventId(), "event_type", env.GetEventType(), "err", err)
		return fmt.Errorf("archiver: topic: %w", err)
	}

	// Body VERBATIM. Key = partition_key, so events sharing a key land on one
	// partition, in order.
	out := bus.Message{
		Subject: name,
		Key:     []byte(env.GetPartitionKey()),
		Body:    msg.Body,
		Headers: map[string]string{HeaderEventID: env.GetEventId()},
	}
	if err := a.cfg.Kafka.Publish(ctx, out); err != nil {
		// NACK. Never ack an event we failed to archive.
		a.cfg.Logger.Error("archiver: kafka produce failed — NACKing",
			"topic", name, "event_id", env.GetEventId(), "err", err)
		return fmt.Errorf("archiver: publish to %q: %w", name, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests — verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/archive/ -v`
Expected: PASS — all four tests green.

- [ ] **Step 5: Verify the central guarantee actually bites (mutation check)**

Temporarily change `Handle` so the Kafka error is swallowed (`_ = a.cfg.Kafka.Publish(ctx, out); return nil`).

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/archive/ -run TestHandle_KafkaFailureNacks`
Expected: **FAIL** — "Handle returned nil on a failed produce". Then revert the mutation and confirm PASS.

- [ ] **Step 6: Commit**

```bash
git add kanz/services/archiver/internal/archive/
git commit -m "feat(archiver): the fold — map, produce verbatim, ack only after Kafka acknowledges"
```

---

## Task 4: Config + composition root + image

**Files:**
- Create: `kanz/services/archiver/internal/config/config.go`
- Create: `kanz/services/archiver/internal/config/config_test.go`
- Create: `kanz/services/archiver/cmd/archiver/main.go`
- Create: `kanz/services/archiver/Dockerfile`

**Interfaces:**
- Consumes: `archive.New`, `archive.Config`, `(*Archiver).Run` (Task 3).
- Produces: `config.Load() (config.Config, error)`; `config.Config{Listen string; LogLevel slog.Level; Tenant string; NATSURL string; Brokers []string; Subjects []string; Group string; Source string; OTLPEndpoint string}`; `config.DefaultSubjects` (the in-scope subject list).

- [ ] **Step 1: Write the failing test**

Create `kanz/services/archiver/internal/config/config_test.go`:

```go
package config_test

import (
	"testing"

	"github.com/kanz-eng/kanz/services/archiver/internal/config"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func valid() map[string]string {
	return map[string]string{
		"ARCHIVER_TENANT":        "acme",
		"ARCHIVER_NATS_URL":      "nats://localhost:4222",
		"ARCHIVER_KAFKA_BROKERS": "localhost:9092",
	}
}

func TestLoad_Valid(t *testing.T) {
	setEnv(t, valid())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tenant != "acme" {
		t.Errorf("Tenant = %q, want acme", cfg.Tenant)
	}
	if len(cfg.Brokers) != 1 || cfg.Brokers[0] != "localhost:9092" {
		t.Errorf("Brokers = %v", cfg.Brokers)
	}
	// The in-scope streams default in — market and observability stay OUT.
	if len(cfg.Subjects) == 0 {
		t.Fatal("Subjects is empty — the archiver would archive nothing")
	}
	for _, s := range cfg.Subjects {
		if s == "market.>" || s == "observability.>" {
			t.Errorf("subject %q is out of scope for DATA-M1", s)
		}
	}
}

// FAIL CLOSED AT STARTUP. An archiver with no tenant cannot route; an archiver with
// no Kafka has nowhere to archive TO. Either one, running, looks healthy and
// silently retains nothing — so it must not start.
func TestLoad_FailsClosed(t *testing.T) {
	for _, missing := range []string{"ARCHIVER_TENANT", "ARCHIVER_NATS_URL", "ARCHIVER_KAFKA_BROKERS"} {
		t.Run("missing "+missing, func(t *testing.T) {
			env := valid()
			delete(env, missing)
			// t.Setenv on the others; the missing one is simply never set.
			setEnv(t, env)
			t.Setenv(missing, "")

			if _, err := config.Load(); err == nil {
				t.Fatalf("Load succeeded without %s — a silently non-archiving archiver reports healthy", missing)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/config/ -v`
Expected: FAIL — package `config` does not exist.

- [ ] **Step 3: Implement the config**

Create `kanz/services/archiver/internal/config/config.go`:

```go
// Package config is the archiver's runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d). Mirrors
// the lake-sink / risk-engine config shape.
package config

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

// DefaultSubjects is what DATA-M1 archives: every stream carrying history that
// cannot be reconstructed from anywhere else.
//
// market.> is OUT on purpose — ticks are the highest-volume streams by far and are
// re-fetchable from the venue, unlike a fill. observability.> is OUT because it is
// a 1h ephemeral stream and nothing declares a subject on it.
var DefaultSubjects = []string{
	"order.>",
	"strategy.>",
	"execution.>",
	"accounting.>",
	"settlement.>",
	"compliance.breach.>",
	"compliance.mandate.>",
	"risk.portfolio.>",
	"risk.exposure.>",
	"risk.signal.>",
	"risk.command.>",
	"risk.position.>",
	"inference.>",
	"platform.>",
	"data.>",
}

// Config is the archiver's runtime configuration.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// Tenant is the tenant this archiver serves. REQUIRED: NATS isolation is by
	// account, and the Kafka topic is tenant-prefixed. An archiver that does not
	// know its tenant cannot route a single event.
	Tenant string
	// NATSURL is the live spine it drains. REQUIRED.
	NATSURL string
	// Brokers is the Kafka cluster it archives INTO. REQUIRED: an archiver with no
	// Kafka reports healthy and retains nothing.
	Brokers []string
	// Subjects are the NATS subject patterns to archive.
	Subjects []string
	// Group is the durable consumer name.
	Group string
	// Source is the service identity stamped on telemetry.
	Source string
	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string
}

// Load reads the environment and REFUSES to return a config that cannot archive.
func Load() (Config, error) {
	cfg := Config{
		Listen:       envOr("ARCHIVER_LISTEN", ":8086"),
		LogLevel:     parseLevel(envOr("ARCHIVER_LOG_LEVEL", "info")),
		Tenant:       os.Getenv("ARCHIVER_TENANT"),
		NATSURL:      os.Getenv("ARCHIVER_NATS_URL"),
		Brokers:      splitList(os.Getenv("ARCHIVER_KAFKA_BROKERS")),
		Subjects:     splitList(os.Getenv("ARCHIVER_SUBJECTS")),
		Group:        envOr("ARCHIVER_CONSUMER_GROUP", "archiver"),
		Source:       envOr("ARCHIVER_SOURCE", "archiver"),
		OTLPEndpoint: os.Getenv("ARCHIVER_OTLP_ENDPOINT"),
	}
	if len(cfg.Subjects) == 0 {
		cfg.Subjects = DefaultSubjects
	}
	if cfg.Tenant == "" {
		return Config{}, errors.New("ARCHIVER_TENANT is required: an archiver that does not know its tenant cannot route an event")
	}
	if cfg.NATSURL == "" {
		return Config{}, errors.New("ARCHIVER_NATS_URL is required")
	}
	if len(cfg.Brokers) == 0 {
		return Config{}, errors.New("ARCHIVER_KAFKA_BROKERS is required: an archiver with no Kafka reports healthy and retains nothing")
	}
	return cfg, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
```

- [ ] **Step 4: Run the tests — verify they pass**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Write the composition root**

Create `kanz/services/archiver/cmd/archiver/main.go`. Follow `services/lake-sink/cmd/lake-sink/main.go` exactly for the observability/HTTP/shutdown scaffolding; the archiver-specific wiring is:

```go
// archiver binary entrypoint (DATA-M1). Drains the NATS live spine (EVT-08) into
// the Kafka durable log of record (EVT-09) — the producer that, until this shipped,
// did not exist, leaving the log of record empty and every event older than the
// stream's max-age gone forever.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/pkg/server"
	"github.com/kanz-eng/kanz/services/archiver/internal/archive"
	"github.com/kanz-eng/kanz/services/archiver/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2) // fail closed: a non-archiving archiver reports healthy
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    cfg.Source,
		ServiceVersion: version(),
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRatio:    1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		os.Exit(2)
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	kafka, err := bus.DialKafka(bus.KafkaConfig{Brokers: cfg.Brokers, ClientID: cfg.Source})
	if err != nil {
		logger.Error("kafka dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = kafka.Close() }()

	nc, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		logger.Error("nats dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = nc.Close() }()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("archiver listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	a := archive.New(archive.Config{
		Tenant:   cfg.Tenant,
		Group:    cfg.Group,
		Subjects: cfg.Subjects,
		Kafka:    kafka,
		NATS:     nc,
		Logger:   logger,
	})
	readiness.Set(true)

	if err := a.Run(ctx); err != nil {
		logger.Error("archiver stopped", "err", err)
		os.Exit(1)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

// version is the service version stamped on telemetry.
func version() string { return "0.1.0" }
```

**Check `pkg/server`'s readiness API before writing this** (`server.Readiness` / `readiness.Set`) — copy whatever `lake-sink` does verbatim rather than guessing; if its API differs, follow lake-sink.

- [ ] **Step 6: Create the Dockerfile**

Create `kanz/services/archiver/Dockerfile` by copying the pattern from an existing Go service Dockerfile (e.g. `kanz/services/lake-sink/Dockerfile` if present, otherwise `kanz/services/oms/Dockerfile`), changing only the binary path to `./services/archiver/cmd/archiver`. Distroless base, as every other service uses.

- [ ] **Step 7: Build and vet**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./services/archiver/...`
Expected: clean, no output.

- [ ] **Step 8: Commit**

```bash
git add kanz/services/archiver/
git commit -m "feat(archiver): config, composition root and image — fail closed without a tenant or Kafka"
```

---

## Task 5: The payoff — a real NATS and a real Kafka

The tests that prove the thing actually works. Everything before this was a fake.

**Files:**
- Create: `kanz/services/archiver/internal/archive/archiver_integration_test.go`

**Interfaces:**
- Consumes: `archive.New/Config/Run/Handle`, `bus.DialNATS`, `bus.DialKafka`.
- Produces: nothing.

**Conventions to match (read these first):** `kanz/pkg/bus/kafka_integration_test.go` (skips on empty `TEST_KAFKA_BROKERS`; creates its topic out-of-band via `kafka.Dial` + `CreateTopics` because auto-create is disabled) and `kanz/pkg/bus/nats_integration_test.go` (skips on empty `TEST_NATS_URL`; creates its stream via `jetstream.New(conn).CreateStream`).

**Precondition — the CI Kafka container MUST run with `KAFKA_AUTO_CREATE_TOPICS_ENABLE=false`** (Task 7). Production has auto-create disabled; a test broker that silently conjures a missing topic cannot reproduce the fail-closed behaviour, and Test 4 would pass for the wrong reason.

**A note on the subject used:** these tests publish on a synthetic domain (`archtest{suffix}.order.submitted`) rather than a real one like `order.order.submitted`. That is deliberate: a JetStream subject may belong to exactly ONE stream, so creating a test stream on `order.>` would collide with the real `EXECUTION` stream on any bootstrapped broker and make the suite order-dependent. The mapping of the *real* subject names is exhaustively covered by the Task 1 unit tests; what these tests exercise is the machinery.

- [ ] **Step 1: Write the integration tests**

Create `kanz/services/archiver/internal/archive/archiver_integration_test.go`:

```go
package archive_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/segmentio/kafka-go"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/archiver/internal/archive"
)

const testTenant = "acme"

// harness is one isolated NATS stream + Kafka topic pair for a single test.
type harness struct {
	brokers  []string
	natsURL  string
	domain   string // synthetic, e.g. "archtest1730000000"
	subject  string // "<domain>.order.submitted"
	topic    string // "acme.<domain>.order"
	nats     *bus.NATSClient
	kafka    *bus.KafkaClient
	group    string
}

func setup(t *testing.T, partitions int) *harness {
	t.Helper()
	rawBrokers := os.Getenv("TEST_KAFKA_BROKERS")
	natsURL := os.Getenv("TEST_NATS_URL")
	if rawBrokers == "" || natsURL == "" {
		t.Skip("TEST_KAFKA_BROKERS / TEST_NATS_URL not set")
	}
	brokers := strings.Split(rawBrokers, ",")

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	h := &harness{
		brokers: brokers,
		natsURL: natsURL,
		domain:  "archtest" + suffix,
		group:   "archiver-" + suffix,
	}
	h.subject = h.domain + ".order.submitted"
	h.topic = testTenant + "." + h.domain + ".order"

	ctx := context.Background()

	// --- NATS stream (the live spine under test) ---
	conn, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "ARCHTEST" + suffix,
		Subjects:  []string{h.domain + ".>"},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		conn.Close()
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() {
		_ = js.DeleteStream(context.Background(), "ARCHTEST"+suffix)
		conn.Close()
	})

	// --- Kafka topic. Created out-of-band: auto-create is DISABLED, in production
	// and in CI. Several partitions, or the ordering assertion is vacuous.
	kconn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("kafka dial: %v", err)
	}
	if err := kconn.CreateTopics(kafka.TopicConfig{
		Topic:             h.topic,
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	}); err != nil {
		kconn.Close()
		t.Fatalf("create topic: %v", err)
	}
	kconn.Close()
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.DeleteTopics(h.topic)
	})

	h.nats, err = bus.DialNATS(ctx, bus.NATSConfig{URL: natsURL, Name: "archiver-test"})
	if err != nil {
		t.Fatalf("DialNATS: %v", err)
	}
	t.Cleanup(func() { _ = h.nats.Close() })

	h.kafka, err = bus.DialKafka(bus.KafkaConfig{Brokers: brokers, ClientID: "archiver-test"})
	if err != nil {
		t.Fatalf("DialKafka: %v", err)
	}
	t.Cleanup(func() { _ = h.kafka.Close() })

	return h
}

// publish puts one envelope on the NATS spine.
//
// THE BODY IS AN EventFrame, NOT A BARE Envelope. bus.Unframe unmarshals into
// envelopepb.EventFrame{Envelope, Payload}, and this is a trap: EventFrame's field 1
// and Envelope's field 1 are BOTH wire-type 2, so a bare marshaled Envelope does not
// fail to decode — it silently yields a frame holding a garbage envelope. Marshal the
// frame, exactly as bus.Producer does on the real publish path.
func (h *harness) publish(t *testing.T, eventID, partitionKey string) {
	t.Helper()
	e := &envelopepb.Envelope{
		EventType:    h.subject,
		TenantId:     testTenant,
		EventId:      eventID,
		PartitionKey: partitionKey,
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	body, err := proto.Marshal(&envelopepb.EventFrame{Envelope: e})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := h.nats.Publish(context.Background(), bus.Message{Subject: h.subject, Body: body}); err != nil {
		t.Fatalf("publish %s: %v", eventID, err)
	}
}

// drain reads up to want messages off the Kafka topic, returning the event_ids per
// partition key, in arrival order.
func (h *harness) drain(t *testing.T, want int, timeout time.Duration) map[string][]string {
	t.Helper()
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: h.brokers,
		Topic:   h.topic,
		GroupID: "drain-" + h.group,
	})
	defer r.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out := map[string][]string{}
	for n := 0; n < want; n++ {
		m, err := r.ReadMessage(ctx)
		if err != nil {
			break // timeout: caller asserts on what did (or did not) arrive
		}
		// The archived body is the EventFrame, byte-for-byte as it rode the spine.
		var frame envelopepb.EventFrame
		if err := proto.Unmarshal(m.Value, &frame); err != nil {
			t.Fatalf("the archived body is not a valid EventFrame — it was rewritten in flight: %v", err)
		}
		if frame.GetEnvelope() == nil {
			t.Fatal("the archived frame has no envelope — the body was rewritten in flight")
		}
		key := string(m.Key)
		out[key] = append(out[key], frame.GetEnvelope().GetEventId())
	}
	return out
}

func (h *harness) archiver(k archive.Publisher) *archive.Archiver {
	return archive.New(archive.Config{
		Tenant:   testTenant,
		Group:    h.group,
		Subjects: []string{h.domain + ".>"},
		Kafka:    k,
		NATS:     h.nats,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// run starts the archiver and stops it after d.
func (h *harness) run(t *testing.T, a *archive.Archiver, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
}

// deadKafka fails every produce — a Kafka outage, without stopping the container.
type deadKafka struct{ calls int }

func (d *deadKafka) Publish(context.Context, bus.Message) error {
	d.calls++
	return errors.New("kafka is down")
}

// ---------------------------------------------------------------------------
// 1. THE PAYOFF TEST. Before this service existed, NOTHING had ever written an
// event to Kafka. This is the proof that it does — and that per-key order survives
// partitioning, which is what makes the log replayable into the same book.
// ---------------------------------------------------------------------------
func TestArchiver_LandsEveryEventInOrder(t *testing.T) {
	h := setup(t, 4) // several partitions, or "in order" proves nothing

	// Two entities, interleaved. Each key's events must come back in publish order.
	h.publish(t, "a1", "portfolio-A")
	h.publish(t, "b1", "portfolio-B")
	h.publish(t, "a2", "portfolio-A")
	h.publish(t, "b2", "portfolio-B")
	h.publish(t, "a3", "portfolio-A")

	h.run(t, h.archiver(h.kafka), 10*time.Second)

	got := h.drain(t, 5, 15*time.Second)

	wantA := []string{"a1", "a2", "a3"}
	wantB := []string{"b1", "b2"}
	if !equal(got["portfolio-A"], wantA) {
		t.Errorf("portfolio-A = %v, want %v (per-key order was not preserved)", got["portfolio-A"], wantA)
	}
	if !equal(got["portfolio-B"], wantB) {
		t.Errorf("portfolio-B = %v, want %v (per-key order was not preserved)", got["portfolio-B"], wantB)
	}
}

// ---------------------------------------------------------------------------
// 2. KAFKA OUTAGE — NO LOSS, CLEAN CATCH-UP. A gap is deferred work, not loss:
// NATS holds the events while Kafka is down, and the archiver drains them when it
// comes back. This is why replicas:1 + Recreate is SAFE here and was not for
// webhook-ingest.
// ---------------------------------------------------------------------------
func TestArchiver_KafkaOutageLosesNothing(t *testing.T) {
	h := setup(t, 4)

	h.publish(t, "e1", "portfolio-A")
	h.publish(t, "e2", "portfolio-A")
	h.publish(t, "e3", "portfolio-A")

	// --- Kafka is down. Every produce fails, so every message is NACKed. ---
	dead := &deadKafka{}
	h.run(t, h.archiver(dead), 5*time.Second)

	if dead.calls == 0 {
		t.Fatal("the archiver never tried to produce — the test proves nothing")
	}
	if landed := h.drain(t, 3, 2*time.Second); len(landed) != 0 {
		t.Fatalf("events landed in Kafka while it was down: %v", landed)
	}

	// --- Kafka is back. The SAME durable group must redeliver everything. ---
	h.run(t, h.archiver(h.kafka), 10*time.Second)

	got := h.drain(t, 3, 15*time.Second)
	want := []string{"e1", "e2", "e3"}
	if !equal(got["portfolio-A"], want) {
		t.Fatalf("after the outage, Kafka holds %v, want %v — events were LOST across the outage",
			got["portfolio-A"], want)
	}
}

// ---------------------------------------------------------------------------
// 3. THE RED TWIN, KEPT EXECUTABLE. Ack-before-produce is the bug this service
// exists to prevent, and a test that cannot reproduce it is not guarding anything.
// The SAME outage, against a handler that acks whatever it failed to produce,
// LOSES the events permanently — NATS considers them delivered and never sends
// them again.
// ---------------------------------------------------------------------------
func TestArchiver_AckBeforeProduceLosesEvents_RedTwin(t *testing.T) {
	h := setup(t, 4)

	h.publish(t, "e1", "portfolio-A")
	h.publish(t, "e2", "portfolio-A")

	// The broken archiver: produce, ignore the error, ACK anyway.
	dead := &deadKafka{}
	broken := func(ctx context.Context, msg bus.Message) error {
		_ = dead.Publish(ctx, msg) // the error is swallowed — this is the bug
		return nil                 // ACK. The event is now gone from NATS forever.
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := h.nats.Subscribe(ctx, h.domain+".>", h.group, broken); err != nil {
		cancel()
		t.Fatalf("subscribe: %v", err)
	}
	<-ctx.Done()
	cancel()

	// Kafka comes back, and a CORRECT archiver runs on the same durable group.
	h.run(t, h.archiver(h.kafka), 8*time.Second)

	got := h.drain(t, 2, 5*time.Second)
	if len(got["portfolio-A"]) != 0 {
		t.Fatalf("the red twin did NOT lose the events (Kafka holds %v) — either the ack semantics "+
			"changed or this test no longer reproduces the bug it guards against", got["portfolio-A"])
	}
	t.Log("red twin confirmed: ack-before-produce lost every event across the outage")
}

// ---------------------------------------------------------------------------
// 4. FAIL CLOSED ON AN UNPROVISIONED TENANT. Kafka auto-create is disabled, so a
// tenant whose topics were never provisioned has nowhere to land. That must NACK —
// never cross-file into the shared un-prefixed topic, which would breach the Kafka
// PREFIXED-ACL isolation boundary (MT-01c).
// ---------------------------------------------------------------------------
func TestArchiver_UnprovisionedTenantNacksAndDoesNotCrossFile(t *testing.T) {
	h := setup(t, 1)

	// An archiver serving a tenant whose topic ("ghost.<domain>.order") was never created.
	a := archive.New(archive.Config{
		Tenant:   "ghost",
		Group:    h.group,
		Subjects: []string{h.domain + ".>"},
		Kafka:    h.kafka,
		NATS:     h.nats,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	e := &envelopepb.Envelope{
		EventType:    h.subject,
		TenantId:     "ghost",
		EventId:      "g1",
		PartitionKey: "portfolio-A",
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	body, err := proto.Marshal(&envelopepb.EventFrame{Envelope: e}) // frame, not bare envelope
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := a.Handle(context.Background(), bus.Message{Subject: h.subject, Body: body}); err == nil {
		t.Fatal("an event for an unprovisioned tenant was ACCEPTED — Kafka auto-create must be disabled " +
			"on the test broker, or the archiver is not failing closed")
	}

	// And it must not have been cross-filed into the un-prefixed topic.
	if landed := h.drain(t, 1, 2*time.Second); len(landed) != 0 {
		t.Fatalf("the event was cross-filed: %v", landed)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

**Note on `h.nats.Publish`:** confirm `*bus.NATSClient` exposes `Publish(ctx, bus.Message) error` (it satisfies `bus.Publisher`). If the JetStream publish requires the subject to be bound to a stream, the harness already creates that stream — that is why `setup` runs first.

- [ ] **Step 2: Run against real brokers**

Start the brokers (auto-create OFF, matching production):

```bash
docker run -d --name nats-test -p 4222:4222 nats:2 -js
docker run -d --name kafka-test -p 9092:9092 \
  -e KAFKA_AUTO_CREATE_TOPICS_ENABLE=false \
  apache/kafka:3.9.0
```

Run:
```bash
cd kanz && TEST_NATS_URL=nats://localhost:4222 TEST_KAFKA_BROKERS=localhost:9092 \
  GOFLAGS=-mod=mod go test ./services/archiver/internal/archive/ -v -run TestArchiver
```
Expected: all four PASS. **If they SKIP, that is a failure, not a pass** — the env guard is unset. That is exactly how `-tags redis` rotted into dead code before EXEC-M22.

- [ ] **Step 3: Verify the ordering assertion actually bites (mutation check)**

In `archiver.go`, temporarily produce with `Key: nil`.

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/archive/ -run TestArchiver_LandsEveryEventInOrder`
Expected: **FAIL** on the ordering assertion (with >1 partition, an unkeyed produce round-robins and per-key order is lost). Revert and confirm PASS.

If it still passes, your test topic has one partition — recreate it with several, or the ordering claim is untested.

- [ ] **Step 4: Commit**

```bash
git add kanz/services/archiver/internal/archive/archiver_integration_test.go
git commit -m "test(archiver): prove it against a real NATS and a real Kafka — no loss, clean catch-up, red twin"
```

---

## Task 6: Lag metric

The real failure mode is not downtime, it is falling behind. An archiver silently lagging past its stream's max-age is data loss with a delay on it.

**Files:**
- Modify: `kanz/services/archiver/internal/archive/archiver.go`
- Modify: `kanz/services/archiver/cmd/archiver/main.go`
- Test: `kanz/services/archiver/internal/archive/archiver_test.go`

**Interfaces:**
- Consumes: `archive.New/Config/Handle` (Task 3).
- Produces: `archive.NewMetrics(reg prometheus.Registerer) *archive.Metrics`; `archive.Metrics{Archived, Failed *prometheus.CounterVec; Lag *prometheus.GaugeVec}`; **and a new `Metrics *Metrics` field added to `archive.Config`** (it did not exist in Task 3 — add it now).

- [ ] **Step 1: Write the failing test**

Add to `kanz/services/archiver/internal/archive/archiver_test.go`:

```go
import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Lag is the metric that matters. Success and failure counts are how an operator
// sees the archiver refusing events (a NACK loop is invisible otherwise: it logs,
// redelivers, logs, redelivers, and nothing else changes).
func TestHandle_CountsOutcomes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := archive.NewMetrics(reg)

	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "acme", EventId: "evt-1"}
	ok := &fakeKafka{}
	a := archive.New(archive.Config{
		Tenant: "acme", Group: "archiver", Kafka: ok, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err := a.Handle(context.Background(), bus.Message{Body: body(t, e)}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := testutil.ToFloat64(m.Archived.WithLabelValues("acme.order.order")); got != 1 {
		t.Errorf("archived_total = %v, want 1", got)
	}

	bad := &fakeKafka{fail: errors.New("kafka down")}
	a2 := archive.New(archive.Config{
		Tenant: "acme", Group: "archiver", Kafka: bad, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := a2.Handle(context.Background(), bus.Message{Body: body(t, e)}); err == nil {
		t.Fatal("expected a produce failure")
	}
	if got := testutil.ToFloat64(m.Failed.WithLabelValues("acme.order.order", "publish")); got != 1 {
		t.Errorf("failed_total{reason=publish} = %v, want 1", got)
	}
}
```

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/archive/ -v -run TestHandle_CountsOutcomes`
Expected: FAIL — `archive.NewMetrics` undefined.

- [ ] **Step 3: Implement the metrics**

Create `kanz/services/archiver/internal/archive/metrics.go`:

```go
package archive

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the archiver's own counters. Lag (how far behind the spine it is)
// comes from bus.BusMetrics.SetPending — the same kanz_bus_pending_messages gauge
// the risk-engine's KEDA ScaledObject already scales on.
//
// AN ARCHIVER SILENTLY FALLING BEHIND ITS STREAM'S MAX-AGE IS DATA LOSS WITH A
// DELAY ON IT. Being down is survivable and self-healing; being slow is not, and it
// is invisible without this.
type Metrics struct {
	Archived *prometheus.CounterVec // by topic
	Failed   *prometheus.CounterVec // by topic, reason
	Lag      *prometheus.GaugeVec   // by subject: messages pending on the spine
}

// NewMetrics registers the archiver's metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Archived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_archiver_archived_total",
			Help: "Envelopes durably written to the Kafka log of record.",
		}, []string{"topic"}),
		Failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_archiver_failed_total",
			Help: "Envelopes the archiver REFUSED (NACKed and left safe on the spine).",
		}, []string{"topic", "reason"}),
		Lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_archiver_pending_messages",
			Help: "Messages pending on the spine for this archiver's durable consumer.",
		}, []string{"subject"}),
	}
	reg.MustRegister(m.Archived, m.Failed, m.Lag)
	return m
}
```

Then in `archiver.go`, **add** the field `Metrics *Metrics` to `Config` (it did not exist before) and record outcomes in `Handle`. The four call sites, in order:

```go
// after a failed Unframe:
a.observeFail("", "unframe")
// after a failed topic.For:
a.observeFail("", "route")
// after a failed Publish:
a.observeFail(name, "publish")
// after a successful Publish:
if a.cfg.Metrics != nil {
	a.cfg.Metrics.Archived.WithLabelValues(name).Inc()
}
```

with the helper:

```go
func (a *Archiver) observeFail(topic, reason string) {
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.Failed.WithLabelValues(topic, reason).Inc()
	}
}
```

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/archiver/internal/archive/ -v`
Expected: PASS (all Task 3 tests plus the new one).

- [ ] **Step 4b: Wire the lag poller in `main.go`**

In `cmd/archiver/main.go`, after building the archiver, register the metrics and poll each durable consumer's pending count:

```go
metrics := archive.NewMetrics(prometheus.DefaultRegisterer)
// ... pass Metrics: metrics into archive.Config ...

// Poll the spine for how far behind we are. Alert on this, not on pod restarts.
go func() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, subject := range cfg.Subjects {
				pending, err := nc.Pending(ctx, subject, cfg.Group)
				if err != nil {
					logger.Warn("archiver: lag poll failed", "subject", subject, "err", err)
					continue
				}
				metrics.Lag.WithLabelValues(subject).Set(float64(pending))
			}
		}
	}
}()
```

**Check whether `*bus.NATSClient` already exposes a pending/lag accessor** (`bus.BusMetrics.SetPending` exists, so something computes pending today — find it and reuse it). If no exported accessor exists, add one to `pkg/bus/nats.go` rather than reaching into JetStream from the archiver: the bus owns the transport, and a second consumer-info path would be a competing abstraction.

- [ ] **Step 5: Commit**

```bash
git add kanz/services/archiver/
git commit -m "feat(archiver): export lag — falling behind is the failure mode, not being down"
```

---

## Task 7: Deploy, network policy, CI

**Files:**
- Create: `kanz/infra/deploy/archiver-deploy.yaml`
- Modify: `kanz/infra/security/runtime/network-policies.yaml`
- Modify: `.github/workflows/kanz-ci.yml` (test job: add a Kafka service container if absent)
- Modify: `.github/workflows/build.yml` (add `archiver` to the image build matrix)

- [ ] **Step 1: Deployment manifest**

Create `kanz/infra/deploy/archiver-deploy.yaml`, copying the structure of `kanz/infra/deploy/tv-sync-deploy.yaml` (per-tenant env, Vault CSI secret mount, probes, resources). Archiver-specific:

```yaml
spec:
  replicas: 1          # SINGLE WRITER. Two producers can invert per-key order in
                       # Kafka, and a reordered log rebuilds a DIFFERENT book.
  strategy:
    type: Recreate     # Deliberately NOT RollingUpdate, and deliberately the
                       # OPPOSITE call from webhook-ingest (EXEC-M22). For an
                       # INGRESS, a gap is permanent signal loss. For an ARCHIVER, a
                       # gap is deferred work: NATS retains 24h (168h on the money
                       # streams) and the pod catches up on restart. Recreate is what
                       # keeps the single-writer ordering guarantee true across a
                       # rollout — two overlapping pods would not.
```

Env: `ARCHIVER_TENANT`, `ARCHIVER_NATS_URL`, `ARCHIVER_KAFKA_BROKERS`, `ARCHIVER_OTLP_ENDPOINT`.

- [ ] **Step 2: NetworkPolicy**

In `kanz/infra/security/runtime/network-policies.yaml`, add an archiver policy: egress to **NATS and Kafka only** (namespace `kanz-messaging`), plus the OTel collector. No path to the OMS, the venues or webhook-ingest — the archiver reads the spine and writes the log; it has no business anywhere near the capital path.

- [ ] **Step 3: CI**

In `.github/workflows/kanz-ci.yml`, ensure the Go test job has **both** a NATS and a Kafka service container so the Task 5 integration tests actually run (they must not silently skip — that is how `-tags redis` rotted into dead code before EXEC-M22). Follow the Redis service-container pattern EXEC-M22 added.

In `.github/workflows/build.yml`, add `archiver` to the image build matrix.

- [ ] **Step 4: Verify CI runs the integration tests, and that they are not skipping**

Push the branch and read the CI log for the archiver test job. Confirm you see the four `TestArchiver_*` tests **run**, not skip.

- [ ] **Step 5: Commit**

```bash
git add kanz/infra/ .github/workflows/
git commit -m "feat(infra,ci): deploy the archiver (single writer, Recreate); run its broker tests in CI"
```

---

## Task 8: Retire the board and brain flags

**Files:**
- Modify: `KANZ_TASKS.md` — move DATA-M1 out of TODO into DONE (one-line record, following the existing DONE style).
- Modify: `KANZ_BRAIN.md` — retire the **"NOT YET BUILT"** flag on the archiver decision in §Event platform; the bullet becomes a description of what the system does, not a target.

- [ ] **Step 1: Update `KANZ_TASKS.md`** — DATA-M1 to DONE; leave DATA-M2 and RISK-M1 in TODO. Note in DATA-M2 that the log is now non-empty, so the sink has something to land.

- [ ] **Step 2: Update `KANZ_BRAIN.md`** — remove "NOT YET BUILT — DATA-M1; until it ships, read this bullet as a target"; keep the decision itself and the DATA-05 consequence.

- [ ] **Step 3: Full verification before claiming done**

```bash
cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./... && GOFLAGS=-mod=mod go test ./...
```

Expected: all PASS. Then confirm the four `TestArchiver_*` integration tests pass against real brokers (not skipped), and that `TestEverySubjectHasAKafkaTopic` passes.

- [ ] **Step 4: Commit**

```bash
git add KANZ_TASKS.md KANZ_BRAIN.md
git commit -m "docs(board,brain): DATA-M1 done — the log of record now holds the log"
```

---

## Done means

- Every in-scope subject lands in its Kafka topic, in per-key order, acked only after Kafka has the write.
- `TestEverySubjectHasAKafkaTopic` passes, and **fails** when a subject loses its topic (verified by mutation in Task 2, Step 5).
- The Kafka-outage test proves no loss and clean catch-up; the red twin proves the ack-before-produce bug is real (Task 5).
- Archiver lag is exported and alertable (Task 6).
- CI runs the broker-backed tests rather than skipping them (Task 7, Step 4).
- `KANZ_BRAIN.md`'s "NOT YET BUILT" flag is retired (Task 8).
