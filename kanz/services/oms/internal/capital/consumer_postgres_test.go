package capital

import (
	"context"
	"errors"
	"testing"

	"github.com/eighred/kanz/internal/cashview"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

func cashEnvelope() *envelopepb.Envelope {
	return &envelopepb.Envelope{
		TenantId: "tenant-a", EventType: cashview.Subject, Domain: "accounting",
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1,
		PayloadSchemaRef: "accounting.v1.PortfolioCashBalance:1", PartitionKey: "fund",
		EventTime: coverageBalance().AsOf,
	}
}

func TestAccountingDeliveryScopeCannotPoisonAnotherTenant(t *testing.T) {
	p := database(t)
	ctx := bus.WithTenantID(context.Background(), "tenant-a")
	for _, change := range []func(*envelopepb.Envelope){
		func(e *envelopepb.Envelope) { e.TenantId = "tenant-b" },
		func(e *envelopepb.Envelope) { e.TenantId = "" },
	} {
		e := cashEnvelope()
		change(e)
		if err := ApplyEnvelope(ctx, p, "tenant-a", e, []byte{0xff}); err == nil {
			t.Fatal("foreign delivery accepted")
		}
	}
	if err := ApplyEnvelope(context.Background(), p, "tenant-a", cashEnvelope(), []byte{0xff}); err == nil {
		t.Fatal("unscoped context accepted")
	}
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM capital_source_health").Scan(&n); err != nil || n != 0 {
		t.Fatalf("foreign event changed tenant safety state: count=%d err=%v", n, err)
	}
	if err := ApplyEnvelope(ctx, p, "tenant-a", cashEnvelope(), sourcePayload(t, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestContradictoryCashEnvelopeDurablyQuarantinesSource(t *testing.T) {
	for name, change := range map[string]func(*envelopepb.Envelope){
		"command":         func(e *envelopepb.Envelope) { e.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND },
		"wrong subject":   func(e *envelopepb.Envelope) { e.EventType = "accounting.balance.reconciled" },
		"wrong domain":    func(e *envelopepb.Envelope) { e.Domain = "order" },
		"wrong schema":    func(e *envelopepb.Envelope) { e.PayloadSchemaRef = "unknown" },
		"wrong version":   func(e *envelopepb.Envelope) { e.SchemaVersion = 2 },
		"wrong partition": func(e *envelopepb.Envelope) { e.PartitionKey = "other-fund" },
		"missing time":    func(e *envelopepb.Envelope) { e.EventTime = nil },
		"different time":  func(e *envelopepb.Envelope) { e.EventTime.Seconds++ },
		"degraded": func(e *envelopepb.Envelope) {
			e.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED}
		},
		"synthetic": func(e *envelopepb.Envelope) {
			e.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_SYNTHETIC}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := database(t)
			ctx := bus.WithTenantID(context.Background(), "tenant-a")
			env := cashEnvelope()
			change(env)
			if err := ApplyEnvelope(ctx, p, "tenant-a", env, sourcePayload(t, nil)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("contradictory envelope: %v", err)
			}
			if err := ApplyEnvelope(ctx, p, "tenant-a", cashEnvelope(), sourcePayload(t, nil)); err != nil {
				t.Fatal(err)
			}
			var latched bool
			if err := p.QueryRow(ctx, "SELECT fault_digest IS NOT NULL FROM capital_source_health").Scan(&latched); err != nil || !latched {
				t.Fatalf("later valid event cleared fault: %v %v", latched, err)
			}
		})
	}
}

func TestCashDeliveryReplaysEveryRevisionAndRetainsGapAcrossRestart(t *testing.T) {
	p := database(t)
	ctx := bus.WithTenantID(context.Background(), "tenant-a")
	payload := func(revision int64) []byte {
		return sourcePayload(t, func(m *accountingpb.PortfolioCashBalance) { m.CashCommit.Revision = revision })
	}
	if err := ApplyEnvelope(ctx, p, "tenant-a", cashEnvelope(), payload(3)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("snapshot boot accepted: %v", err)
	}
	restarted := tenantPool(t, "tenant-a")
	for _, rev := range []int64{1, 1, 2, 3, 2} {
		env := proto.Clone(cashEnvelope()).(*envelopepb.Envelope)
		env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
		if err := ApplyEnvelope(ctx, restarted, "tenant-a", env, payload(rev)); err != nil {
			t.Fatalf("replay %d: %v", rev, err)
		}
	}
	var revision, gap, receipts int
	if err := restarted.QueryRow(ctx, "SELECT revision,gap_revision FROM capital_balances").Scan(&revision, &gap); err != nil {
		t.Fatal(err)
	}
	if err := restarted.QueryRow(ctx, "SELECT count(*) FROM capital_cash_events").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || gap > revision || receipts != 3 {
		t.Fatalf("replay not contiguous/idempotent: revision=%d gap=%d receipts=%d", revision, gap, receipts)
	}
}
