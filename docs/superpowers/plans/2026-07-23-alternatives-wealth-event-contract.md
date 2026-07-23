# Alternatives & Wealth Event Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the `alternatives` and `wealth` services a real event contract — named subjects, provisioned streams, proto decoders, operator publishers and wired consumers — so their `Folder.Handle` implementations stop being unreachable code and actually fold what the platform records.

**Architecture:** Follow `cmd/kanz-mandate` exactly. It exists because the mandate registry "was always empty" and "NOTHING PUBLISHED IT" — the identical situation. Both domains' events originate OUTSIDE the platform (GP capital-call and NAV notices from fund administrators; household valuations from advisor onboarding), so the publisher is an operator CLI, not a computing service. The two domains take DIFFERENT stream shapes, and that difference is load-bearing: alternatives is a **journal** whose replay reproduces the position and whose full cashflow history IRR/TVPI are computed from, so it is append-only; wealth is **state** (its decoder returns a whole `Household`, not a delta), so it is compacted per household.

**Tech Stack:** Go, protobuf/buf, NATS JetStream (`pkg/bus`), Kafka (archiver topics), Postgres (pgx).

## Global Constraints

- **The subject-topology arch guards will FAIL THE BUILD if you skip provisioning.** `test/arch/subject_topology_test.go` scans every non-test `.go` file for a const whose name contains `Subject` or `EventType` (case-insensitive) and fails unless the subject is covered by an `ensure_stream` line in `infra/nats/bootstrap-job.yaml`. `test/arch/kafka_topology_test.go` does the same against `infra/kafka/topics-job.yaml`, reducing each subject to `{domain}.{entity}`. Name your constants with `Subject`/`EventType` in them (that is what makes them visible) and add BOTH provisioning rows.
- **Subject grammar is `{domain}.{entity}.{event_type}`**, three `lower_snake_case` segments. Compacted per-entity subjects additionally append routing tokens, exactly as `SubjectMandateFor` appends `.{tenant}.{portfolio}` — see `internal/platform/subject/subject.go`. Journal subjects stay flat and carry identity in `PartitionKey`, exactly as `order.order.filled` does.
- **Money on the wire is `common.v1.Decimal`.** Decoding UNTRUSTED wire input MUST use `dec.FromProtoChecked` (bounds `|exponent| <= 64`, returns `ok=false` you must refuse on), never the unchecked `dec.FromProto`. Producing a Decimal on a capital path MUST use `dec.ToProtoScaled`, never `dec.ToProto` (which wraps above ~$92bn).
- **`internal/wealth` is deliberately `float64`** — its own doc states money is exact Decimal at the wire and lands as float at the analytics edge (EVT-14). Decode Decimal → float64 there; do NOT "fix" the package to big.Rat.
- **Proto changes must be ADDITIVE.** `buf breaking` gates this repo. Add new fields/messages with new field numbers; never renumber or repurpose.
- **`kanz-schemas/gen/go` is generated-not-committed (EVT-15a).** Regenerate with `cd kanz-schemas && buf generate` (the Makefile target is `make generate` from `kanz/`, but `make` is not on PATH on this host — run buf directly).
- Run Go commands from `kanz/`. Do NOT pass `-race` — this host has no C compiler and race builds fail with an unrelated toolchain error.
- If `gofmt` reports differences, check whether they are CRLF artifacts before changing a file. Four files (`services/audit/...`, `internal/marketdata/mark/mark_test.go`) are known pre-existing CRLF noise — leave them.

---

### Task 1: Name the alternatives subjects and provision their stream and topic

**Files:**
- Create: `kanz/internal/alternatives/subject.go`
- Modify: `kanz/infra/nats/bootstrap-job.yaml`, `kanz/infra/nats/bootstrap-job-dev-plaintext.yaml`, `kanz/infra/kafka/topics-job.yaml`

**Interfaces:**
- Produces: `alternatives.SubjectCommitted`, `SubjectCalled`, `SubjectDistributed`, `SubjectMarked`, `Domain`, and `AllSubjects() []string`.

**Why these are flat journal subjects:** a capital call is an EVENT that happened, not the current state of anything. The fold replays them in order to reproduce the position, and IRR/TVPI/DPI are computed from the dated cashflow series. Compacting these — keeping only the last message per subject — would silently destroy the history those metrics are derived from, and the loss would show up as wrong returns, not as an error.

- [ ] **Step 1: Write the subject constants**

Create `kanz/internal/alternatives/subject.go`:

```go
package alternatives

// The commitment-lifecycle subjects (ALT-01b). One subject per event type, flat
// three-segment names carrying identity in the envelope's PartitionKey — the same
// shape as order.order.filled, and for the same reason: these are events that
// happened, not the current state of anything.
//
// THEY MUST NOT BE COMPACTED. The fund position is the FOLD of this journal, and
// IRR/TVPI/DPI are computed from the dated cashflow series it carries. A stream
// that kept only the last message per subject would destroy the history those
// numbers are derived from, and the damage would surface as wrong returns rather
// than as an error — see the ALTERNATIVES stream in infra/nats/bootstrap-job.yaml,
// which is deliberately append-only.
const (
	// Domain is the envelope domain segment for every subject below.
	Domain = "alternatives"

	// SubjectCommitted records a new capital commitment to a fund.
	SubjectCommitted = "alternatives.commitment.committed"
	// SubjectCalled records a capital call drawn against a commitment.
	SubjectCalled = "alternatives.commitment.called"
	// SubjectDistributed records a distribution returned to the LP.
	SubjectDistributed = "alternatives.commitment.distributed"
	// SubjectMarked records a NAV mark of the residual holding.
	SubjectMarked = "alternatives.commitment.marked"
)

// AllSubjects is the set an alternatives consumer subscribes to. It exists so the
// service's default subject list and the operator publisher cannot drift apart.
func AllSubjects() []string {
	return []string{SubjectCommitted, SubjectCalled, SubjectDistributed, SubjectMarked}
}
```

- [ ] **Step 2: Watch the arch guard fail — this is the evidence the guard works**

```bash
cd kanz && go test ./test/arch/ -run TestEverySubjectIsCarriedByAStream 2>&1 | tail -20
```

Expected: FAIL, naming the four new `alternatives.commitment.*` subjects as carried by no stream. If it PASSES, the scanner did not see your constants — check that their names contain `Subject` and that the file is not `_test.go`.

Also run:

```bash
cd kanz && go test ./test/arch/ -run TestEverySubjectHasAKafkaTopic 2>&1 | tail -20
```

Expected: FAIL, naming `alternatives.commitment`.

- [ ] **Step 3: Provision the NATS stream**

In `kanz/infra/nats/bootstrap-job.yaml`, beside the other `ensure_stream` lines, add:

```
ensure_stream ALTERNATIVES  "alternatives.>" 168h
```

Match the surrounding lines' column alignment. Note there is NO `--max-msgs-per-subject=1` here, and that omission is the whole point — contrast the MANDATE line above it, which has it because a mandate is state. 168h matches ACCOUNTING, the closest money-carrying analogue; the durable record is the service's Postgres store, and the stream's retention only bounds how far a redelivery or replay can reach back.

Make the identical addition to `kanz/infra/nats/bootstrap-job-dev-plaintext.yaml` so the rig provisions it too. Read that file first — if its `ensure_stream` helper differs, match ITS shape rather than pasting.

- [ ] **Step 4: Provision the Kafka topic**

In `kanz/infra/kafka/topics-job.yaml`, following the existing table format (`name partitions cleanup retention_ms dlq`), add:

```
alternatives.commitment 3 delete 604800000 yes
```

`delete`, not `compact` — same reasoning as the stream. 604800000 ms = 7 days, matching the 168h stream.

- [ ] **Step 5: Both guards go green**

```bash
cd kanz && go test ./test/arch/ -count=1 2>&1 | tail -5
```

Expected: PASS. The two guards that failed in Step 2 now pass, and nothing else broke.

- [ ] **Step 6: Commit**

```bash
git add kanz/internal/alternatives/subject.go kanz/infra/nats/bootstrap-job.yaml kanz/infra/nats/bootstrap-job-dev-plaintext.yaml kanz/infra/kafka/topics-job.yaml
git commit -m "feat(alternatives): name the commitment-lifecycle subjects and provision them

The alternatives Folder has always been bus-handler-shaped and wired to nothing,
because no subject existed for it to consume. These are flat journal subjects,
deliberately NOT compacted: the fund position is the fold of this journal and
IRR/TVPI are computed from its dated cashflow series, so keeping only the last
message per subject would destroy the history the numbers come from.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Decode the alternatives protos into the fold's Event

**Files:**
- Create: `kanz/services/alternatives/internal/consume/proto.go`
- Test: `kanz/services/alternatives/internal/consume/proto_test.go`

**Interfaces:**
- Consumes: `alternatives.Subject*` (Task 1); `alternatives.Event`/`EventType` (`internal/alternatives/position.go:19-44`); the generated `alternativespb` SDK.
- Produces: `func DecodeProto(eventType string) Decoder` — returns a `Decoder` bound to one event type, for the composition root to wire per subscription.

**Why this shape:** `consume.go:30` already anticipates it — *"The default is DecodeJSON; a composition root with the generated alternatives.v1 SDK swaps in a proto decoder behind this seam."* The seam was built for this; do not change `Decoder`, `Folder` or `Handle`.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/alternatives/internal/consume/proto_test.go`:

```go
package consume

import (
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	altpb "github.com/kanz-eng/kanz-schemas-go/alternatives/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	alt "github.com/kanz-eng/kanz/internal/alternatives"
)

func TestDecodeProto_CapitalCall(t *testing.T) {
	body, err := proto.Marshal(&altpb.CapitalCall{
		CallId:       "call-1",
		CommitmentId: "c-1",
		Amount:       &commonpb.Decimal{Coefficient: 250000, Exponent: 0},
		CalledAt:     timestamppb.New(t0()),
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodeProto(alt.SubjectCalled)(body)
	if err != nil {
		t.Fatalf("DecodeProto: %v", err)
	}
	if e.EventID != "call-1" || e.CommitmentID != "c-1" {
		t.Fatalf("identity = %q/%q, want call-1/c-1", e.EventID, e.CommitmentID)
	}
	if e.Type != alt.EventCall {
		t.Fatalf("type = %v, want EventCall", e.Type)
	}
	if e.Amount.RatString() != "250000" {
		t.Fatalf("amount = %s, want 250000", e.Amount.RatString())
	}
}

// AN OUT-OF-RANGE EXPONENT MUST BE REFUSED, NOT COERCED.
//
// dec.FromProtoChecked exists precisely because this payload is untrusted wire
// input: an exponent outside +/-64 is not a small number, it is a number this
// platform cannot represent, and substituting zero would book a capital call of
// nothing while reporting success.
func TestDecodeProto_RefusesUnrepresentableAmount(t *testing.T) {
	body, err := proto.Marshal(&altpb.CapitalCall{
		CallId:       "call-2",
		CommitmentId: "c-1",
		Amount:       &commonpb.Decimal{Coefficient: 1, Exponent: 9999},
		CalledAt:     timestamppb.New(t0()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProto(alt.SubjectCalled)(body); err == nil {
		t.Fatal("an unrepresentable amount decoded without error — a call of an " +
			"amount we cannot represent must be refused, never silently zeroed")
	}
}

func TestDecodeProto_UnknownSubjectIsRefused(t *testing.T) {
	if _, err := DecodeProto("alternatives.commitment.invented")(nil); err == nil {
		t.Fatal("an unknown event type produced a decoder instead of an error")
	}
}
```

Add a `t0()` helper to this file if the package has none (check first with `grep -rn "func t0" kanz/services/alternatives/internal/consume/`):

```go
func t0() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }
```

- [ ] **Step 2: Run it and confirm it fails**

```bash
cd kanz && go test ./services/alternatives/internal/consume/ -run TestDecodeProto -v 2>&1 | tail -10
```

Expected: FAIL to compile — `undefined: DecodeProto`.

- [ ] **Step 3: Write the decoder**

Create `kanz/services/alternatives/internal/consume/proto.go`. Read `kanz-schemas/proto/alternatives/v1/alternatives.proto` FIRST and use the real field names — the getters below are the expected shape, but the proto is the authority and a guessed field name is exactly the defect this repo's board records as "only firing the thing catches":

```go
package consume

import (
	"fmt"
	"math/big"

	altpb "github.com/kanz-eng/kanz-schemas-go/alternatives/v1"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	alt "github.com/kanz-eng/kanz/internal/alternatives"
	"github.com/kanz-eng/kanz/internal/dec"
)

// DecodeProto returns the Decoder for one commitment-lifecycle subject. The
// composition root binds one per subscription, which is why this is a factory
// rather than a single function: the wire payload's TYPE is determined by the
// subject it arrived on, and nothing in the bytes themselves says which it is.
//
// It replaces DecodeJSON on the live path. DecodeJSON decodes the Go-native
// alternatives.Event as JSON, which is not what an EventFrame carries — it was a
// placeholder for exactly this, and consume.go's own doc says so.
func DecodeProto(eventType string) Decoder {
	return func(payload []byte) (*alt.Event, error) {
		switch eventType {
		case alt.SubjectCommitted:
			var m altpb.Commitment
			if err := proto.Unmarshal(payload, &m); err != nil {
				return nil, fmt.Errorf("consume: %s: %w", eventType, err)
			}
			return event(m.GetCommitmentId(), m.GetCommitmentId(), alt.EventCommit,
				m.GetCommittedAmount(), m.GetCommittedAt())
		case alt.SubjectCalled:
			var m altpb.CapitalCall
			if err := proto.Unmarshal(payload, &m); err != nil {
				return nil, fmt.Errorf("consume: %s: %w", eventType, err)
			}
			return event(m.GetCallId(), m.GetCommitmentId(), alt.EventCall,
				m.GetAmount(), m.GetCalledAt())
		case alt.SubjectDistributed:
			var m altpb.Distribution
			if err := proto.Unmarshal(payload, &m); err != nil {
				return nil, fmt.Errorf("consume: %s: %w", eventType, err)
			}
			return event(m.GetDistributionId(), m.GetCommitmentId(), alt.EventDistribution,
				m.GetAmount(), m.GetDistributedAt())
		case alt.SubjectMarked:
			var m altpb.NAVMark
			if err := proto.Unmarshal(payload, &m); err != nil {
				return nil, fmt.Errorf("consume: %s: %w", eventType, err)
			}
			return event(m.GetMarkId(), m.GetCommitmentId(), alt.EventNAVMark,
				m.GetNav(), m.GetAsOf())
		default:
			return nil, fmt.Errorf("consume: no decoder for event type %q — the "+
				"subject determines the payload type, so a subject this build does "+
				"not know is a wiring error, not a bad message", eventType)
		}
	}
}

// event builds the fold's Event, refusing anything it cannot represent exactly.
func event(id, commitmentID string, typ alt.EventType, amt *commonpb.Decimal, at *timestamppb.Timestamp) (*alt.Event, error) {
	if id == "" {
		return nil, fmt.Errorf("consume: event has no id")
	}
	if commitmentID == "" {
		return nil, fmt.Errorf("consume: event %s names no commitment", id)
	}
	// FromProtoChecked, not FromProto: this is untrusted wire input. An exponent
	// outside the representable range is not a small number — it is one this
	// platform cannot hold, and coercing it to zero would record a capital call
	// of nothing and report success.
	r, ok := dec.FromProtoChecked(amt)
	if !ok {
		return nil, fmt.Errorf("consume: event %s carries an amount this platform "+
			"cannot represent exactly; refusing rather than rounding it", id)
	}
	if r == nil {
		r = new(big.Rat)
	}
	return &alt.Event{
		EventID:      id,
		CommitmentID: commitmentID,
		Type:         typ,
		Amount:       r,
		Date:         at.AsTime().UTC(),
	}, nil
}
```

- [ ] **Step 4: Run the tests**

```bash
cd kanz && go test ./services/alternatives/internal/consume/ -count=1 -v 2>&1 | tail -12
```

Expected: all PASS, including the pre-existing `DecodeJSON` tests, which must keep working — `DecodeJSON` stays as the default for callers that inject nothing.

- [ ] **Step 5: Commit**

```bash
git add kanz/services/alternatives/internal/consume/proto.go kanz/services/alternatives/internal/consume/proto_test.go
git commit -m "feat(alternatives): decode the commitment protos behind the Decoder seam

consume.go always said a composition root with the alternatives.v1 SDK would swap
a proto decoder in behind this seam; DecodeJSON was the placeholder standing in
for it, and it decodes a Go struct as JSON, which is not what an EventFrame
carries. Amounts go through dec.FromProtoChecked because this is untrusted wire
input: an unrepresentable amount is refused rather than rounded to zero.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Wire the alternatives consumer

**Files:**
- Modify: `kanz/services/alternatives/internal/config/config.go`, `kanz/services/alternatives/cmd/alternatives/main.go`

**Interfaces:**
- Consumes: `DecodeProto` (Task 2), `alternatives.AllSubjects()` (Task 1)
- Produces: a running consumer folding into the same `fund.Store` the HTTP surface already reads.

- [ ] **Step 1: Add the config fields**

In `kanz/services/alternatives/internal/config/config.go`, add fields mirroring `services/accounting/internal/config/config.go` exactly — read it first and match its naming, defaulting and `secret()`/`envOr()` helpers:

- `NATSURL` from `ALTERNATIVES_NATS_URL`
- `Source` from `ALTERNATIVES_SOURCE`, default `"alternatives"`
- `ConsumerGroup` from `ALTERNATIVES_CONSUMER_GROUP`, default `"alternatives"`
- `Subjects` from `ALTERNATIVES_SUBJECTS` (comma-split), defaulting to `alternatives.AllSubjects()`
- `SPIFFESocket` from `SPIFFE_ENDPOINT_SOCKET`

Document `Subjects` with a line saying the default is the full lifecycle set and that dropping one silently stops folding that event type.

- [ ] **Step 2: Wire the consumer in main**

In `kanz/services/alternatives/cmd/alternatives/main.go`, add a `runConsumer` following `services/accounting/cmd/accounting/main.go`'s shape — read it and mirror it, including the mesh, the DLQ, the per-subject goroutine and the fail-fast `once.Do`. Start it only when `cfg.NATSURL != ""`, exactly as accounting does, so the HTTP-only deployment keeps working.

The load-bearing detail: **one Folder per subject**, each with `consume.DecodeProto(subject)`, because the decoder is bound to the payload type the subject carries.

```go
// The journal folder, one per subject: the decoder is bound to the payload type
// its subject carries, because nothing in the bytes says which message they are.
for _, subject := range cfg.Subjects {
	folder, err := consume.NewFolder(store, consume.DecodeProto(subject))
	if err != nil {
		return err
	}
	subscribe(subject, folder.Handle)
}
```

Wire `bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))` — `WithDLQ` is not optional; `test/arch/bus_dlq_test.go` fails the build without it, and for good reason.

- [ ] **Step 3: Verify it builds and the estate stays green**

```bash
cd kanz && go build ./... && go vet ./services/alternatives/... && go test ./services/alternatives/... ./test/arch/ -count=1 2>&1 | tail -6
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add kanz/services/alternatives/internal/config/config.go kanz/services/alternatives/cmd/alternatives/main.go
git commit -m "feat(alternatives): consume the commitment journal into the fund store

The Folder has been bus-handler-shaped and unreachable since it was written: no
NewConsumer call site existed, so it was invisible even to the estate-wide DLQ
guard. It now folds the lifecycle subjects into the same fund.Store the HTTP
surface already serves from. One Folder per subject, since the decoder is bound
to the payload type its subject carries.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: `kanz-altevent` — the operator publisher

**Files:**
- Create: `kanz/cmd/kanz-altevent/main.go`

**Interfaces:**
- Consumes: `alternatives.Subject*` (Task 1), the `alternativespb` SDK
- Produces: a CLI that publishes one commitment-lifecycle FACT.

**Why a CLI:** capital calls, distributions and NAV marks arrive as GP and administrator notices from outside this platform. Nothing in kanz computes them, so nothing in kanz can publish them on its own. `cmd/kanz-mandate` is the same situation and the same answer.

- [ ] **Step 1: Write it, modelled on `cmd/kanz-mandate/main.go`**

Read `cmd/kanz-mandate/main.go` in full first and mirror its structure: flags (`-nats`, `-spiffe-socket`, `-tenant`, `-file`, `-by`, `-reason`, `-dry-run`), `transport.NewMesh` → `bus.DialNATS` → `bus.NewProducer` with `Tenant` SET, protojson input loading with validation BEFORE any network I/O, `run()` returning an error with `main()` printing to stderr and `os.Exit(1)`, and the `PUBLISHED —` success line.

Differences from kanz-mandate, all deliberate:

- It takes a `-kind` flag (`commit|call|distribution|navmark`) selecting the subject and the proto message, because unlike a mandate there are four event types.
- `EventClass` is `EVENT_CLASS_FACT`, `SchemaVersion: 1`, `Domain: alternatives.Domain`.
- `PartitionKey` is the **commitment_id**, so every event for one commitment is ordered — the fold depends on that ordering.
- `PayloadSchemaRef` is the proto's full name and version, e.g. `"alternatives.v1.CapitalCall:1"`.
- `EventTime` is the event's own dated timestamp (`called_at` / `distributed_at` / `as_of` / `committed_at`), NOT `time.Now()`. These are dated business events and the IRR is computed from those dates; stamping ingest time would silently corrupt the return calculation.

Validate before publishing: the file's `tenant_id` (if the message carries one) must match `-tenant`; the commitment id and the event id must be non-empty; the amount must be present and must survive `dec.FromProtoChecked`. State in the refusal message why a bad publish matters — unlike the compacted mandate stream a journal keeps every message, so a wrong event stays in the fold until somebody writes a correcting one.

- [ ] **Step 2: Build and dry-run it**

```bash
cd kanz && go build ./cmd/kanz-altevent/ && go vet ./cmd/kanz-altevent/
```

Then write a sample call file to the scratchpad and dry-run:

```bash
cat > /tmp/call.json <<'JSON'
{"call_id":"call-1","commitment_id":"c-1","amount":{"coefficient":"250000","exponent":0},"called_at":"2026-06-01T00:00:00Z"}
JSON
cd kanz && go run ./cmd/kanz-altevent -kind call -tenant __system__ -file /tmp/call.json -by operator:you -reason "GP notice 2026-06" -dry-run
```

Expected: prints the subject `alternatives.commitment.called` and stops without publishing. If protojson rejects the file, the field names differ from the plan's guess — read the proto and correct the SAMPLE, not the decoder.

- [ ] **Step 3: Commit**

```bash
git add kanz/cmd/kanz-altevent/main.go
git commit -m "feat(cmd): kanz-altevent publishes the commitment lifecycle

Capital calls, distributions and NAV marks arrive as GP and administrator notices
from outside this platform; nothing in kanz computes them, so nothing in kanz
could publish them. Same situation kanz-mandate was written for, same answer.

EventTime is the event's own dated timestamp, not ingest time: IRR is computed
from those dates, so stamping now() would corrupt the return silently.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Model household valuation in `wealth.v1` — the wealth blocker

**Files:**
- Modify: `kanz-schemas/proto/wealth/v1/wealth.proto`

**Interfaces:**
- Produces: `wealth.v1.HouseholdValued` carrying accounts → holdings → market values.

**The blocker, stated plainly:** the wealth Folder folds a whole `wealth.Household` — `{HouseholdID, Accounts[{AccountID, Holdings[{InstrumentID, AssetClass, MarketValue}], Cash}]}`. The existing `wealth.v1.Household` carries `household_id`, `name`, `account_ids`, `risk_profile`, `currency_code`, and `wealth.v1.Account` carries `account_id`, `owner`, `account_type`, `currency_code`. Those model REGISTRY data — who owns which account and what risk profile they have. They carry no holdings, no cash and no market value, so publishing them could not feed the fold. The schema is missing the valuation message, and that is why wealth could never have been wired.

- [ ] **Step 1: Add the message**

In `kanz-schemas/proto/wealth/v1/wealth.proto`, append (do NOT modify `Account` or `Household` — additive only, `buf breaking` gates this):

```protobuf
// HouseholdValued is a household's VALUATION at a point in time: every account,
// what it holds, and what those holdings are worth. It is the state the advisory
// exposure view aggregates over.
//
// It is separate from Household and Account, which model the REGISTRY — who owns
// which account, its type, the household's risk profile. Those carry no holdings
// and no market value, so they cannot feed a valuation fold; conflating the two
// is why the wealth service's consumer could not be wired at all.
//
// It is published COMPACTED, one retained message per household: this is the
// current picture, not an event that happened, and a booting pod must arm itself
// with the latest one. Contrast alternatives, whose journal must keep every event.
message HouseholdValued {
  // household_id is the aggregate identity and the compaction key. Required.
  string household_id = 1;

  // accounts is the full set for this household — a REPLACEMENT, not a delta.
  // A consumer folds it last-write-wins, so an account omitted here is an account
  // the household no longer has.
  repeated ValuedAccount accounts = 2;

  // as_of is the valuation time these market values are marked at. Required.
  google.protobuf.Timestamp as_of = 3;

  // currency_code is the reporting currency every market value below is expressed
  // in. Required — a number with no currency is not a value.
  string currency_code = 4;
}

// ValuedAccount is one account's holdings and cash at as_of.
message ValuedAccount {
  // account_id joins to Account for the registry attributes. Required.
  string account_id = 1;

  // holdings are the account's positions, marked to the reporting currency.
  repeated ValuedHolding holdings = 2;

  // cash is the un-invested balance. It carries no instrument and still counts
  // toward the household's total value.
  common.v1.Decimal cash = 3;
}

// ValuedHolding is one position, marked.
message ValuedHolding {
  // instrument_id is the Kanz canonical instrument identifier. Required.
  string instrument_id = 1;

  // asset_class groups holdings for the asset-class exposure view. Required —
  // the view is the point, and an unclassified holding silently vanishes from it.
  string asset_class = 2;

  // market_value is the position's value in the parent HouseholdValued's
  // currency_code. Exact Decimal on the wire; the analytics edge lands it as
  // float64 (EVT-14).
  common.v1.Decimal market_value = 3;
}
```

Confirm the file already imports `google/protobuf/timestamp.proto` and `common/v1/common.proto`; add whichever is missing.

- [ ] **Step 2: Regenerate and confirm the SDK carries it**

```bash
cd kanz-schemas && buf generate
grep -c "type HouseholdValued struct\|type ValuedAccount struct\|type ValuedHolding struct" gen/go/wealth/v1/wealth.pb.go
```

Expected: `3`.

- [ ] **Step 3: Confirm the addition is not breaking**

```bash
cd kanz-schemas && buf breaking --against '.git#branch=main' 2>&1 | tail -5
```

Expected: no breaking-change findings. New messages are additive. If buf cannot resolve the git ref on this host, note that in the report and rely on the fact that only new messages were added — but say so plainly rather than claiming the check passed.

- [ ] **Step 4: Commit**

```bash
git add kanz-schemas/proto/wealth/v1/wealth.proto
git commit -m "feat(schema): model household valuation, which wealth.v1 never did

The wealth service folds a household's accounts, holdings and market values.
wealth.v1's Household and Account model the REGISTRY — owner, account type, risk
profile — and carry no holdings, no cash and no market value, so nothing that
could be published would feed the fold. That missing message is why the wealth
consumer could never be wired.

Additive: Household and Account are untouched.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Name the wealth subject and provision its compacted stream

**Files:**
- Create: `kanz/internal/wealth/subject.go`
- Modify: `kanz/infra/nats/bootstrap-job.yaml`, `kanz/infra/kafka/topics-job.yaml`, `kanz/infra/kafka/tenancy.yaml`

**Two provisioning facts learned in Task 1 — they save you a wrong turn:**

1. **`bootstrap-job-dev-plaintext.yaml` needs no edit.** It carries no `ensure_stream` lines of its own; it mounts the same `nats-bootstrap` ConfigMap, so editing `bootstrap-job.yaml` already covers the rig.
2. **There is a THIRD guard the plan originally missed.** `TestTenantTopicTableMatchesTheSystemTable` requires `infra/kafka/tenancy.yaml`'s per-tenant table to mirror `topics-job.yaml` exactly. Add the identical row to BOTH files or the arch suite fails.

**Interfaces:**
- Produces: `wealth.Domain`, `wealth.EventTypeHouseholdValued`, `wealth.SubjectHouseholdAll`, `func SubjectHouseholdFor(tenant, householdID string) string`

**Why compacted, unlike alternatives:** the decoder returns a whole `Household` and the store does `Put` — last-write-wins. This is the current picture, not an event that happened, so a pod booting tomorrow must arm itself with the latest per household. That is the MANDATE and POSITION shape exactly.

- [ ] **Step 1: Write the subject helpers**

Create `kanz/internal/wealth/subject.go`, modelling `internal/platform/subject/subject.go`'s routing-token convention — read it and match how `SubjectMandateFor` appends `.{tenant}.{portfolio}`:

```go
package wealth

import "fmt"

// The household valuation subject (WEALTH-01b).
//
// COMPACTED, one retained message per household — the opposite of alternatives'
// journal, and deliberately so. A HouseholdValued is the CURRENT picture, not an
// event that happened: the fold is last-write-wins and a pod booting tomorrow must
// arm itself with the latest valuation per household. That is the same shape as
// compliance.mandate and risk.position; see the WEALTH stream in
// infra/nats/bootstrap-job.yaml, which carries --max-msgs-per-subject=1.
const (
	// Domain is the envelope domain segment.
	Domain = "wealth"

	// EventTypeHouseholdValued is the envelope event_type. The subject a message
	// rides appends routing tokens (see SubjectHouseholdFor); this stays the flat
	// three-segment logical name.
	EventTypeHouseholdValued = "wealth.household.valued"

	// SubjectHouseholdAll is the wildcard a consumer subscribes to.
	SubjectHouseholdAll = "wealth.household.valued.>"
)

// SubjectHouseholdFor is the subject one household's valuation is published on.
// The tenant and household tokens are what make compaction per-household rather
// than per-domain: with a single flat subject the stream would retain exactly one
// valuation for the entire platform.
func SubjectHouseholdFor(tenant, householdID string) string {
	return fmt.Sprintf("%s.%s.%s", EventTypeHouseholdValued, tenant, householdID)
}
```

- [ ] **Step 2: Watch both arch guards fail**

```bash
cd kanz && go test ./test/arch/ -run 'TestEverySubjectIsCarriedByAStream|TestEverySubjectHasAKafkaTopic' 2>&1 | tail -15
```

Expected: FAIL, naming the wealth subject.

- [ ] **Step 3: Provision**

`bootstrap-job.yaml` (and the dev-plaintext twin), beside MANDATE:

```
ensure_stream WEALTH        "wealth.>" 0 --max-msgs-per-subject=1
```

`topics-job.yaml` AND `tenancy.yaml` (the same row in both — see the note above):

```
wealth.household 3 compact -1 yes
```

`compact` and `-1`, matching `compliance.mandate` — this is state, and it must not age out.

- [ ] **Step 4: Guards green**

```bash
cd kanz && go test ./test/arch/ -count=1 2>&1 | tail -4
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add kanz/internal/wealth/subject.go kanz/infra/nats/bootstrap-job.yaml kanz/infra/nats/bootstrap-job-dev-plaintext.yaml kanz/infra/kafka/topics-job.yaml
git commit -m "feat(wealth): name the household valuation subject, compacted per household

A HouseholdValued is the current picture, not an event that happened: the fold is
last-write-wins and a booting pod must arm itself with the latest per household.
The tenant and household routing tokens are what make compaction per-household —
on a single flat subject the stream would retain one valuation for the platform.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Decode `HouseholdValued` and wire the wealth consumer

**Files:**
- Create: `kanz/services/wealth/internal/consume/proto.go`, `kanz/services/wealth/internal/consume/proto_test.go`
- Modify: `kanz/services/wealth/internal/config/config.go`, `kanz/services/wealth/cmd/wealth/main.go`

**Interfaces:**
- Consumes: `wealth.v1.HouseholdValued` (Task 5), `wealth.SubjectHouseholdAll` (Task 6)
- Produces: `func DecodeProto(payload []byte) (wealth.Household, error)`, and a running consumer.

Unlike alternatives this is a single message type on a single subject, so it is a plain `Decoder`, not a factory.

- [ ] **Step 1: Write the failing test**

Create `proto_test.go` asserting: a `HouseholdValued` with two accounts, one holding each plus cash, decodes to the Go `wealth.Household` with matching ids, asset classes and market values; and that an unrepresentable `market_value` is REFUSED rather than coerced. Model the assertions on Task 2's tests.

- [ ] **Step 2: Confirm it fails**

```bash
cd kanz && go test ./services/wealth/internal/consume/ -run TestDecodeProto -v 2>&1 | tail -8
```

Expected: FAIL to compile — `undefined: DecodeProto`.

- [ ] **Step 3: Write the decoder**

Create `kanz/services/wealth/internal/consume/proto.go`. Map `HouseholdValued` → `wealth.Household`, `ValuedAccount` → `wealth.Account`, `ValuedHolding` → `wealth.Holding`.

Money crosses a boundary here and the comment must say so: `internal/wealth` is float64 by design (EVT-14 — exact Decimal at the wire, float at the analytics edge), so decode each `common.v1.Decimal` with `dec.FromProtoChecked` and convert the resulting `*big.Rat` to float64 via its `Float64()`. Refuse when `FromProtoChecked` returns `ok=false`. Do NOT change `internal/wealth` to big.Rat — that package's float64 stance is deliberate and documented.

- [ ] **Step 4: Config and wiring**

Mirror Task 3 exactly, for wealth: `WEALTH_NATS_URL`, `WEALTH_SOURCE`, `WEALTH_CONSUMER_GROUP`, `WEALTH_SUBJECTS` (default `[]string{wealth.SubjectHouseholdAll}`), `SPIFFE_ENDPOINT_SOCKET`; `runConsumer` following accounting's shape with `bus.WithDLQ`.

One Folder suffices here — a single subject, a single message type.

- [ ] **Step 5: Verify**

```bash
cd kanz && go build ./... && go vet ./services/wealth/... && go test ./services/wealth/... ./test/arch/ -count=1 2>&1 | tail -6
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add kanz/services/wealth/internal/consume/ kanz/services/wealth/internal/config/config.go kanz/services/wealth/cmd/wealth/main.go
git commit -m "feat(wealth): consume household valuations into the book

The Folder has been bus-handler-shaped and unreachable since it was written, and
could not have been wired before wealth.v1 modelled valuation at all. Decimal
crosses to float64 at this boundary by design (EVT-14); an unrepresentable market
value is refused rather than rounded.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: `kanz-household` — the wealth publisher

**Files:**
- Create: `kanz/cmd/kanz-household/main.go`

Model on `cmd/kanz-mandate/main.go` — this one is the closer analogue of the two publishers, because like a mandate it publishes compacted state.

- [ ] **Step 1: Write it**

Flags as kanz-mandate. Publish a `wealth.v1.HouseholdValued` loaded from protojson on `wealth.SubjectHouseholdFor(tenant, householdID)`, with `EventType: wealth.EventTypeHouseholdValued`, `EventClass: EVENT_CLASS_FACT`, `SchemaVersion: 1`, `Domain: wealth.Domain`, `PartitionKey: household_id`, `TenantID: tenant`, `PayloadSchemaRef: "wealth.v1.HouseholdValued:1"`, `EventTime: as_of`.

Validate before publishing, and say why in the refusal text: the stream keeps only the LAST message per household, so a bad publish is not recoverable without another good one — the same reasoning kanz-mandate gives. Require `household_id`, `as_of`, `currency_code`, and that every market value survives `dec.FromProtoChecked`.

- [ ] **Step 2: Build and dry-run**

```bash
cd kanz && go build ./cmd/kanz-household/ && go vet ./cmd/kanz-household/
```

Dry-run with a sample file and confirm it prints the per-household subject and does not publish.

- [ ] **Step 3: Commit**

```bash
git add kanz/cmd/kanz-household/main.go
git commit -m "feat(cmd): kanz-household publishes a household valuation

Household valuations arrive from advisor onboarding and custodial feeds outside
this platform. Compacted state, so validation happens before publish: the stream
keeps only the last message per household and a bad one is unrecoverable without
another good one.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: Prove both loops on the rig

**Files:** none (verification only)

- [ ] **Step 1: Full gate**

```bash
cd kanz && go build ./... && go vet ./... && go test ./... -count=1 2>&1 | tail -20
cd kanz && gofmt -l internal/ services/ cmd/
```

Expected: no failures. The four known CRLF gofmt hits are pre-existing — ignore them, do not "fix" files this plan did not touch.

- [ ] **Step 2: Provision the new streams on the rig**

```bash
kubectl -n kanz-messaging delete job nats-bootstrap-dev-plaintext --ignore-not-found=true
kubectl apply -f kanz/infra/nats/bootstrap-job-dev-plaintext.yaml
kubectl -n kanz-messaging wait --for=condition=complete job/nats-bootstrap-dev-plaintext --timeout=240s
```

Then confirm both streams exist:

```bash
kubectl -n kanz-messaging run natsbox-$RANDOM --rm -i --restart=Never --image=natsio/nats-box:latest --command -- \
  nats --server nats://nats.kanz-messaging.svc:4222 stream ls
```

Expected: `ALTERNATIVES` and `WEALTH` present alongside the existing streams.

- [ ] **Step 3: Publish and prove the alternatives fold**

Publish a commitment and a capital call against it with `kanz-altevent`, port-forwarding NATS as the `kanz-e2e-cluster-proof` memory describes. Then query the alternatives service's HTTP position endpoint and confirm the committed and called amounts are what you published.

The assertion that matters: the position reflects BOTH events in order. A fold that shows only the last one means the stream compacted the journal — check the `ensure_stream` line has no `--max-msgs-per-subject`.

- [ ] **Step 4: Publish and prove the wealth fold**

Publish a `HouseholdValued` with `kanz-household`, then query the wealth service's household endpoint and confirm the accounts, holdings and market values match.

Then publish a SECOND valuation for the same household with a different market value and confirm the endpoint shows the new one — last-write-wins is the intended behaviour here, and it is the opposite of Task 3's assertion. Confirm too that the stream retains ONE message for that household's subject.

- [ ] **Step 5: Record on the board**

Add a row to `KANZ_TASKS.md` recording what is now wired and what is not: both services consume and fold; the publishers are operator CLIs because these events originate outside the platform; and — stated plainly — **no automated producer exists**, so nothing flows until an operator runs the CLI. That is the honest limit and it must not read as "the integration is live".

```bash
bash tools/validate-board.sh KANZ_TASKS.md
```

---

## Verification

Complete when:

1. `cd kanz && go build ./... && go test ./... -count=1` is green.
2. `go test ./test/arch/` passes — both topology guards see the new subjects and find their stream and topic rows.
3. `TestDecodeProto_RefusesUnrepresentableAmount` passes in BOTH consume packages.
4. `ALTERNATIVES` and `WEALTH` streams exist on the rig, the first WITHOUT `--max-msgs-per-subject` and the second WITH it.
5. An alternatives commitment + call published by CLI appears in the service's position, both events folded.
6. A household valuation published by CLI appears in the wealth book, and a second valuation replaces it.
7. `bash tools/validate-board.sh KANZ_TASKS.md` exits 0.

**Known limits, to be recorded on the board rather than glossed:**

- **There is no automated producer.** Both publishers are operator CLIs. Nothing in the platform emits these events on its own, so the integration is live only insofar as an operator runs the tool. Wiring a real administrator feed or custodial file drop is separate work.
- **`wealth.v1.Household`/`Account` remain registry-only.** `HouseholdValued` is a parallel valuation message; nothing reconciles the two, so an account present in one and absent from the other is not detected.
- **The alternatives journal's durable record is the service's Postgres store, not the stream** — the 168h retention bounds replay, not history. Rebuilding a fund position from scratch after that window requires the database, and no tooling replays it.
