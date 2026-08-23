package refdata

import (
	"context"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/compliance"
)

// A SECTOR AND AN ISSUER MANDATE CAN NOW FIRE (#640).
//
// internal/compliance/classifier_absent_test.go pins the OTHER half of this
// issue: with no classifier, a 100%-technology book must not PASS a 10%
// technology cap. That half stopped a control lying. It did not make the
// control work — the answer was still a refusal, and a mandate that can only
// ever be refused is a mandate nobody can run a fund under.
//
// These tests run the same rules through the SAME production engine with a
// refdata-backed classifier and assert the thing that was never true on this
// estate: the cap BREACHES when the book breaches it, PASSES when it does not,
// and the difference comes from real reference data rather than from a map a
// test wrote.
//
// THEY LIVE HERE AND NOT IN internal/compliance because that package cannot
// import this one — refdata depends on compliance for Attributes, so the arrow
// only goes one way. This is the package under test in any case: the question
// is whether the projection feeds the engine correctly, not whether the engine
// works.

var evalTime = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func d(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func usd(coeff int64, exp int32) *commonpb.Money {
	return &commonpb.Money{Amount: d(coeff, exp), CurrencyCode: "USD"}
}

func holding(instrumentID string, marketValue int64) compliance.Position {
	return compliance.Position{InstrumentID: instrumentID, Quantity: d(1, 0), MarketValue: usd(marketValue, 0)}
}

// classifierOver builds the production shape — a cache filled from a Source,
// projected as a compliance.Classifier — holding exactly these records.
func classifierOver(t *testing.T, recs map[string]Record) compliance.Classifier {
	t.Helper()
	src := newFakeSource()
	for id, rec := range recs {
		src.put(id, rec)
	}
	// The real clock: these assertions are about classification, not staleness,
	// and the defaults must be the configuration under test.
	c, err := NewCache(src, Options{})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	for id := range recs {
		c.Lookup(id, time.Time{}) // want it
	}
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return c.Compliance()
}

func evaluate(t *testing.T, book *compliance.Book, cl compliance.Classifier, rule *compliancepb.Rule) *compliancepb.ComplianceResult {
	t.Helper()
	return compliance.NewEngine(nil).Evaluate(
		context.Background(),
		&compliance.Candidate{Book: book, Classifier: cl, AsOf: evalTime},
		&compliancepb.Mandate{
			MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
			Rules: []*compliancepb.Rule{rule}, EffectiveAt: timestamppb.New(evalTime),
		},
	)
}

// techCap is the mandate from the issue: "no more than 10% in TECH", where TECH
// is the GICS Information Technology bucket the master actually resolves.
func techCap(bucket string) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "sector-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			Bucket:    bucket,
			MaxWeight: d(10, -2),
		}},
	}
}

const (
	gicsTech    = "GICS:45"
	gicsStaples = "GICS:30"
)

func techAndStaples() map[string]Record {
	return map[string]Record{
		"AAPL": {AssetClass: "EQUITY", Sector: Sector{Taxonomy: "GICS", Code: "45"}, IssuerID: "LEI-APPLE"},
		"MSFT": {AssetClass: "EQUITY", Sector: Sector{Taxonomy: "GICS", Code: "45"}, IssuerID: "LEI-MSFT"},
		"KO":   {AssetClass: "EQUITY", Sector: Sector{Taxonomy: "GICS", Code: "30"}, IssuerID: "LEI-COCACOLA"},
	}
}

// THE HEADLINE. A book that is entirely technology breaches a 10% technology
// cap — the outcome the issue's title says was impossible on this estate.
func TestASectorCapBreachesTheBookItWasWrittenFor(t *testing.T) {
	book := &compliance.Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: usd(200_000, 0),
		Positions: []compliance.Position{holding("AAPL", 100_000), holding("MSFT", 100_000)},
	}
	res := evaluate(t, book, classifierOver(t, techAndStaples()), techCap(gicsTech))

	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("a 100%%-technology book returned %v against a 10%% technology cap, want BREACH: %v",
			res.GetStatus(), res)
	}
	if len(res.GetViolations()) != 1 {
		t.Fatalf("violations = %v, want exactly one", res.GetViolations())
	}
	v := res.GetViolations()[0]
	// THE VIOLATION MUST BE THE LIMIT, NOT THE MISSING INPUT. "cannot be
	// verified" is the refusal this issue's first half installed; reading it here
	// would mean the classifier is wired and still not resolving, which looks
	// like a working control and is not one.
	if strings.Contains(v.GetMessage(), "cannot be verified") {
		t.Fatalf("the cap reported itself unevaluable with a classifier wired: %v", v)
	}
	if got := v.GetEvidence()["bucket"]; got != gicsTech {
		t.Errorf("evidence[bucket] = %q, want %q — the breach must name the sector it is about",
			got, gicsTech)
	}
}

// The other direction, which is the one that proves the control DISCRIMINATES.
// A test that only ever breaches passes just as well against a rule that always
// breaches.
func TestASectorCapPassesABookInsideIt(t *testing.T) {
	book := &compliance.Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: usd(200_000, 0),
		Positions: []compliance.Position{holding("AAPL", 10_000), holding("KO", 190_000)},
	}
	res := evaluate(t, book, classifierOver(t, techAndStaples()), techCap(gicsTech))

	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a 5%%-technology book returned %v against a 10%% technology cap, want PASS: %v",
			res.GetStatus(), res)
	}
}

// The bucket the mandate names must be the bucket the master resolves. A
// mandate written against a taxonomy the reference data does not use resolves
// every holding into a DIFFERENT bucket, so the named one is genuinely not
// held — and "not held" is a pass, correctly, because the fund holds none of
// it. This pins that the pass is reached with every holding RESOLVED, which is
// what makes it a real pass rather than the silent one #640 was about.
func TestACapOnASectorTheFundDoesNotHoldPassesWithEveryHoldingResolved(t *testing.T) {
	book := &compliance.Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: usd(200_000, 0),
		Positions: []compliance.Position{holding("AAPL", 100_000), holding("MSFT", 100_000)},
	}
	res := evaluate(t, book, classifierOver(t, techAndStaples()), techCap(gicsStaples))

	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("status = %v, want PASS — the fund holds no consumer staples: %v", res.GetStatus(), res)
	}
}

// A HOLDING THE MASTER DOES NOT RESOLVE STILL REFUSES, with a classifier
// wired. This is the second refusal shape compliance.unresolvedDimension
// distinguishes: not "nothing is wired" but "this reference data is missing",
// and it must name the instrument so somebody can go and load it.
func TestAnUnresolvedHoldingRefusesAndNamesItself(t *testing.T) {
	book := &compliance.Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: usd(200_000, 0),
		Positions: []compliance.Position{holding("AAPL", 100_000), holding("PRIVATE-CO", 100_000)},
	}
	res := evaluate(t, book, classifierOver(t, techAndStaples()), techCap(gicsTech))

	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a book holding an unclassified name PASSED a sector cap: %v", res)
	}
	v := res.GetViolations()[0]
	if got := v.GetEvidence()["classifier"]; got != "present" {
		t.Errorf("evidence[classifier] = %q, want \"present\" — a wired classifier that does not "+
			"know an instrument is a REFERENCE-DATA GAP, and an operator sent to the "+
			"composition root instead would be sent to the wrong place", got)
	}
	if got := v.GetEvidence()["unresolved_instruments"]; !strings.Contains(got, "PRIVATE-CO") {
		t.Errorf("evidence[unresolved_instruments] = %q, want it to name PRIVATE-CO", got)
	}
}

// An issuer exclusion — the dimension that had NO source at all before this
// change, because datamaster's golden record carried no issuer column.
func TestAnIssuerExclusionFiresOnTheIssuerTheMasterResolves(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "no-apple", Type: compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
		Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
			IssuerIds: []string{"LEI-APPLE"},
		}},
	}
	cl := classifierOver(t, techAndStaples())

	held := &compliance.Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: usd(200_000, 0),
		Positions: []compliance.Position{holding("AAPL", 100_000), holding("KO", 100_000)},
	}
	if got := evaluate(t, held, cl, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("status = %v holding an excluded issuer, want BREACH — before #640 no issuer was "+
			"resolvable anywhere on this estate, so this rule could never fire", got)
	}

	clean := &compliance.Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: usd(200_000, 0),
		Positions: []compliance.Position{holding("MSFT", 100_000), holding("KO", 100_000)},
	}
	if got := evaluate(t, clean, cl, rule).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("status = %v holding no excluded issuer, want PASS", got)
	}
}
