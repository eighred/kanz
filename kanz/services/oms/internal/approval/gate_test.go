package approval

import (
	"math/big"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
)

// marks is a reference-mark source. nil for an instrument means "no usable
// price", which is what mark.Source returns for an unknown or aged-out mark.
type marks map[string]*big.Rat

func (m marks) Mark(instrument string) *big.Rat { return m[instrument] }

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad rational " + s)
	}
	return r
}

// limitOrder is 5 units at 50,000 — a notional of 250,000.
func limitOrder() *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		OrderId:      "ord-1",
		PortfolioId:  "flagship",
		InstrumentId: "BTC-USD",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     d(5, 0),
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   d(50_000, 0),
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_GTC,
	}
}

func marketOrder() *orderpb.SubmitOrder {
	c := limitOrder()
	c.OrderType = orderpb.OrderType_ORDER_TYPE_MARKET
	c.LimitPrice = nil
	return c
}

func gate(t *testing.T, require bool, threshold *big.Rat, m marks) *Gate {
	t.Helper()
	var src = m
	g, err := NewGate(require, threshold, src, nil)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	return g
}

// TestNoThresholdIsAbsentAndNotBelow. "Nothing configured" and "checked, and
// fine" must not look the same — on a dashboard, folding an unconfigured control
// into below_threshold would report the whole book as small and safe.
func TestNoThresholdIsAbsentAndNotBelow(t *testing.T) {
	g := gate(t, false, nil, nil)

	got := g.Decide(limitOrder())
	if got.Posture != PostureAbsent {
		t.Fatalf("posture = %q, want %q — an unconfigured control must not report as a passed check",
			got.Posture, PostureAbsent)
	}
	if got.Required {
		t.Error("dual control was required with no threshold configured")
	}
	if got.Act != dualcontrol.ActOrderSubmission {
		t.Errorf("act = %q, want %q", got.Act, dualcontrol.ActOrderSubmission)
	}
}

// TestTheThresholdIsInclusiveAtTheBoundary. "At or above it" is the ruling's
// wording, and an off-by-one here is a control that exempts precisely the order
// sized to the limit.
func TestTheThresholdIsInclusiveAtTheBoundary(t *testing.T) {
	g := gate(t, true, rat("250000"), nil)

	got := g.Decide(limitOrder()) // exactly 250,000
	if got.Posture != PostureAtOrAbove {
		t.Fatalf("an order exactly AT the threshold reported %q — the ruling says at or above",
			got.Posture)
	}
	if !got.Required {
		t.Fatal("an armed gate did not require dual control for an order at the threshold")
	}
}

func TestAnOrderBelowTheThresholdDoesNotRequireDualControl(t *testing.T) {
	g := gate(t, true, rat("250000.01"), nil)

	got := g.Decide(limitOrder())
	if got.Posture != PostureBelow {
		t.Fatalf("posture = %q, want %q", got.Posture, PostureBelow)
	}
	if got.Required {
		t.Error("dual control was required below the threshold")
	}
	if got.Digest != "" {
		t.Error("a digest was computed for an order below the threshold — a sha256 per admission " +
			"for the whole book buys nothing")
	}
}

// TestAnOrderThatCannotBePricedIsUnvaluableAndNeverBelow is the fail-closed
// property. "Cannot price it" must never mean "below the threshold": an order
// whose size nobody could compute is precisely the one a control must not exempt.
func TestAnOrderThatCannotBePricedIsUnvaluableAndNeverBelow(t *testing.T) {
	// A MARKET order carries no price of its own and there is no mark for it.
	g := gate(t, false, rat("250000"), marks{})

	got := g.Decide(marketOrder())
	if got.Posture != PostureUnvaluable {
		t.Fatalf("posture = %q, want %q — an order the platform could not size must not be counted "+
			"as small", got.Posture, PostureUnvaluable)
	}
}

// TestAnUnvaluableOrderRequiresDualControlWhenArmed. The safe direction on a
// control is more signatures, not fewer.
func TestAnUnvaluableOrderRequiresDualControlWhenArmed(t *testing.T) {
	g := gate(t, true, rat("250000"), marks{})

	if got := g.Decide(marketOrder()); !got.Required {
		t.Fatalf("an armed gate admitted an order it could not value without a second signature "+
			"(posture %q) — every MARKET order becomes an exemption the moment the price spine is quiet",
			got.Posture)
	}
}

// TestAMarketOrderIsValuedAtTheReferenceMark, using the same price selection the
// pre-trade compliance gate uses. An operator who sets one number must not get
// two behaviours from it.
func TestAMarketOrderIsValuedAtTheReferenceMark(t *testing.T) {
	g := gate(t, false, rat("250000"), marks{"BTC-USD": rat("50000")})

	if got := g.Decide(marketOrder()); got.Posture != PostureAtOrAbove {
		t.Fatalf("posture = %q, want %q — 5 units at a mark of 50,000 is 250,000",
			got.Posture, PostureAtOrAbove)
	}
}

// TestASellIsAsLargeAsABuy. Direction is carried by side, not by the sign of the
// quantity — but a negative coefficient reaching here must not make an order look
// small.
func TestASellIsAsLargeAsABuy(t *testing.T) {
	g := gate(t, false, rat("250000"), nil)

	cmd := limitOrder()
	cmd.Side = orderpb.Side_SIDE_SELL
	cmd.Quantity = d(-5, 0)
	if got := g.Decide(cmd); got.Posture != PostureAtOrAbove {
		t.Fatalf("posture = %q for a signed quantity — abs is what makes size independent of "+
			"direction", got.Posture)
	}
}

// TestAnOrderAtOrAboveTheThresholdCarriesTheDigestASecondSignatureWouldCover.
// UNARMED IS NOT UNRECORDED: the value is computed and logged now, so arming the
// control later does not leave today's large orders ambiguous.
func TestAnOrderAtOrAboveTheThresholdCarriesTheDigestASecondSignatureWouldCover(t *testing.T) {
	g := gate(t, false, rat("250000"), nil)

	got := g.Decide(limitOrder())
	if got.DigestErr != nil {
		t.Fatalf("digest: %v", got.DigestErr)
	}
	want, err := TermsOfSubmit(limitOrder()).Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got.Digest != want {
		t.Fatalf("the gate recorded a digest that does not match the order's terms (%q vs %q) — an "+
			"approver could not be shown to have signed for this order", got.Digest, want)
	}
}

// TestArmingWithNoThresholdIsRefusedByTheConstructorToo. config.Load refuses it
// first; this is the second line, because a constructor callable from a test is
// not a guarantee.
func TestArmingWithNoThresholdIsRefusedByTheConstructorToo(t *testing.T) {
	_, err := NewGate(true, nil, nil, nil)
	if err == nil {
		t.Fatal("NewGate armed the control with no threshold — every order would be compared " +
			"against nothing")
	}
	for _, want := range []string{"OMS_REQUIRE_DUAL_CONTROL", "OMS_DUAL_CONTROL_MIN_NOTIONAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %s: %q", want, err.Error())
		}
	}
}

func TestANonPositiveThresholdIsRefusedByTheConstructor(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		if _, err := NewGate(false, rat(v), nil, nil); err == nil {
			t.Errorf("NewGate accepted a threshold of %s — every order would be at or above it", v)
		}
	}
}

// TestEverySeriesExistsBeforeAnythingFires. A counter that appears only once it
// fires makes "no large order has ever been admitted" and "this build does not
// have the gate" identical on a dashboard, and the second is the one worth
// knowing.
func TestEverySeriesExistsBeforeAnythingFires(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := NewGate(false, nil, nil, reg); err != nil {
		t.Fatalf("NewGate: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "kanz_oms_order_signatures_total" {
			found = f
		}
	}
	if found == nil {
		t.Fatal("kanz_oms_order_signatures_total was not registered — with no threshold configured " +
			"the metric is the ONLY thing saying so, and its absence is indistinguishable from a " +
			"build without the gate")
	}
	// 2 signature values x 4 postures.
	if got := len(found.GetMetric()); got != 8 {
		t.Fatalf("%d series exist before anything fired, want 8 — a label value that only appears "+
			"on its first increment cannot be alerted on", got)
	}
}

// TestTheMetricLabelsAreASmallClosedSet. This counter runs on EVERY admission, so
// an order id or an instrument here is unbounded cardinality — the kind that
// takes a Prometheus down rather than reporting on one.
func TestTheMetricLabelsAreASmallClosedSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	g, err := NewGate(false, rat("250000"), nil, reg)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}

	// Admit two orders that differ in id, portfolio and instrument. If either
	// reached a label, the series count would grow.
	first := limitOrder()
	second := limitOrder()
	second.OrderId, second.PortfolioId, second.InstrumentId = "ord-2", "research", "ETH-USD"
	g.Count(g.Decide(first), SingleSigned)
	g.Count(g.Decide(second), SingleSigned)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_oms_order_signatures_total" {
			continue
		}
		if got := len(f.GetMetric()); got != 8 {
			t.Fatalf("%d series after two orders, want the same 8 — a per-order label value has been "+
				"added and this counter fires on every admission", got)
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "signatures", "posture":
				default:
					t.Errorf("unexpected label %q — the label set must stay closed", l.GetName())
				}
			}
		}
	}
}

// TestSingleSignedIsCountedForEveryAdmittedOrder is the number the arming
// decision rests on.
func TestSingleSignedIsCountedForEveryAdmittedOrder(t *testing.T) {
	reg := prometheus.NewRegistry()
	g, err := NewGate(false, rat("250000"), nil, reg)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}

	g.Count(g.Decide(limitOrder()), SingleSigned) // 250,000 ⇒ at or above
	small := limitOrder()
	small.Quantity = d(1, -3)
	g.Count(g.Decide(small), SingleSigned) // 50 ⇒ below

	if got := counterValue(t, reg, SingleSigned, string(PostureAtOrAbove)); got != 1 {
		t.Errorf("single_signed/at_or_above_threshold = %v, want 1 — this is the count that says how "+
			"much of the LARGE order flow one person committed alone", got)
	}
	if got := counterValue(t, reg, SingleSigned, string(PostureBelow)); got != 1 {
		t.Errorf("single_signed/below_threshold = %v, want 1", got)
	}
	if got := counterValue(t, reg, DualSigned, string(PostureAtOrAbove)); got != 0 {
		t.Errorf("dual_signed = %v with nothing collecting a second signature yet", got)
	}
}

func counterValue(t *testing.T, reg *prometheus.Registry, signatures, posture string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_oms_order_signatures_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var s, p string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "signatures":
					s = l.GetValue()
				case "posture":
					p = l.GetValue()
				}
			}
			if s == signatures && p == posture {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no series for signatures=%q posture=%q", signatures, posture)
	return 0
}

// TestANilGateIsSilentRatherThanFatal. A Service built without the option is the
// test default; a metric that panicked when unobserved would make the
// observability wiring load-bearing for admission itself.
func TestANilGateIsSilentRatherThanFatal(t *testing.T) {
	var g *Gate
	got := g.Decide(limitOrder())
	if got.Posture != PostureAbsent {
		t.Fatalf("posture = %q from a nil gate, want %q", got.Posture, PostureAbsent)
	}
	g.Count(got, SingleSigned) // must not panic
	if g.Armed() || g.Watching() || g.Threshold() != nil {
		t.Error("a nil gate reported itself configured")
	}
}
