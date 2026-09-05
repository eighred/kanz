package publish_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/pkg/bus"
)

// THE MODEL HAS TO SURVIVE THE TRANSLATION (#1037).
//
// The engine's own tests can prove compute.VaR99 declares a placeholder and the
// OMS's can prove riskview refuses one, and BOTH stay green while this file drops
// the field on the way to the wire — the producer computes it, the consumer looks
// for it, and nothing between them carries it. That failure was live for one
// mutation run: deleting `Provenance: toProtoProvenance(...)` from
// ToProtoMeasureSet compiled and passed every other test in the module, with the
// only visible effect being that an illustrative VaR silently gates order
// admission again. This is the assertion that kills it.

func measuresFromTheShippedRegistry(t *testing.T) *domain.MeasureSet {
	t.Helper()
	p := domain.NewPortfolio("PORT-1", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: baseTime, BaseCurrency: "USD"})
	p.SetPosition(domain.Position{
		InstrumentID: "AAPL",
		MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1000}, CurrencyCode: "USD"},
		AsOf:         baseTime,
	})
	// DefaultRegistry, not a hand-built set: this is the registry an engine with
	// no RISK_ENGINE_MARKETDATA_DATABASE_URL serves, which is every manifest in
	// infra/.
	return compute.ComputeMeasures(p, compute.DefaultRegistry(), nil)
}

func measuresPayload(t *testing.T, cc *captureClient) *domainpb.RiskMeasureSet {
	t.Helper()
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}
	_, body, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	var set domainpb.RiskMeasureSet
	if err := proto.Unmarshal(body, &set); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return &set
}

// THE PLACEHOLDER REACHES THE BUS DECLARING ITSELF. This is the exact FACT the
// OMS folds into the pre-trade gate, so an absent method here is an admitted
// order.
func TestEmitMeasures_ProvenanceReachesTheWire(t *testing.T) {
	pub, cc := newPublisher(t)
	if err := pub.EmitMeasures(context.Background(), measuresFromTheShippedRegistry(t), nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}

	want := map[string]v1.MeasureMethod{
		"VaR99":         v1.MethodPlaceholder1PctGross,
		"Delta":         v1.MethodNetExposurePlaceholder,
		"GrossExposure": v1.MethodPortfolioArithmetic,
		"NetExposure":   v1.MethodPortfolioArithmetic,
		"HHI":           v1.MethodPortfolioArithmetic,
	}
	got := map[string]string{}
	for _, m := range measuresPayload(t, cc).GetMeasures() {
		got[m.GetName()] = m.GetProvenance().GetMethod()
	}
	for name, method := range want {
		if got[name] != string(method) {
			t.Errorf("%s provenance.method on the wire = %q, want %q\n\n"+
				"The engine computed the model and the OMS gate reads it, and this translation is "+
				"the only thing between them. Dropped here, an illustrative constant is "+
				"byte-identical to a calibrated number again and gates order admission (#1037).",
				name, got[name], method)
		}
	}
}

// AN UNDECLARED MEASURE PUBLISHES NO PROVENANCE MESSAGE AT ALL, rather than a
// present-but-empty one. Absent means "this producer declares no model"; a
// present message with an empty method would look like a declaration, and the
// OMS gate's IsPlaceholder is false for both — so the difference an operator can
// see is the only one there is.
func TestEmitMeasures_AnUndeclaredMeasurePublishesNoProvenance(t *testing.T) {
	pub, cc := newPublisher(t)
	ms := domain.NewMeasureSet("PORT-1", baseTime, map[v1.MeasureName]v1.Measure{
		"DV01": {Name: "DV01", Value: &commonpb.Decimal{Coefficient: 0}},
	})
	if err := pub.EmitMeasures(context.Background(), ms, nil); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	for _, m := range measuresPayload(t, cc).GetMeasures() {
		if m.GetProvenance() != nil {
			t.Errorf("%s carries a provenance message %v — an empty declaration reads as one, "+
				"and every measure published before #1037 declares nothing", m.GetName(), m.GetProvenance())
		}
	}
}
