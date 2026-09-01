package compliance

import (
	"context"
	"strings"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// The three instants this file is about, deliberately far apart so no assertion
// can pass by rounding.
//
//	lastFill    — the as_of of the observation under evaluation: the position
//	              FACT the post-trade monitor replays at boot, whose as_of is as
//	              old as the portfolio's last trade.
//	recordAsOf  — the reference record's OWN as_of, a month later, because the
//	              security master kept refreshing while the fund sat still.
//	clockNow    — what the evaluating process's clock says.
var (
	lastFill   = t0
	recordAsOf = t0.Add(30 * 24 * time.Hour)
	clockNow   = recordAsOf.Add(72 * time.Hour)
)

// snapshotClassifier reproduces refdata.Cache.Lookup's as-of rule, which is the
// half of that type this file is about: the master holds ONE snapshot per
// instrument, so a question dated BEFORE the snapshot's own as_of cannot be
// answered from it and is REFUSED rather than answered with today's sector.
//
// It also RECORDS the instant it was asked at, because "the classifier resolved"
// and "the classifier was asked at the right time" are two claims and only one
// of them is visible in a rule's verdict.
type snapshotClassifier struct {
	recordAsOf time.Time
	attrs      map[string]Attributes
	askedAt    []time.Time
}

func (c *snapshotClassifier) Classify(_ context.Context, instrumentID string, asOf time.Time) (Attributes, bool) {
	c.askedAt = append(c.askedAt, asOf)
	a, ok := c.attrs[instrumentID]
	if !ok {
		return Attributes{}, false
	}
	// The refdata rule, verbatim in intent: a zero asOf means "now" and skips the
	// check; a record dated after the question is refused.
	if !asOf.IsZero() && !c.recordAsOf.IsZero() && c.recordAsOf.After(asOf) {
		return Attributes{}, false
	}
	return a, true
}

// twoSectorsClassifier resolves the two holdings of twoEqualBook into DIFFERENT
// sectors, so a 60% sector cap binds on neither of them.
func twoSectorsClassifier() *snapshotClassifier {
	return &snapshotClassifier{
		recordAsOf: recordAsOf,
		attrs: map[string]Attributes{
			"AAPL": {Issuer: "APPLE", Sector: "GICS:45", AssetClass: "EQUITY"},
			"MSFT": {Issuer: "MICROSOFT", Sector: "GICS:50", AssetClass: "EQUITY"},
		},
	}
}

func sectorCap(coeff int64, exp int32) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "sector-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
			MaxWeight: dec(coeff, exp),
		}},
	}
}

// TestTheClassifierIsAskedAtClassifyAsOfAndNotAtTheObservation pins #930.
//
// A candidate whose AsOf is an OBSERVATION — the position FACT's as_of on the
// post-trade monitor's boot replay — asked the classifier a BACKDATED question.
// refdata.Cache.Lookup refuses a record dated after the question, so every
// instrument whose reference record had been refreshed since that fill came back
// unresolved, and unresolvedDimension turned that into a violation. A 60% sector
// cap on a book that is 50/50 across two sectors reported a BREACH.
//
// The two fields answer two questions now: AsOf still stamps evaluated_at, and
// ClassifyAsOf says when the reference data is read.
func TestTheClassifierIsAskedAtClassifyAsOfAndNotAtTheObservation(t *testing.T) {
	cl := twoSectorsClassifier()
	res := NewEngine(nil).Evaluate(context.Background(), &Candidate{
		Book:         twoEqualBook(),
		Classifier:   cl,
		AsOf:         lastFill,
		ClassifyAsOf: clockNow,
	}, mandate(sectorCap(60, -2)))

	if got := res.GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("a book 50/50 across two sectors under a 60%% sector cap is %v, want PASS. "+
			"Violations: %s. The classifier was asked at %v; asking it at the observation's "+
			"as_of (%s) backdates the question past the reference record's own as_of (%s), "+
			"which is refused, and the refusal arrives as an unresolved-dimension breach",
			got, violationMessages(res), cl.askedAt, lastFill, recordAsOf)
	}
	for _, at := range cl.askedAt {
		if !at.Equal(clockNow) {
			t.Fatalf("the classifier was asked at %s, want ClassifyAsOf %s", at, clockNow)
		}
	}
	if len(cl.askedAt) == 0 {
		t.Fatal("the classifier was never asked at all, so this test proves nothing about when it is asked")
	}
	// THE ATTRIBUTION HALF, and it must NOT move (#917). Splitting the field is
	// only correct if evaluated_at still carries the observation: a result stamped
	// with the evaluating process's clock claims the fund's book was as observed on
	// the day the monitor happened to restart.
	if got := res.GetEvaluatedAt().AsTime(); !got.Equal(lastFill) {
		t.Fatalf("evaluated_at is %s, want the observation's as_of %s — ClassifyAsOf must give the "+
			"reference-data lookup its own clock WITHOUT moving the stamp", got, lastFill)
	}
}

// TestAZeroClassifyAsOfFallsBackToAsOfAndNeverToTheZeroTime is the fail-closed
// half of the split, and it is why the repair is a second field rather than
// "pass a zero as_of to the classifier".
//
// refdata.Cache.Lookup skips its as-of check ENTIRELY on a zero asOf. Manufacturing
// a zero here would have made every caller — the OMS pre-trade gate included —
// silently accept a reference record dated AFTER the question it asked, which
// trades this issue's false breach for a correctness hole on the capital path.
// So an unset ClassifyAsOf falls back to AsOf: exactly what this platform did
// before the split, which is the STRICTER direction. A construction site that
// never learns this field exists can only refuse, never admit.
func TestAZeroClassifyAsOfFallsBackToAsOfAndNeverToTheZeroTime(t *testing.T) {
	cl := twoSectorsClassifier()
	res := NewEngine(nil).Evaluate(context.Background(), &Candidate{
		Book:       twoEqualBook(),
		Classifier: cl,
		AsOf:       lastFill,
		// ClassifyAsOf deliberately unset.
	}, mandate(sectorCap(60, -2)))

	if len(cl.askedAt) == 0 {
		t.Fatal("the classifier was never asked, so nothing here is proven")
	}
	for _, at := range cl.askedAt {
		if at.IsZero() {
			t.Fatal("an unset ClassifyAsOf asked the classifier at the ZERO time. refdata.Cache.Lookup " +
				"skips its as-of check on a zero, so this would silently serve a reference record " +
				"dated after the question on every caller, including the OMS pre-trade gate")
		}
		if !at.Equal(lastFill) {
			t.Fatalf("an unset ClassifyAsOf asked the classifier at %s, want the fallback to AsOf %s", at, lastFill)
		}
	}
	// And the fallback keeps the OLD, stricter answer rather than quietly relaxing
	// it: a backdated question is still refused, and the refusal is still a breach.
	if got := res.GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("status is %v, want BREACH: with no ClassifyAsOf the question is backdated to AsOf, "+
			"the record is refused, and an unresolvable dimension must still refuse", got)
	}
}

// TestADimensionTheMasterCannotResolveStillBreachesAtNow is the control this
// change must NOT turn off.
//
// "The question was backdated" and "the dimension is dark" arrive at the rule
// engine identically — ok=false from the classifier — and #930 is about telling
// them apart. Moving the reference-data lookup to now separates them at the
// SOURCE: a record the master holds answers a current question, and a record it
// does not hold answers nothing whatever clock is used. An instrument the master
// genuinely cannot classify must still produce the unresolved-dimension refusal,
// because a sector cap that cannot see a sector has not been checked (#640).
func TestADimensionTheMasterCannotResolveStillBreachesAtNow(t *testing.T) {
	cl := twoSectorsClassifier()
	delete(cl.attrs, "MSFT") // the master does not hold this instrument at all

	res := NewEngine(nil).Evaluate(context.Background(), &Candidate{
		Book:         twoEqualBook(),
		Classifier:   cl,
		AsOf:         lastFill,
		ClassifyAsOf: clockNow,
	}, mandate(sectorCap(60, -2)))

	if got := res.GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("status is %v, want BREACH: a holding the security master cannot classify leaves the "+
			"SECTOR dimension dark, and a sector cap that cannot see a sector has not been checked", got)
	}
	var found *compliancepb.Violation
	for _, v := range res.GetViolations() {
		if strings.Contains(v.GetMessage(), "cannot be verified") {
			found = v
		}
	}
	if found == nil {
		t.Fatalf("no unresolved-dimension violation among %s", violationMessages(res))
	}
	if got := found.GetEvidence()["unresolved_instruments"]; got != "MSFT" {
		t.Fatalf("the refusal names unresolved_instruments=%q, want MSFT — the evidence is what sends "+
			"an operator to load the reference data that is actually missing", got)
	}
}

// violationMessages renders a result's violations for a failure message.
func violationMessages(res *compliancepb.ComplianceResult) string {
	msgs := make([]string, 0, len(res.GetViolations()))
	for _, v := range res.GetViolations() {
		msgs = append(msgs, v.GetMessage())
	}
	if len(msgs) == 0 {
		return "(none)"
	}
	return strings.Join(msgs, "; ")
}
