package publish_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/pkg/bus"
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

// --- The PAYLOAD half (#509) -------------------------------------------
//
// The envelope flag above says THAT something in the set was computed over
// nothing. It cannot say which measure, and that is not a detail: a caller
// gating on DV01 either refuses the whole response — throwing away a
// GrossExposure that was never in doubt — or ignores the flag and sizes a
// position against a confident zero. InputCoverage's own doc requires the
// first reading ("a REFUSAL TO ANSWER, not an annotation on a good number"),
// which a set-level bit cannot support.
//
// So the per-measure record rides the payload, and these pin that it survives
// the domain→proto translation both bus consumers and the gRPC query surface
// go through — ToProtoMeasureSet is the single mapping for both.

func measureSetPayload(t *testing.T, cc *captureClient) *domainpb.RiskMeasureSet {
	t.Helper()
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}
	_, payload, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	var set domainpb.RiskMeasureSet
	if err := proto.Unmarshal(payload, &set); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	return &set
}

func wireMeasure(t *testing.T, set *domainpb.RiskMeasureSet, name string) *domainpb.RiskMeasure {
	t.Helper()
	for _, m := range set.GetMeasures() {
		if m.GetName() == name {
			return m
		}
	}
	t.Fatalf("measure %q absent from the payload; got %d measures", name, len(set.GetMeasures()))
	return nil
}

// A MEASURED-NOTHING ZERO MUST SAY SO ON THE WIRE, NAMING ITSELF.
func TestEmitMeasures_CoverageTravelsWithTheMeasureOnTheWire(t *testing.T) {
	pub, cc := newPublisher(t)

	if err := pub.EmitMeasures(context.Background(), measureSetWithUnresolvedInputs(), nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	dv01 := wireMeasure(t, measureSetPayload(t, cc), "DV01")

	cov := dv01.GetCoverage()
	if cov == nil {
		t.Fatalf("DV01.coverage is absent — this DV01 was computed over no bond at all and " +
			"crosses the wire as a confident zero. The envelope flag says the SET is partial; " +
			"only this field says WHICH measure, so a consumer cannot refuse the degraded " +
			"number without refusing the sound ones beside it (#509).")
	}
	if cov.GetContributed() != 0 || cov.GetExcludedCount() != 1 {
		t.Errorf("DV01.coverage contributed=%d excluded_count=%d, want 0 and 1",
			cov.GetContributed(), cov.GetExcludedCount())
	}
	ex := cov.GetExclusions()
	if len(ex) != 1 || ex[0].GetInstrumentId() != "GOVT-10Y" || ex[0].GetReason() != "unknown_instrument" {
		t.Errorf("DV01.coverage exclusions=%+v, want the GOVT-10Y/unknown_instrument sample — "+
			"the count says how bad it is, the sample is what lets somebody go and look", ex)
	}
}

// AND A MEASURE WITH NO PROVIDER MUST STAY ABSENT RATHER THAN REPORT ZEROS.
//
// v1.InputCoverage's zero value means "does not report coverage", not
// "everything resolved". GrossExposure reads the portfolio directly and nothing
// can decline it, so a present-but-empty message would be a claim the engine
// never made — "checked, and fine" wearing the same shape as "not checked".
func TestEmitMeasures_AMeasureWithNoProviderReportsNoCoverage(t *testing.T) {
	pub, cc := newPublisher(t)
	ms := domain.NewMeasureSet("PORT-1", baseTime, map[v1.MeasureName]v1.Measure{
		"GrossExposure": {Name: "GrossExposure", Value: &commonpb.Decimal{Coefficient: 1000}},
	})

	if err := pub.EmitMeasures(context.Background(), ms, nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	if cov := wireMeasure(t, measureSetPayload(t, cc), "GrossExposure").GetCoverage(); cov != nil {
		t.Errorf("GrossExposure.coverage=%+v, want absent — an empty coverage message asserts "+
			"the measure looked and found nothing missing, which is a stronger claim than "+
			"'this measure does not report coverage'", cov)
	}
}

// AND A MEASURE THAT LOOKED AND FOUND EVERYTHING MUST BE DISTINGUISHABLE FROM
// ONE THAT NEVER LOOKED. This is the half a scalar pair could not carry, and the
// reason domain.v1.InputCoverage is a message.
func TestEmitMeasures_AFullyCoveredMeasureStillReportsItsCoverage(t *testing.T) {
	pub, cc := newPublisher(t)
	ms := domain.NewMeasureSet("PORT-1", baseTime, map[v1.MeasureName]v1.Measure{
		"DV01": {
			Name:     "DV01",
			Value:    &commonpb.Decimal{Coefficient: 4200, Exponent: -2},
			Coverage: v1.InputCoverage{Contributed: 3},
		},
	})

	if err := pub.EmitMeasures(context.Background(), ms, nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	cov := wireMeasure(t, measureSetPayload(t, cc), "DV01").GetCoverage()
	if cov == nil {
		t.Fatalf("DV01.coverage is absent for a measure that priced three bonds — 'checked, and " +
			"fine' now reads identically to 'nothing configured'")
	}
	if cov.GetContributed() != 3 || cov.GetExcludedCount() != 0 {
		t.Errorf("DV01.coverage contributed=%d excluded_count=%d, want 3 and 0",
			cov.GetContributed(), cov.GetExcludedCount())
	}
}
