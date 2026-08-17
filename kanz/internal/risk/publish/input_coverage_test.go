package publish_test

import (
	"context"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// THE BUS MUST NOT SEE A MEASURED-NOTHING ZERO AS A CLEAN NUMBER (#527).
//
// measureQualityFlags used to ask ONE question — are there currency exclusions —
// so a measure set whose FI family priced nothing published with no envelope
// flag at all. The compliance monitor and the archiver take their risk numbers
// from this path, and a DV01 of zero on a clean FACT is an assertion that the
// book carries no rate risk.
//
// The envelope enum cannot say WHICH kind of partial it is; that resolution
// exists only on the query surface. What it must not do is stay silent.
func measureSetWithUnresolvedInputs() *domain.MeasureSet {
	return domain.NewMeasureSet("PORT-1", baseTime, map[v1.MeasureName]v1.Measure{
		"DV01": {
			Name:  "DV01",
			Value: &commonpb.Decimal{Coefficient: 0},
			Coverage: v1.InputCoverage{
				ExcludedCount: 1,
				Exclusions:    []v1.InputExclusion{{InstrumentID: "GOVT-10Y", Reason: "unknown_instrument"}},
			},
		},
	})
}

func TestEmitMeasures_UnresolvedInputsAreFlaggedDegradedOnTheEnvelope(t *testing.T) {
	pub, cc := newPublisher(t)

	if err := pub.EmitMeasures(context.Background(), measureSetWithUnresolvedInputs(), nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	env := envelopeOf(t, cc)
	if !hasEnvelopeFlag(env, envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED) {
		t.Errorf("envelope quality_flags=%v, missing QUALITY_FLAG_DEGRADED\n\n"+
			"This FACT's DV01 was computed over no bond at all, and the set carries no currency "+
			"exclusions — which is exactly the combination the old single-question check published "+
			"clean. Every bus consumer then reads a confident zero (#527).", env.GetQualityFlags())
	}
}

// AND A SET WITH FULL COVERAGE STILL PUBLISHES CLEAN, or DEGRADED lands on every
// risk FACT and stops distinguishing anything.
func TestEmitMeasures_FullyCoveredMeasuresPublishClean(t *testing.T) {
	pub, cc := newPublisher(t)
	ms := domain.NewMeasureSet("PORT-1", baseTime, map[v1.MeasureName]v1.Measure{
		"DV01": {Name: "DV01", Value: &commonpb.Decimal{Coefficient: 0}, Coverage: v1.InputCoverage{Contributed: 3}},
	})

	if err := pub.EmitMeasures(context.Background(), ms, nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	if env := envelopeOf(t, cc); hasEnvelopeFlag(env, envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED) {
		t.Errorf("envelope quality_flags=%v — three bonds priced and nothing was excluded",
			env.GetQualityFlags())
	}
}
