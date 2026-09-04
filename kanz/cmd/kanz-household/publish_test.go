// THE ENVELOPE THIS CLI PUBLISHES, PINNED AGAINST A REAL PRODUCER (#245).
//
// Before this file, nothing in the package had ever seen bus.Validate — run()
// dials a broker before it constructs anything, so every field of the envelope
// was unreachable without one. That is the shape pkg/bus/producer.go records in
// past tense (twelve publish sites that "validated fine in unit tests … and
// failed on the first real broker") and the shape that put a tenant-less
// producer into production in services/accounting.
//
// WHY IT MATTERS MORE HERE THAN ON AN APPEND-ONLY SUBJECT. The household stream
// is COMPACTED to the last message per household. A bad publish does not sit
// beside the good history for comparison — it REPLACES the household's current
// stated worth, and nothing is left to diff against. So the two fields that
// decide WHERE a valuation lands, the subject and the partition key, are
// asserted as carefully as the ones Validate checks.
//
// TIER-B: a REAL bus.Producer over a FAKE bus.Client. The Producer stamps
// event_id / publish_time / producer_sequence and runs Validate, so a double at
// the Event level would remove exactly the thing under test.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/pkg/bus"
)

// captureClient is a bus.Client that records the framed wire bytes — the Tier-B
// helper from internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func newTestProducer(t *testing.T, tenant string) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "kanz-household",
		ProducerVersion: "1",
		Tenant:          tenant,
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

var asOf = time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)

func valuation() *wealthpb.HouseholdValued {
	return &wealthpb.HouseholdValued{
		HouseholdId:  "hh-1",
		CurrencyCode: "USD",
		AsOf:         timestamppb.New(asOf),
		RecordedBy:   "operator:akif",
		Reason:       "quarter close",
		RiskProfile:  wealthpb.RiskProfile_RISK_PROFILE_GROWTH,
	}
}

// THE ONE THAT MATTERS. Every field asserted here is one Validate rejects when
// it is wrong or absent, so this is the test that would fail if this CLI carried
// cashmove's defect.
func TestHouseholdEventPublishesAValidFactEnvelope(t *testing.T) {
	prod, cc := newTestProducer(t, "eighred")

	if err := prod.Publish(context.Background(), householdEvent(options{tenant: "eighred"}, valuation())); err != nil {
		t.Fatalf("Publish: %v — this CLI's envelope does not survive bus.Validate, which is the "+
			"failure an operator would meet the first time they used it", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}

	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("the emitted envelope fails Validate: %v", err)
	}

	if got := env.GetTenantId(); got != "eighred" {
		t.Errorf("tenant_id = %q, want eighred — a missing tenant is what the bus rejects first, and "+
			"it is how the accounting cash producer was broken in production", got)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", got)
	}
	if got := env.GetDomain(); got != wealth.Domain {
		t.Errorf("domain = %q, want %q", got, wealth.Domain)
	}
	if got := env.GetPayloadSchemaRef(); got != "wealth.v1.HouseholdValued:1" {
		t.Errorf("payload_schema_ref = %q", got)
	}
	// FACT rule (pkg/bus/validate.go): idempotency_key == event_id.
	if env.GetIdempotencyKey() != env.GetEventId() {
		t.Errorf("idempotency_key %q != event_id %q — the FACT rule Validate enforces",
			env.GetIdempotencyKey(), env.GetEventId())
	}
	// as_of, never ingest time: the exposure view aggregates over it, so a
	// wall-clock stamp mislabels a stale valuation as current.
	if got := env.GetEventTime().AsTime(); !got.Equal(asOf) {
		t.Errorf("event_time = %s, want the valuation's own as_of %s", got, asOf)
	}
}

// THE SUBJECT AND THE EVENT TYPE ARE DIFFERENT STRINGS HERE, deliberately, and
// that is worth pinning because every other publisher in this repo sets them to
// the same value — so "they match" is the shape a careless edit would produce.
// The subject is tenant-prefixed (where the message lands); the type is not
// (what the message is).
func TestHouseholdSubjectIsTenantScopedAndTypeIsNot(t *testing.T) {
	prod, cc := newTestProducer(t, "eighred")
	if err := prod.Publish(context.Background(), householdEvent(options{tenant: "eighred"}, valuation())); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	wantSubject := wealth.SubjectHouseholdFor("eighred", "hh-1")
	if got := cc.sent[0].Subject; got != wantSubject {
		t.Errorf("subject = %q, want %q — a valuation on the wrong subject lands in another "+
			"tenant's compacted history", got, wantSubject)
	}
	if !strings.Contains(wantSubject, "eighred") {
		t.Fatalf("this test asserts tenant scoping but %q does not contain the tenant — the subject "+
			"helper changed shape and the assertion above no longer means what it says", wantSubject)
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if got := env.GetEventType(); got != wealth.EventTypeHouseholdValued {
		t.Errorf("event_type = %q, want the un-prefixed %q", got, wealth.EventTypeHouseholdValued)
	}
	if env.GetEventType() == cc.sent[0].Subject {
		t.Error("event_type and subject are now the same string. On this publisher they must differ: " +
			"the subject carries the tenant and the household, the type does not")
	}
}

// THE COMPACTION KEY. This stream keeps only the last message per household, so
// the partition key is not an ordering nicety — it decides WHOSE stated worth
// this message overwrites.
func TestHouseholdPartitionKeyIsTheHouseholdID(t *testing.T) {
	prod, cc := newTestProducer(t, "eighred")
	if err := prod.Publish(context.Background(), householdEvent(options{tenant: "eighred"}, valuation())); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := string(cc.sent[0].Key); got != "hh-1" {
		t.Errorf("partition key = %q, want the household id. This stream is COMPACTED to the last "+
			"message per household, so a wrong key replaces a DIFFERENT household's current stated "+
			"worth, and there is nothing left to diff against afterwards", got)
	}
}

// A TENANT-LESS RUN IS REFUSED BY THE BUS, and this pins that it is refused
// rather than published blank — the accounting defect pinned directly.
func TestHouseholdEventWithNoTenantIsRefused(t *testing.T) {
	prod, cc := newTestProducer(t, "")

	err := prod.Publish(context.Background(), householdEvent(options{tenant: ""}, valuation()))
	if err == nil {
		t.Fatal("a tenant-less household valuation was published. On the live spine bus.Validate " +
			"refuses it, so this CLI would report success while the FACT never left")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("the refusal must name the tenant; got %v", err)
	}
	if len(cc.sent) != 0 {
		t.Errorf("a refused envelope still reached the transport (%d message(s))", len(cc.sent))
	}
}

// A VALUATION WITH NO RISK PROFILE IS REFUSED BEFORE THE NETWORK (#1010).
//
// The profile selects the model portfolio the household's book is measured
// against, and this stream is COMPACTED — a valuation published without one does
// not leave the household's previous profile standing, it ERASES it, and the
// wealth service then reports that household under
// kanz_wealth_drift_evaluations_total{outcome="no_profile"} for good. A custodial
// statement carries no risk profile, so this is precisely the field an operator
// transcribing one omits.
func TestHouseholdValuationWithNoRiskProfileIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hh.json")
	body := `{"householdId":"hh-1","currencyCode":"USD","asOf":"2026-06-30T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := loadValuation(path)
	if err == nil {
		t.Fatal("a valuation with no risk profile was accepted. Published, it would erase the " +
			"household's profile on a compacted subject and nothing would measure its drift again")
	}
	if !strings.Contains(err.Error(), "risk_profile") {
		t.Errorf("the refusal does not name the field, so an operator cannot tell what to add: %v", err)
	}

	withProfile := `{"householdId":"hh-1","currencyCode":"USD","asOf":"2026-06-30T00:00:00Z","riskProfile":"RISK_PROFILE_GROWTH"}`
	if err := os.WriteFile(path, []byte(withProfile), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	hv, err := loadValuation(path)
	if err != nil {
		t.Fatalf("a valuation WITH a profile was refused: %v", err)
	}
	if hv.GetRiskProfile() != wealthpb.RiskProfile_RISK_PROFILE_GROWTH {
		t.Errorf("risk_profile = %v after load, want GROWTH", hv.GetRiskProfile())
	}
}
