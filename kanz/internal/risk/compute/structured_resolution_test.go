package compute

import (
	"context"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// The four answers a StructuredProvider can now give, and what each one licenses
// (#572). Kept beside structured_test.go rather than inside it because these
// four are one property — the widening of a bool into a resolution — and the
// file next door is about what the MEASURES do with a spec once one arrives.

// A RECORD THAT SAYS "THIS IS NOT A SECURITIZATION" IS THE ONE SILENT ABSENCE,
// and it is the arm the schema ruling made reachable.
//
// Before #572 nothing on this estate could describe a structured product at all,
// so a provider's "no" certified nothing and every share on the book had to be
// reported as unassessed — flagging every response with
// QualityFlagInputsUnresolved, which is noise an operator learns to scroll past.
// With a `structured` case in the ContractTerms oneof, a record carrying an
// OPTION variant positively establishes that the instrument holds no tranche, so
// its absence from the structured measures is an answer rather than a gap.
//
// THIS IS THE ONLY ARM THAT MAY BE SILENT. If it ever became reachable from a
// provider that merely failed to find something, #527's confident zero would be
// back with no signal at all — which is why the fixture that holds no record
// answers TermsUnknown and is asserted separately, next door.
func TestStructMeasures_ACertifiedNonSecuritizationIsASilentAbsence(t *testing.T) {
	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, structProviders(t, certifyingStore{}))
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "EQ", MarketValue: &commonpb.Money{Amount: dec(50_000, 0), CurrencyCode: "USD"}})

	ms := ComputeMeasures(p, r, nil)
	for _, name := range []v1.MeasureName{MeasureStructDuration, MeasureStructConvexity, MeasureStructWAL} {
		m, ok := ms.Lookup(name)
		if !ok {
			t.Fatalf("%s missing from the set", name)
		}
		if m.Coverage.ExcludedCount != 0 || len(m.Coverage.Exclusions) != 0 {
			t.Errorf("%s: coverage = %+v, want NO exclusions — a terms record positively says this "+
				"instrument is not a securitization, so reporting it would flag every equity on the "+
				"estate and bury the tranches that really were dropped (#572)", name, m.Coverage)
		}
		if m.Coverage.Contributed != 0 {
			t.Errorf("%s: Contributed=%d, want 0 — nothing was priced", name, m.Coverage.Contributed)
		}
	}
}

// A DEAL THE STORE CANNOT DESCRIBE IS DECLINED, NOT PRICED (#572/#585).
//
// This is the property that makes supporting only PART of the family safe. The
// terms provider refuses a STRUCTURED record it cannot turn into a spec — a
// held_tranche naming no tranche, a BEHAVIORAL prepayment assumption whose
// S-curve parameters the schema does not carry, an absent quoted OAS — and the
// measure records it under its own reason rather than folding a zero into the
// average. A structured NOTE reaches the same outcome one step earlier: nothing
// can write a record for it, so it never resolves at all.
func TestStructMeasures_AnUnusableDealIsDeclinedWithItsOwnReason(t *testing.T) {
	var skipped []string
	pr := structProviders(t, unusableDealStore{})
	pr.OnSkip = func(_, reason string) { skipped = append(skipped, reason) }

	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, pr)
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "MBS_A", MarketValue: &commonpb.Money{Amount: dec(950_000, 0), CurrencyCode: "USD"}})

	ms := ComputeMeasures(p, r, nil)
	dur, ok := ms.Lookup(MeasureStructDuration)
	if !ok {
		t.Fatalf("StructDuration missing from the set")
	}
	if decimalToFloat(dur.Value) != 0 || dur.Coverage.Contributed != 0 {
		t.Fatalf("value=%.4f contributed=%d — the fixture must price nothing",
			decimalToFloat(dur.Value), dur.Coverage.Contributed)
	}
	if len(dur.Coverage.Exclusions) != 1 || dur.Coverage.Exclusions[0].Reason != SkipUnusableDeal {
		t.Errorf("exclusions = %+v, want one MBS_A:%s — an unusable deal must be attributable to "+
			"the RECORD, not merged with an unknown instrument, because the repair is different",
			dur.Coverage.Exclusions, SkipUnusableDeal)
	}
	// THREE MEASURES SHARE ONE PER-POSITION PATH, so one dropped tranche reports
	// three skip events. Asserted so the counter's Help text and its behaviour
	// cannot drift apart.
	if len(skipped) != 3 {
		t.Errorf("OnSkip fired %d times, want 3 (once per measure)", len(skipped))
	}
	for _, reason := range skipped {
		if reason != SkipUnusableDeal {
			t.Errorf("OnSkip reason = %q, want %q", reason, SkipUnusableDeal)
		}
	}
}

// A RESOLVED DEAL WITH NO CALIBRATED CURVE IS THE CALIBRATION GAP, NOT A DATA
// GAP (#572) — and it must leave the weighted average rather than enter it at
// zero.
//
// PriceTranche refuses a rate environment with no discount curve. While the
// curve lived INSIDE the spec, that refusal arrived as "unpriceable_tranche",
// sending whoever read it to look for a malformed record that does not exist.
// SkipNoCurve is the FI band's constant, reused rather than respelled, because
// it is the same fact about the same curve store.
func TestStructMeasures_ADealWithNoCalibratedCurveIsExcludedAsSuch(t *testing.T) {
	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, StructuredProviders{
		Terms: staticStructured{"MBS_A": structTestSpec(t)},
		Curve: staticCurve{}, // nothing calibrated
	})
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "MBS_A", MarketValue: &commonpb.Money{Amount: dec(950_000, 0), CurrencyCode: "USD"}})

	ms := ComputeMeasures(p, r, nil)
	for _, name := range []v1.MeasureName{MeasureStructDuration, MeasureStructConvexity, MeasureStructWAL} {
		m, ok := ms.Lookup(name)
		if !ok {
			t.Fatalf("%s missing from the set", name)
		}
		if m.Coverage.Contributed != 0 {
			t.Errorf("%s: Contributed=%d, want 0 — nothing can be priced without a curve",
				name, m.Coverage.Contributed)
		}
		if len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != SkipNoCurve {
			t.Errorf("%s: exclusions = %+v, want one MBS_A:%s", name, m.Coverage.Exclusions, SkipNoCurve)
		}
	}
}
