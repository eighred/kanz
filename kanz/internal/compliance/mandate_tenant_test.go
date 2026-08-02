package compliance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
)

// forTenant builds a mandate for (tenant, portfolio) whose single concentration
// cap is maxPct — the cap is the observable that says WHOSE mandate resolved.
func forTenant(tenant, portfolio string, version uint64, maxPct int64) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId: "m-" + tenant, TenantId: tenant, PortfolioId: portfolio, Version: version,
		EffectiveAt: timestamppb.New(t0),
		Rules: []*compliancepb.Rule{{
			RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: dec(maxPct, -2),
			}},
		}},
	}
}

func capOf(t *testing.T, m *compliancepb.Mandate) int64 {
	t.Helper()
	if len(m.GetRules()) != 1 {
		t.Fatalf("expected one rule, got %d", len(m.GetRules()))
	}
	return m.GetRules()[0].GetConcentration().GetMaxWeight().GetCoefficient()
}

// TWO TENANTS, ONE PORTFOLIO NAME — the whole of #243.
//
// portfolio_id is a caller-chosen string off SubmitOrder, so "growth" belonging
// to two funds needs no malice. Keyed by portfolio alone the registry put both
// in one bucket sorted by (effective_at, version) and the last effective one
// governed BOTH, so tenant beta's order was checked against tenant acme's
// concentration cap and instrument allow-list.
func TestTwoTenantsSharingAPortfolioNameGetTheirOwnMandate(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, forTenant("acme", "growth", 1, 60))
	mustPut(t, reg, forTenant("beta", "growth", 2, 10)) // later version: used to win for both

	for _, tc := range []struct {
		tenant  string
		wantCap int64
	}{
		{"acme", 60},
		{"beta", 10},
	} {
		m, ok, err := reg.Mandate(context.Background(), tc.tenant, "growth", t0.Add(time.Hour))
		if err != nil || !ok {
			t.Fatalf("%s/growth did not resolve: ok=%v err=%v", tc.tenant, ok, err)
		}
		if got := capOf(t, m); got != tc.wantCap {
			t.Fatalf("tenant %s resolved a concentration cap of %d%%, want %d%% — it is being "+
				"governed by the OTHER tenant's mandate (#243)", tc.tenant, got, tc.wantCap)
		}
	}
}

// A VERSION-NUMBER COLLISION USED TO DELETE THE OTHER TENANT'S MANDATE.
//
// Put's idempotent-replace branch matched on version alone inside a
// portfolio-keyed bucket, so tenant beta publishing its v1 for "growth"
// overwrote tenant acme's v1 outright. acme was then UNGOVERNED — and with
// OMS_REQUIRE_MANDATE=false that is admitted unconstrained, not refused.
func TestAVersionCollisionDoesNotOverwriteTheOtherTenantsMandate(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, forTenant("acme", "growth", 1, 60))
	mustPut(t, reg, forTenant("beta", "growth", 1, 10)) // SAME version number

	m, ok, err := reg.Mandate(context.Background(), "acme", "growth", t0.Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("acme's mandate was ERASED by another tenant publishing the same version: ok=%v err=%v", ok, err)
	}
	if got := capOf(t, m); got != 60 {
		t.Fatalf("acme resolved cap %d%%, want 60%% — beta's v1 replaced it", got)
	}
}

// A mandate that names no tenant cannot be filed, and MUST NOT be dropped
// quietly: silence here surfaces downstream as "the portfolio is ungoverned",
// which is admitted-unconstrained rather than refused. The refusal has to reach
// the consumer, which logs it against the config key.
func TestPutRefusesAMandateWithNoTenantAndTheLoaderPropagatesIt(t *testing.T) {
	reg := NewMandateRegistry()
	untenanted := &compliancepb.Mandate{
		MandateId: "m1", PortfolioId: "growth", Version: 1, EffectiveAt: timestamppb.New(t0),
	}
	err := reg.Put(untenanted)
	if err == nil {
		t.Fatal("the registry ACCEPTED a mandate with no tenant_id — it cannot be keyed by " +
			"(tenant, portfolio), so it was silently dropped and the portfolio reads UNGOVERNED")
	}
	if !strings.Contains(err.Error(), "tenant_id") {
		t.Fatalf("the refusal does not name the missing field: %v", err)
	}

	val, mErr := MarshalMandateValue(untenanted)
	if mErr != nil {
		t.Fatal(mErr)
	}
	if _, err := NewMandateLoader(reg).Apply(&lifecyclepb.ConfigChanged{
		ConfigKey: MandateConfigKeyPrefix + "unknown/growth", NewValue: val,
	}); err == nil {
		t.Fatal("MandateLoader.Apply swallowed the registry's refusal — the mandate consumer " +
			"acks it as applied and nothing ever says the portfolio lost its mandate")
	}
}

// THE SHARED-BUCKET BRANCH, which is what keeps today's deployment working.
//
// The OMS position projector stamps OMS_TENANT="__system__" on every position
// FACT, so the post-trade monitor's every mandate lookup arrives as the shared
// bucket while mandates are published under a customer's tenant. One governing
// tenant ⇒ unambiguous ⇒ resolved, exactly as before this change. This branch is
// the reason the fix does not disarm the monitor on the estate that exists.
func TestTheSharedSystemBucketStillResolvesASingleTenantsMandate(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, forTenant("acme", "growth", 1, 60))

	m, ok, err := reg.Mandate(context.Background(), bus.SystemTenant, "growth", t0.Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("a __system__ lookup stopped resolving the only mandate there is: ok=%v err=%v", ok, err)
	}
	if got := capOf(t, m); got != 60 {
		t.Fatalf("resolved cap %d%%, want 60%%", got)
	}
}

// …AND IT REFUSES RATHER THAN GUESSES the moment there are two.
//
// This is the branch that cannot fire on a single-tenant estate and becomes
// load-bearing the day #97 provisions a second one. Picking one of the two is
// precisely the defect; the answer is a terminal refusal, not a coin toss.
func TestTheSharedSystemBucketRefusesWhenTwoTenantsShareAPortfolioName(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, forTenant("acme", "growth", 1, 60))
	mustPut(t, reg, forTenant("beta", "growth", 1, 10))

	_, ok, err := reg.Mandate(context.Background(), bus.SystemTenant, "growth", t0.Add(time.Hour))
	if ok {
		t.Fatal("an ambiguous __system__ lookup RESOLVED — it picked one tenant's mandate to " +
			"govern the other's book, which is #243 with extra steps")
	}
	if !errors.Is(err, ErrMandateTenantUnresolved) {
		t.Fatalf("want ErrMandateTenantUnresolved, got %v", err)
	}
	for _, want := range []string{"acme", "beta", "growth"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not name %q, so an operator cannot act on it: %v", want, err)
		}
	}
}

// A real tenant NEVER falls back to another tenant's mandate: it reads as
// ungoverned, which is the truth. It is not an error, because turning "this
// portfolio's mandate is filed under the wrong tenant" into a hard refusal would
// take a working deployment down — the same reasoning that keeps
// WithRequireMandate off by default. The loudness lives in the log.
func TestARealTenantDoesNotInheritAnotherTenantsMandate(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, forTenant(bus.SystemTenant, "growth", 1, 60))

	m, ok, err := reg.Mandate(context.Background(), "acme", "growth", t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("a miss for a real tenant is not an error: %v", err)
	}
	if ok {
		t.Fatalf("tenant acme was handed a mandate filed under %q (cap %d%%) — a mandate is "+
			"not transferable between tenants (#243)", bus.SystemTenant, capOf(t, m))
	}
	if got := reg.TenantsGoverning("growth"); len(got) != 1 || got[0] != bus.SystemTenant {
		t.Fatalf("TenantsGoverning cannot name who holds the mandate, so the warning cannot either: %v", got)
	}
}

// An untenanted lookup is a caller that lost the envelope's tenant. It resolves
// to nothing and says so terminally — the one thing it must never do is answer
// with somebody's mandate.
func TestAnUntenantedLookupIsRefusedRatherThanGuessed(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, forTenant("acme", "growth", 1, 60))

	_, ok, err := reg.Mandate(context.Background(), "", "growth", t0.Add(time.Hour))
	if ok {
		t.Fatal("a lookup with NO tenant returned a mandate")
	}
	if !errors.Is(err, ErrMandateTenantUnresolved) {
		t.Fatalf("want ErrMandateTenantUnresolved, got %v", err)
	}
}

// THE GATE REFUSES AN UNRESOLVABLE TENANT EVEN WITH require_mandate OFF.
//
// Ungoverned is a policy question an operator may knowingly trade through.
// "Two tenants claim this portfolio name" is not: a mandate exists and the
// platform cannot tell whether it is this order's. And the refusal must be
// TERMINAL — returning the error instead would redeliver the command forever.
func TestTheGateRefusesAnUnresolvableTenantRegardlessOfPosture(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, forTenant("acme", "growth", 1, 60))
	mustPut(t, reg, forTenant("beta", "growth", 1, 10))

	g := NewPreTradeGate(NewEngine(nil), MapBookSource{}, reg, nil, nil, nil,
		WithRequireMandate(false)) // the SHIPPED posture: OMS_REQUIRE_MANDATE="false"

	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: bus.SystemTenant, PortfolioID: "growth", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: dec(1000, 0), Currency: "USD", AsOf: t0.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("a terminal ambiguity must not surface as a retryable error — the command would "+
			"redeliver forever: %v", err)
	}
	if got.Allowed {
		t.Fatal("the order was ADMITTED against a mandate the platform could not attribute")
	}
	if !got.Unscoped {
		t.Fatal("the refusal does not say WHY: nothing was breached, the tenant could not be resolved")
	}
	if got.Ungoverned {
		t.Fatal("reported as UNGOVERNED — that sends an operator to write a mandate that already exists")
	}
}
