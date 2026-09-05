package compute_test

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
)

// A MEASURE NAMES THE MODEL THAT PRODUCED IT (#1037).
//
// The 1%×gross VaR99 placeholder and historical-simulation VaR99 were
// BYTE-IDENTICAL on the wire: same name, same shape, a number in each. Which one
// a deployment served depended on RISK_ENGINE_MARKETDATA_DATABASE_URL, which no
// manifest sets, and the OMS gate checked a mandate's VaR limit against whichever
// arrived. For a leveraged book 1% of gross is materially BELOW a one-day 99%
// VaR, so the gate admitted orders it should have refused — and nothing on the
// response could tell the two apart.
//
// These tests pin the two ends of that fork. They live in this directory rather
// than beside each model because the fork is the point: one `go test
// ./internal/risk/compute/ -run Provenance` has to see BOTH answers, and only an
// external test package can import varmodel (which imports compute).

// flatReturnsProvider hands every instrument the same return series.
type flatReturnsProvider struct{ ret []float64 }

func (f flatReturnsProvider) Returns(_ context.Context, _ string, _ time.Time, _ int) ([]float64, error) {
	return f.ret, nil
}

func provenancePortfolio(t *testing.T) *domain.Portfolio {
	t.Helper()
	asOf := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	p := domain.NewPortfolio(v1.PortfolioID("PF-PROV"), "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: asOf, BaseCurrency: "USD"})
	p.SetPosition(domain.Position{
		InstrumentID: "AAPL",
		MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1000, Exponent: 0}, CurrencyCode: "USD"},
		AsOf:         asOf,
	})
	return p
}

// TestVaR99Provenance_DeclaresThePlaceholder is the arm that makes the shipped
// rollout legible. An engine with no market-data DSN answers every VaR99 query
// from compute.VaR99, and this is the only thing on that answer that says so.
func TestVaR99Provenance_DeclaresThePlaceholder(t *testing.T) {
	m := compute.VaR99(provenancePortfolio(t))

	if got := m.Provenance.Method; got != v1.MethodPlaceholder1PctGross {
		t.Fatalf("VaR99 provenance method = %q, want %q — the 1%%×gross placeholder must "+
			"declare itself, or the OMS gate cannot refuse it", got, v1.MethodPlaceholder1PctGross)
	}
	if !m.Provenance.Method.IsPlaceholder() {
		t.Errorf("%q is not classified as a placeholder — riskview refuses on this predicate, "+
			"so an unclassified placeholder gates order admission", m.Provenance.Method)
	}
}

// TestDeltaProvenance_DeclaresTheNetExposurePlaceholder covers the sibling case
// the posture gauge's own help text conceded: Delta is NetExposure under another
// name and is served as a greek on every deployment.
func TestDeltaProvenance_DeclaresTheNetExposurePlaceholder(t *testing.T) {
	m := compute.Delta(provenancePortfolio(t))

	if got := m.Provenance.Method; got != v1.MethodNetExposurePlaceholder {
		t.Fatalf("Delta provenance method = %q, want %q", got, v1.MethodNetExposurePlaceholder)
	}
	if !m.Provenance.Method.IsPlaceholder() {
		t.Errorf("%q is not classified as a placeholder", m.Provenance.Method)
	}
}

// TestHistoricalProvenance_DeclaresHistoricalSimulation is the other end of the
// fork: the SAME measure name, from a real model, must not be classified as a
// placeholder — otherwise arm (3) would refuse the calibrated number too and the
// fix would be a new outage rather than a control.
func TestHistoricalProvenance_DeclaresHistoricalSimulation(t *testing.T) {
	p := provenancePortfolio(t)
	prov := flatReturnsProvider{ret: []float64{-0.10, -0.05, 0, 0.05, 0.10}}

	m := varmodel.Historical(varmodel.Config{})(context.Background(), p, prov)

	if got := m.Provenance.Method; got != v1.MethodHistoricalSimulation {
		t.Fatalf("historical-simulation VaR99 provenance method = %q, want %q",
			got, v1.MethodHistoricalSimulation)
	}
	if m.Provenance.Method.IsPlaceholder() {
		t.Errorf("historical simulation is classified as a placeholder — the admission gate " +
			"would refuse the calibrated VaR as well as the illustrative one")
	}
	// The parameters an operator needs to reproduce the number, not a second copy
	// of the value. Confidence is what makes "VaR99" mean 99%.
	if got := m.Provenance.Params["confidence"]; got != "0.99" {
		t.Errorf("provenance params[confidence] = %q, want 0.99", got)
	}
}

// TestProvenance_TheTwoVaR99AnswersAreDistinguishable is the defect stated as a
// test. Both are named VaR99, both carry a Decimal, and before #1037 a consumer
// had nothing else to read.
func TestProvenance_TheTwoVaR99AnswersAreDistinguishable(t *testing.T) {
	p := provenancePortfolio(t)
	placeholder := compute.VaR99(p)
	calibrated := varmodel.Historical(varmodel.Config{})(context.Background(), p,
		flatReturnsProvider{ret: []float64{-0.10, -0.05, 0, 0.05, 0.10}})

	if placeholder.Name != calibrated.Name {
		t.Fatalf("the premise changed: %q vs %q — these are supposed to be the same measure "+
			"name from two different models", placeholder.Name, calibrated.Name)
	}
	if placeholder.Provenance.Method == calibrated.Provenance.Method {
		t.Fatalf("both VaR99 answers declare method %q — the placeholder and the calibrated "+
			"model are still indistinguishable on the wire", placeholder.Provenance.Method)
	}
}
