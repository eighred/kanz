package sustainability

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"

	"github.com/kanz-eng/kanz/internal/compliance"
)

func money(amount int64) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: amount, Exponent: 0}, CurrencyCode: "USD"}
}

func bookWith(instruments ...string) *compliance.Book {
	b := &compliance.Book{PortfolioID: "p1", BaseCurrency: "USD"}
	for _, id := range instruments {
		b.Positions = append(b.Positions, compliance.Position{InstrumentID: id, MarketValue: money(100000)})
	}
	return b
}

func TestExclusionScreenSectorBreach(t *testing.T) {
	cl := compliance.StaticClassifier{
		"AAPL": {Sector: "TECH", Issuer: "APPLE"},
		"XOM":  {Sector: "ENERGY", Issuer: "EXXON"},
	}
	policy := ExclusionPolicy{ExcludedSectors: []string{"ENERGY"}}

	// Holding XOM (ENERGY) ⇒ breach.
	res := Screen(context.Background(), bookWith("XOM"), cl, policy, time.Now())
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("excluded sector held: want BREACH got %v", res.GetStatus())
	}
	// Holding only AAPL (TECH) ⇒ pass.
	res = Screen(context.Background(), bookWith("AAPL"), cl, policy, time.Now())
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("clean book: want PASS got %v", res.GetStatus())
	}
}

func TestExclusionScreenIssuerBreach(t *testing.T) {
	cl := compliance.StaticClassifier{"XOM": {Sector: "ENERGY", Issuer: "EXXON"}}
	policy := ExclusionPolicy{ExcludedIssuers: []string{"EXXON"}}
	res := Screen(context.Background(), bookWith("XOM"), cl, policy, time.Now())
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("excluded issuer held: want BREACH got %v", res.GetStatus())
	}
}

func TestEmptyPolicyPasses(t *testing.T) {
	cl := compliance.StaticClassifier{"XOM": {Sector: "ENERGY"}}
	if !(ExclusionPolicy{}).Empty() {
		t.Fatal("empty policy should report Empty")
	}
	res := Screen(context.Background(), bookWith("XOM"), cl, ExclusionPolicy{}, time.Now())
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("empty policy: want PASS got %v", res.GetStatus())
	}
}
