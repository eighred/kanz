package monitor

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/platform/subject"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func positionIdentityMandate(t *testing.T, reg *comp.MandateRegistry, tenant, portfolio string) {
	t.Helper()
	if err := reg.Put(&compliancepb.Mandate{MandateId: "identity-" + portfolio, TenantId: tenant, PortfolioId: portfolio, Version: 7,
		EffectiveAt: timestamppb.New(t0), Rules: []*compliancepb.Rule{{RuleId: "AAPL-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT, Bucket: "AAPL", MaxWeight: decv(60, -2)}}}}}); err != nil {
		t.Fatal(err)
	}
}

func positionIdentityMonitor(t *testing.T, tenant, portfolio string) (*Monitor, *fakeBus, *recordingRecorder) {
	t.Helper()
	reg := comp.NewMandateRegistry()
	positionIdentityMandate(t, reg, tenant, portfolio)
	fb, rec := &fakeBus{}, &recordingRecorder{}
	m := NewMonitor(nil, reg, nil, NewEmitter(fb), rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = clockAt(t0.Add(24 * time.Hour))
	return m, fb, rec
}

func positionIdentityFeed(t *testing.T, m *Monitor, tenant, portfolio, instrument string, qty, value int64, key string) error {
	t.Helper()
	return m.Handle(context.Background(), &envelopepb.Envelope{TenantId: tenant, PartitionKey: key},
		positionTimePayload(t, portfolio, instrument, qty, value, timestamppb.New(t0.Add(time.Hour))))
}

func positionIdentitySeed(t *testing.T, m *Monitor, tenant, portfolio, other string) {
	t.Helper()
	for _, instrument := range []string{"AAPL", other} {
		if err := positionIdentityFeed(t, m, tenant, portfolio, instrument, 1, 100, subject.PositionFor(tenant, portfolio, instrument)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPositionIdentityUnknownCannotDiluteNamedCap(t *testing.T) {
	for _, keyKind := range []string{"canonical", "legacy"} {
		for _, prior := range []bool{false, true} {
			name := "cold"
			if prior {
				name = "prior"
			}
			t.Run(keyKind+"/"+name, func(t *testing.T) {
				m, fb, rec := positionIdentityMonitor(t, "tenant", "portfolio")
				if prior {
					positionIdentitySeed(t, m, "tenant", "portfolio", "MSFT")
				}
				fb.events, rec.records = nil, nil
				key := "portfolio"
				if keyKind == "canonical" {
					key = subject.PositionFor("tenant", "portfolio", "MSFT")
				}
				// Otherwise valid USD900 cannot dilute the named AAPL cap. Merely
				// retaining the previous healthy book is also an unknown-as-known error.
				if err := positionIdentityFeed(t, m, "tenant", "portfolio", "", 9, 900, key); err == nil {
					t.Fatal("unidentified holding was acknowledged as usable state")
				}
				if err := positionIdentityFeed(t, m, "tenant", "portfolio", "AAPL", 1, 100, subject.PositionFor("tenant", "portfolio", "AAPL")); err == nil {
					t.Fatal("unrelated instrument repaired an unidentified holding")
				}
				if err := m.Reevaluate(context.Background(), "tenant", "portfolio"); err == nil {
					t.Fatal("unknown book was measured during re-evaluation")
				}
				state := positionTimeBooks(m)[bookKey{"tenant", "portfolio"}]
				anonymous := state.positions[""]
				if anonymous.MarketValue != nil || anonymous.Quantity != nil || state.lastStatus == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
					t.Fatal("anonymous money entered the book or the unknown book retained PASS")
				}
				if len(fb.events)+len(rec.records) != 0 {
					t.Fatal("unknown book emitted a measured decision or breach")
				}
			})
		}
	}
}

func TestPositionIdentityAnonymousMoneyNeverBecomesMeasured(t *testing.T) {
	m, _, _ := positionIdentityMonitor(t, "tenant", "portfolio")
	bad := positionIdentityFeed(t, m, "tenant", "portfolio", "", 9, 900, "portfolio")
	update := positionIdentityFeed(t, m, "tenant", "portfolio", "AAPL", 1, 100, "portfolio")
	state := positionTimeBooks(m)[bookKey{"tenant", "portfolio"}]
	if state.lastStatus == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatal("anonymous USD900 diluted AAPL100 to a measured 10% under its named 60% cap")
	}
	if bad == nil || update == nil {
		t.Fatal("unidentified state did not remain explicitly unreadable")
	}
}

func TestPositionIdentityUnknownRefusesEvenAnUnconstrainedMandate(t *testing.T) {
	m, fb, rec := positionIdentityMonitor(t, "tenant", "portfolio")
	reg := comp.NewMandateRegistry()
	if err := reg.Put(&compliancepb.Mandate{MandateId: "unconstrained", TenantId: "tenant", PortfolioId: "portfolio", Version: 1, EffectiveAt: timestamppb.New(t0)}); err != nil {
		t.Fatal(err)
	}
	m.mandates = reg
	if err := positionIdentityFeed(t, m, "tenant", "portfolio", "", 9, 900, "portfolio"); err == nil {
		t.Fatal("unknown identity was acknowledged under an unconstrained mandate")
	}
	if err := positionIdentityFeed(t, m, "tenant", "portfolio", "AAPL", 1, 100, "portfolio"); err == nil {
		t.Fatal("zero rules turned unreadable identity into a successful check")
	}
	if m.Reevaluate(context.Background(), "tenant", "portfolio") == nil || len(fb.events)+len(rec.records) != 0 {
		t.Fatal("an unconstrained policy measured an unidentified book")
	}
}

func TestPositionIdentityCanonicalRepairIsScoped(t *testing.T) {
	for _, tc := range []struct{ name, tenant, portfolio, instrument string }{
		{"plain", "tenant", "portfolio", "MSFT"},
		{"escaped", "tenant.one", "portfolio * one", "MSFT.α"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fb, rec := positionIdentityMonitor(t, tc.tenant, tc.portfolio)
			registry := m.mandates.(*comp.MandateRegistry)
			positionIdentityMandate(t, registry, "another-tenant", tc.portfolio)
			positionIdentityMandate(t, registry, tc.tenant, "another-portfolio")
			positionIdentitySeed(t, m, tc.tenant, tc.portfolio, tc.instrument)
			positionIdentitySeed(t, m, "another-tenant", tc.portfolio, tc.instrument)
			positionIdentitySeed(t, m, tc.tenant, "another-portfolio", tc.instrument)
			before := positionTimeBooks(m)
			fb.events, rec.records = nil, nil
			key := subject.PositionFor(tc.tenant, tc.portfolio, tc.instrument)
			if err := positionIdentityFeed(t, m, tc.tenant, tc.portfolio, "", 9, 900, key); err == nil {
				t.Fatal("canonical unidentified slot was acknowledged")
			}
			if err := positionIdentityFeed(t, m, tc.tenant, tc.portfolio, "AAPL", 1, 100, subject.PositionFor(tc.tenant, tc.portfolio, "AAPL")); err == nil {
				t.Fatal("another instrument cleared the poisoned slot")
			}
			after := positionTimeBooks(m)
			for _, bystander := range []bookKey{{"another-tenant", tc.portfolio}, {tc.tenant, "another-portfolio"}} {
				if before[bystander].lastStatus != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS || !reflect.DeepEqual(before[bystander], after[bystander]) || m.Reevaluate(context.Background(), bystander.tenant, bystander.portfolio) != nil {
					t.Fatal("poison changed or made a bystander's book unreadable")
				}
			}
			if err := positionIdentityFeed(t, m, tc.tenant, tc.portfolio, tc.instrument, 1, 100, key); err != nil {
				t.Fatalf("matching repair failed: %v", err)
			}
			if err := m.Reevaluate(context.Background(), tc.tenant, tc.portfolio); err != nil {
				t.Fatal(err)
			}
			if len(fb.events)+len(rec.records) != 0 {
				t.Fatal("repaired 50/50 book changed the approved 60% policy")
			}
			if err := positionIdentityFeed(t, m, tc.tenant, tc.portfolio, "AAPL", 4, 400, subject.PositionFor(tc.tenant, tc.portfolio, "AAPL")); err != nil {
				t.Fatal(err)
			}
			if len(fb.events) != 1 || len(rec.records) != 1 || rec.records[0].TenantID != tc.tenant {
				t.Fatal("valid 80% concentration breach lost its tenant/decision")
			}
			result := rec.records[0].Result
			if result.GetPortfolioId() != tc.portfolio || result.GetMandateVersion() != 7 || !result.GetEvaluatedAt().AsTime().Equal(t0.Add(time.Hour)) {
				t.Fatal("repair changed mandate or observation attribution")
			}
		})
	}
}

func TestPositionIdentityMatchingFlatRepairsCanonicalSlot(t *testing.T) {
	m, fb, rec := positionIdentityMonitor(t, "tenant", "portfolio")
	positionIdentitySeed(t, m, "tenant", "portfolio", "MSFT")
	fb.events, rec.records = nil, nil
	key := subject.PositionFor("tenant", "portfolio", "MSFT")
	if err := positionIdentityFeed(t, m, "tenant", "portfolio", "", 9, 900, key); err == nil {
		t.Fatal("unidentified slot was acknowledged")
	}
	if err := positionIdentityFeed(t, m, "tenant", "portfolio", "MSFT", 0, 0, key); err != nil {
		t.Fatal("explicit matching zero did not repair its slot", err)
	}
	state := positionTimeBooks(m)[bookKey{"tenant", "portfolio"}]
	if _, retained := state.positions["MSFT"]; retained || len(fb.events) != 1 || len(rec.records) != 1 || state.lastStatus != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatal("matching flat did not remove only its slot and expose the real AAPL100% breach")
	}
	if err := positionIdentityFeed(t, m, "tenant", "portfolio", "AAPL", 0, 0, subject.PositionFor("tenant", "portfolio", "AAPL")); err != nil {
		t.Fatal(err)
	}
	if state := positionTimeBooks(m)[bookKey{"tenant", "portfolio"}]; len(state.positions) != 0 || state.emptiedAt.IsZero() || len(fb.events) != 1 {
		t.Fatal("valid final flat lost the existing idle-book lifetime")
	}
}

func TestPositionIdentityConflictsCannotPoisonVictims(t *testing.T) {
	for _, tc := range []struct{ name, tenant, portfolio, instrument, key string }{
		{"key-tenant", "tenant", "portfolio", "", subject.PositionFor("foreign", "portfolio", "MSFT")},
		{"envelope-tenant", "foreign", "portfolio", "", subject.PositionFor("tenant", "portfolio", "MSFT")},
		{"key-portfolio", "tenant", "portfolio", "", subject.PositionFor("tenant", "foreign", "MSFT")},
		{"payload-instrument", "tenant", "portfolio", "AAPL", subject.PositionFor("tenant", "portfolio", "MSFT")},
		{"truncated-key", "tenant", "portfolio", "", subject.PositionChanged + ".tenant.portfolio"},
		{"bad-escape", "tenant", "portfolio", "", subject.PositionChanged + ".tenant.portfolio.M%xx"},
		{"noncanonical-escape", "tenant", "portfolio", "", subject.PositionChanged + ".tenant.portfolio.%4DSFT"},
		{"empty-key-instrument", "tenant", "portfolio", "", subject.PositionFor("tenant", "portfolio", "")},
		{"foreign-legacy-key", "tenant", "portfolio", "", "foreign"},
		{"missing-envelope-tenant", "", "portfolio", "", "portfolio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fb, rec := positionIdentityMonitor(t, "tenant", "portfolio")
			positionIdentitySeed(t, m, "tenant", "portfolio", "MSFT")
			positionIdentitySeed(t, m, "foreign", "portfolio", "MSFT")
			before, gc := positionTimeBooks(m), m.lastBookGC
			fb.events, rec.records = nil, nil
			if err := positionIdentityFeed(t, m, tc.tenant, tc.portfolio, tc.instrument, 9, 900, tc.key); err == nil {
				t.Fatal("conflicting/unattributable identity was acknowledged")
			}
			if !reflect.DeepEqual(before, positionTimeBooks(m)) || !gc.Equal(m.lastBookGC) || len(fb.events)+len(rec.records) != 0 {
				t.Fatal("conflicting identity changed a victim, GC, decision or breach")
			}
		})
	}
}

func TestPositionIdentityLegacyPoisonSurvivesUnrelatedRepairAndGC(t *testing.T) {
	for _, key := range []string{"", "portfolio"} {
		name := "absent-key"
		if key != "" {
			name = "portfolio-key"
		}
		t.Run(name, func(t *testing.T) {
			m, _, _ := positionIdentityMonitor(t, "tenant", "portfolio")
			positionIdentitySeed(t, m, "tenant", "portfolio", "MSFT")
			if err := positionIdentityFeed(t, m, "tenant", "portfolio", "", 9, 900, key); err == nil {
				t.Fatal("unidentified legacy holding was acknowledged")
			}
			for _, instrument := range []string{"AAPL", "MSFT"} {
				if err := positionIdentityFeed(t, m, "tenant", "portfolio", instrument, 0, 0, subject.PositionFor("tenant", "portfolio", instrument)); err == nil {
					t.Fatal("closing an unrelated known holding cleared legacy uncertainty")
				}
			}
			m.now = clockAt(t0.Add(48 * time.Hour))
			if err := positionIdentityFeed(t, m, "tenant", "another-portfolio", "AAPL", 1, 100, "another-portfolio"); err != nil {
				t.Fatal(err)
			}
			if _, ok := positionTimeBooks(m)[bookKey{"tenant", "portfolio"}]; !ok || m.ReevaluateAll(context.Background()) == nil {
				t.Fatal("GC or the sweep silently forgot the legacy unknown book")
			}
			// Only a new complete retained fold can discard an unidentified slot.
			rebuilt, _, _ := positionIdentityMonitor(t, "tenant", "portfolio")
			positionIdentitySeed(t, rebuilt, "tenant", "portfolio", "MSFT")
			if err := rebuilt.Reevaluate(context.Background(), "tenant", "portfolio"); err != nil {
				t.Fatal(err)
			}
			for _, instrument := range []string{"AAPL", "MSFT"} {
				if err := positionIdentityFeed(t, rebuilt, "tenant", "portfolio", instrument, 0, 0, subject.PositionFor("tenant", "portfolio", instrument)); err != nil {
					t.Fatal(err)
				}
			}
			if got := positionTimeBooks(rebuilt)[bookKey{"tenant", "portfolio"}]; len(got.positions) != 0 || got.emptiedAt.IsZero() {
				t.Fatal("genuine identified flat positions stopped leaving the book")
			}
		})
	}
}

type positionIdentityPausedMandate struct {
	comp.MandateSource
	entered chan struct{}
	release chan struct{}
}

func (p positionIdentityPausedMandate) Mandate(ctx context.Context, tenant, portfolio string, at time.Time) (*compliancepb.Mandate, comp.Governance, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, comp.NeverMandated, ctx.Err()
	}
	return p.MandateSource.Mandate(ctx, tenant, portfolio, at)
}

func TestPositionIdentityInflightEvaluationCannotRestorePASS(t *testing.T) {
	m, fb, rec := positionIdentityMonitor(t, "tenant", "portfolio")
	positionIdentitySeed(t, m, "tenant", "portfolio", "MSFT")
	fb.events, rec.records = nil, nil
	pause := positionIdentityPausedMandate{m.mandates, make(chan struct{}), make(chan struct{})}
	m.mandates = pause
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Reevaluate(ctx, "tenant", "portfolio") }()
	select {
	case <-pause.entered:
	case <-ctx.Done():
		t.Fatal("old measured snapshot did not reach mandate lookup")
	}
	poison := positionIdentityFeed(t, m, "tenant", "portfolio", "", 9, 900, subject.PositionFor("tenant", "portfolio", "MSFT"))
	close(pause.release)
	if poison == nil {
		t.Error("unidentified holding was acknowledged")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("inflight measured evaluation hid the newer unreadable identity")
		}
	case <-ctx.Done():
		t.Fatal("inflight evaluation did not return")
	}
	if state := positionTimeBooks(m)[bookKey{"tenant", "portfolio"}]; state.lastStatus == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS || len(fb.events)+len(rec.records) != 0 {
		t.Fatal("old evaluation restored PASS or emitted a measured result over newer UNKNOWN")
	}
}
