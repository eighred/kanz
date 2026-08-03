package publish_test

import (
	"context"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/pkg/bus"
)

// The bus is the other way a risk number reaches a consumer, and it is
// the way the compliance monitor and the archiver get theirs.
//
// domain.v1.RiskMeasureSet has no field for "this is partial", so a
// currency-excluded set would otherwise land on the spine looking
// complete (#257). envelope.v1.QUALITY_FLAG_DEGRADED is defined as
// "produced from a degraded or PARTIAL source", so the coverage signal
// rides the envelope. These tests pin that it is set when it should be
// and — just as important — absent when it should not.

func measureSetWithExclusions(ex ...v1.CurrencyExclusion) *domain.MeasureSet {
	return domain.NewMeasureSet("PORT-1", baseTime, map[v1.MeasureName]v1.Measure{
		"GrossExposure": {Name: "GrossExposure", Value: &commonpb.Decimal{Coefficient: 1000}},
	}, domain.WithCurrencyExclusions(ex))
}

func envelopeOf(t *testing.T, cc *captureClient) *envelopepb.Envelope {
	t.Helper()
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("emitted envelope fails Validate: %v", err)
	}
	return env
}

func hasEnvelopeFlag(env *envelopepb.Envelope, want envelopepb.QualityFlag) bool {
	for _, f := range env.GetQualityFlags() {
		if f == want {
			return true
		}
	}
	return false
}

func TestEmitMeasures_PartialSetIsFlaggedDegradedOnTheEnvelope(t *testing.T) {
	pub, cc := newPublisher(t)
	ms := measureSetWithExclusions(v1.CurrencyExclusion{InstrumentID: "SAP", Currency: "EUR"})

	if err := pub.EmitMeasures(context.Background(), ms, nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	env := envelopeOf(t, cc)
	if !hasEnvelopeFlag(env, envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED) {
		t.Errorf("envelope quality_flags=%v, missing QUALITY_FLAG_DEGRADED\n\n"+
			"This FACT's measures cover only part of the book. RiskMeasureSet has no "+
			"field to say so, so without the envelope flag every bus consumer — the "+
			"compliance monitor included — reads an understated exposure as the "+
			"fund's risk (#257).", env.GetQualityFlags())
	}
}

func TestEmitMeasures_CompleteSetCarriesNoDegradedFlag(t *testing.T) {
	pub, cc := newPublisher(t)

	if err := pub.EmitMeasures(context.Background(), measureSetWithExclusions(), nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	env := envelopeOf(t, cc)
	if hasEnvelopeFlag(env, envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED) {
		t.Errorf("envelope quality_flags=%v — a complete measure set must publish "+
			"clean, or DEGRADED is on every risk FACT and means nothing",
			env.GetQualityFlags())
	}
}
