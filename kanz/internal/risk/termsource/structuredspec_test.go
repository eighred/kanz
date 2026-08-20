package termsource

import (
	"testing"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/risk/pricing/structured"
)

// THE STRUCTURED TERMS CONVERSION (#572).
//
// UNGATED, for the reason bondspec_test.go gives: the conversion is a pure
// function and it is where the plausible-wrong-answer failures live, so it runs
// on every machine rather than skipping wherever TEST_POSTGRES_URL is unset.
//
// MOST OF THIS FILE IS ABOUT WHAT IS REFUSED, and that is the point of the
// family. #585 made the pricer return ErrUnpriceable instead of a confident
// zero; that only holds if this layer stops handing it plausible rubbish, so a
// record that cannot be described is declined here and never becomes a spec.

func oas(v float64) *float64 { return &v }

// aDeal is a well-formed two-tranche USD MBS: a 1,000,000 pool at a 6% WAC over
// 30 years, an 800k senior A and a 200k B, projected at 150 PSA, with the A note
// held and marked at 125bp.
func aDeal() *referencepb.StructuredTerms {
	return &referencepb.StructuredTerms{
		DealId: "DEAL-2026-1",
		Pool: &referencepb.CollateralPool{
			OriginalBalance:  bdec(1_000_000, 0),
			GrossCoupon:      0.06,
			ServicingFee:     0.005,
			TermMonths:       360,
			PaymentFrequency: referencepb.PaymentFrequency_PAYMENT_FREQUENCY_MONTHLY,
		},
		Tranches: []*referencepb.Tranche{
			{Name: "A", OriginalBalance: bdec(800_000, 0), Coupon: 0.04, Seniority: 0, Attachment: 0.2, Detachment: 1},
			{Name: "B", OriginalBalance: bdec(200_000, 0), Coupon: 0.06, Seniority: 1, Attachment: 0, Detachment: 0.2},
		},
		BaseAssumption: &referencepb.PrepaymentAssumption{
			Model: "PSA", PsaMultiple: 1.5, Cdr: 0.01, Severity: 0.35,
		},
		HeldTranche:  "A",
		CurrencyCode: "USD",
		QuotedOas:    oas(0.0125),
	}
}

// A COMPLETE DEAL MAPS COMPLETELY.
func TestToStructuredSpec_MapsEveryField(t *testing.T) {
	spec, ok := toStructuredSpec(aDeal())
	if !ok {
		t.Fatal("a well-formed deal did not convert — every structured measure would exclude it")
	}
	if spec.Deal.Pool.Balance != 1_000_000 {
		t.Errorf("pool balance = %v, want 1000000 — a conversion reading only the coefficient "+
			"would mis-scale every cashflow in the waterfall", spec.Deal.Pool.Balance)
	}
	if spec.Deal.Pool.GrossCoupon != 0.06 || spec.Deal.Pool.ServicingFee != 0.005 {
		t.Errorf("pool coupon/fee = %v/%v, want 0.06/0.005", spec.Deal.Pool.GrossCoupon, spec.Deal.Pool.ServicingFee)
	}
	if spec.Deal.Pool.TermMonths != 360 {
		t.Errorf("term = %v, want 360", spec.Deal.Pool.TermMonths)
	}
	if len(spec.Deal.Tranches) != 2 || spec.Deal.Tranches[0].Name != "A" {
		t.Fatalf("tranches = %+v, want A then B", spec.Deal.Tranches)
	}
	if spec.TrancheIndex != 0 {
		t.Errorf("held tranche index = %d, want 0 (the A note)", spec.TrancheIndex)
	}
	if spec.Currency != "USD" {
		t.Errorf("currency = %q, want USD — it is the discount curve's key", spec.Currency)
	}
	if spec.OAS != 0.0125 {
		t.Errorf("OAS = %v, want 0.0125", spec.OAS)
	}
	psa, isPSA := spec.Prepay.(structured.PSA)
	if !isPSA || psa.Multiple != 1.5 || psa.CDR != 0.01 || psa.Sev != 0.35 {
		t.Errorf("prepay = %#v, want PSA{1.5, 0.01, 0.35}", spec.Prepay)
	}
}

// AN ABSENT QUOTED OAS AND A ZERO ONE ARE DIFFERENT RECORDS, and that is the
// whole reason the field is `optional double` (#572).
//
// Zero is a real mark — a tranche trading flat to the curve — and it is also
// what proto3 reads an unset double as. Collapsing them would carry every
// subordinate tranche on the estate at zero spread, which is the flattering
// direction and indistinguishable from a measurement. So absent is REFUSED and
// an explicit zero RESOLVES.
func TestToStructuredSpec_AnAbsentQuotedOASIsRefusedAndAnExplicitZeroIsNot(t *testing.T) {
	missing := aDeal()
	missing.QuotedOas = nil
	if _, ok := toStructuredSpec(missing); ok {
		t.Error("a deal with no quoted OAS converted — the tranche would be carried flat to the " +
			"curve on nobody's authority, which is the confident-zero shape #585 removed one " +
			"layer down", //nolint:gocritic // the message is the assertion
		)
	}

	flat := aDeal()
	flat.QuotedOas = oas(0)
	spec, ok := toStructuredSpec(flat)
	if !ok {
		t.Fatal("a deal explicitly marked flat to the curve was refused — presence, not value, is " +
			"what this arm reads")
	}
	if spec.OAS != 0 {
		t.Errorf("OAS = %v, want 0", spec.OAS)
	}
}

// A HELD TRANCHE THAT NAMES NOTHING IN THE DEAL IS REFUSED.
//
// Falling back to the senior note would report a AAA's duration for whatever is
// actually held — off by an order of magnitude for an equity piece, in the
// direction that passes a limit. Refusing puts the record's id in an exclusion
// instead.
func TestToStructuredSpec_TheHeldTrancheMustExistAndBeUnambiguous(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*referencepb.StructuredTerms)
	}{
		{"names no tranche", func(d *referencepb.StructuredTerms) { d.HeldTranche = "Z" }},
		{"unset", func(d *referencepb.StructuredTerms) { d.HeldTranche = "" }},
		{"ambiguous", func(d *referencepb.StructuredTerms) { d.Tranches[1].Name = "A" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := aDeal()
			tc.mutate(d)
			if _, ok := toStructuredSpec(d); ok {
				t.Errorf("a deal whose held tranche %s converted — the spec would price a slice of "+
					"the capital structure nobody selected", tc.name)
			}
		})
	}
}

// THE CAPITAL STRUCTURE IS ORDERED BY SENIORITY, AND THE HELD INDEX FOLLOWS IT.
//
// Deal.Project pays interest and principal by SLICE POSITION and allocates
// losses in reverse, so the order IS the waterfall. A record that lists the
// equity piece first would have it paid before the senior note — and the held
// index, computed against the same ordering, has to move with it.
func TestToStructuredSpec_TranchesAreOrderedBySeniority(t *testing.T) {
	d := aDeal()
	d.Tranches[0], d.Tranches[1] = d.Tranches[1], d.Tranches[0] // listed junior-first

	spec, ok := toStructuredSpec(d)
	if !ok {
		t.Fatal("a deal listed junior-first did not convert")
	}
	if spec.Deal.Tranches[0].Name != "A" || spec.Deal.Tranches[1].Name != "B" {
		t.Fatalf("order = %v/%v, want A then B — the slice order is the waterfall",
			spec.Deal.Tranches[0].Name, spec.Deal.Tranches[1].Name)
	}
	if spec.TrancheIndex != 0 {
		t.Errorf("held index = %d, want 0 — it must be resolved against the SORTED structure, or "+
			"reordering silently repoints the position at a different note", spec.TrancheIndex)
	}
}

// A BEHAVIORAL PREPAYMENT ASSUMPTION IS DECLINED, NOT APPROXIMATED (#572).
//
// structured.Behavioral needs Base, Max and Steepness — the S-curve that makes
// prepayment respond to rates, and the source of the negative convexity this
// family exists to measure. reference.v1.PrepaymentAssumption carries none of
// them. Substituting a flat CPR would report a mortgage tranche as positively
// convex: the wrong SIGN, not merely the wrong size, and served with the same
// confidence as a measurement.
func TestToStructuredSpec_AnUnrepresentablePrepaymentModelIsDeclined(t *testing.T) {
	for _, model := range []string{"BEHAVIORAL", "", "CPR", "MONTE_CARLO"} {
		d := aDeal()
		d.BaseAssumption.Model = model
		if _, ok := toStructuredSpec(d); ok {
			t.Errorf("model %q converted — a prepayment assumption is most of a mortgage "+
				"tranche's duration, and a silent substitution reports a different deal under "+
				"this one's name", model)
		}
	}
}

// A CONSTANT-CPR DEAL RESOLVES, so the refusal above is about what the schema
// cannot express rather than about the conversion refusing everything.
func TestToStructuredSpec_ConstantCPRResolves(t *testing.T) {
	d := aDeal()
	d.BaseAssumption = &referencepb.PrepaymentAssumption{Model: "CONSTANT", Cpr: 0.08, Cdr: 0.01, Severity: 0.4}
	spec, ok := toStructuredSpec(d)
	if !ok {
		t.Fatal("a CONSTANT-CPR deal did not convert")
	}
	c, isConst := spec.Prepay.(structured.ConstantCPR)
	if !isConst || c.CPR != 0.08 {
		t.Errorf("prepay = %#v, want ConstantCPR{CPR: 0.08}", spec.Prepay)
	}
}

// THE FIELDS WHOSE UNSET VALUE IS A PLAUSIBLE NUMBER ARE ALL REFUSED.
//
// Each case below reads as a real deal to a caller and produces a projection
// that is silently not the one that was loaded. They are grouped because they
// are one property: proto3 has no presence for scalars, so "nobody said" arrives
// as a value, and the only place that can be caught is here.
func TestToStructuredSpec_RefusesRecordsWhoseDefaultsWouldReadAsMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*referencepb.StructuredTerms)
	}{
		{"no pool", func(d *referencepb.StructuredTerms) { d.Pool = nil }},
		{"pool with no balance", func(d *referencepb.StructuredTerms) { d.Pool.OriginalBalance = nil }},
		{"pool with no term", func(d *referencepb.StructuredTerms) { d.Pool.TermMonths = 0 }},
		{"pool with no gross coupon", func(d *referencepb.StructuredTerms) { d.Pool.GrossCoupon = 0 }},
		{"servicing fee above the coupon", func(d *referencepb.StructuredTerms) { d.Pool.ServicingFee = 0.1 }},
		{"a non-monthly pool", func(d *referencepb.StructuredTerms) {
			d.Pool.PaymentFrequency = referencepb.PaymentFrequency_PAYMENT_FREQUENCY_QUARTERLY
		}},
		{"no tranches", func(d *referencepb.StructuredTerms) { d.Tranches = nil }},
		{"a tranche with no balance", func(d *referencepb.StructuredTerms) { d.Tranches[1].OriginalBalance = nil }},
		{"an unnamed tranche", func(d *referencepb.StructuredTerms) { d.Tranches[1].Name = "" }},
		{"no currency", func(d *referencepb.StructuredTerms) { d.CurrencyCode = "" }},
		{"no prepayment assumption", func(d *referencepb.StructuredTerms) { d.BaseAssumption = nil }},
		{"a PSA speed of zero", func(d *referencepb.StructuredTerms) { d.BaseAssumption.PsaMultiple = 0 }},
		{"a severity above one", func(d *referencepb.StructuredTerms) { d.BaseAssumption.Severity = 1.5 }},
		{"a CDR of one", func(d *referencepb.StructuredTerms) { d.BaseAssumption.Cdr = 1 }},
		{"an OAS of 100%", func(d *referencepb.StructuredTerms) { d.QuotedOas = oas(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := aDeal()
			tc.mutate(d)
			if _, ok := toStructuredSpec(d); ok {
				t.Errorf("%s converted — the spec would project a deal nobody loaded", tc.name)
			}
		})
	}
}

// A ZERO-COUPON (PRINCIPAL-ONLY) TRANCHE IS VALID and must not be caught by the
// refusals above. The distinction matters: a tranche coupon of zero is a real
// structure, while a POOL coupon of zero is an unloaded field, and treating them
// alike in either direction is wrong.
func TestToStructuredSpec_APrincipalOnlyTrancheIsNotAMissingCoupon(t *testing.T) {
	d := aDeal()
	d.Tranches[1].Coupon = 0
	if _, ok := toStructuredSpec(d); !ok {
		t.Error("a principal-only tranche was refused — it is a real structure, not a missing field")
	}
}
