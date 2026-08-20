package venuemargin

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/bus"
)

type fakeSource struct {
	obs execution.VenueMargin
	err error
}

func (f *fakeSource) MarginState(context.Context) (execution.VenueMargin, error) {
	return f.obs, f.err
}

type capturePub struct{ events []bus.Event }

func (c *capturePub) Publish(_ context.Context, e bus.Event) error {
	c.events = append(c.events, e)
	return nil
}

func reporterOver(src *fakeSource, pub *capturePub, opts ...func(*ReporterConfig)) *Reporter {
	cfg := ReporterConfig{
		Source: src, Pub: pub, Venue: "OKX", Account: "acct-1", Tenant: "t1",
		Now: func() time.Time { return observed.Add(time.Second) },
	}
	for _, o := range opts {
		o(&cfg)
	}
	return NewReporter(cfg)
}

func onlyState(t *testing.T, pub *capturePub) *collateralpb.VenueMarginState {
	t.Helper()
	if len(pub.events) != 1 {
		t.Fatalf("published %d events, want 1", len(pub.events))
	}
	msg, ok := pub.events[0].Payload.(*collateralpb.VenueMarginState)
	if !ok {
		t.Fatalf("payload is %T, want *collateralpb.VenueMarginState", pub.events[0].Payload)
	}
	return msg
}

func full() execution.VenueMargin {
	mm, _ := new(big.Rat).SetString("1250.5")
	mr, _ := new(big.Rat).SetString("3.75")
	lp, _ := new(big.Rat).SetString("41000")
	return execution.VenueMargin{
		MaintenanceMargin: mm, MarginRatio: mr,
		Positions:  []execution.VenuePositionMargin{{Symbol: "BTC-USDT-SWAP", LiquidationPrice: lp}},
		ObservedAt: observed,
	}
}

// TestReportCarriesTheVenuesOwnFigures is the non-vacuity arm: without it a
// reporter that dropped everything would satisfy every "must not invent" test
// below.
func TestReportCarriesTheVenuesOwnFigures(t *testing.T) {
	pub := &capturePub{}
	r := reporterOver(&fakeSource{obs: full()}, pub)
	if err := r.Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	msg := onlyState(t, pub)

	if got := dec.FromProto(msg.GetMaintenanceMargin()); got.Cmp(rat(t, "1250.5")) != 0 {
		t.Errorf("maintenance margin = %s, want 1250.5", got.FloatString(4))
	}
	if got := dec.FromProto(msg.GetMarginRatio()); got.Cmp(rat(t, "3.75")) != 0 {
		t.Errorf("margin ratio = %s, want 3.75", got.FloatString(4))
	}
	if len(msg.GetLiquidationPrices()) != 1 || msg.GetLiquidationPrices()[0].GetVenueSymbol() != "BTC-USDT-SWAP" {
		t.Fatalf("liquidation prices = %v, want one for BTC-USDT-SWAP", msg.GetLiquidationPrices())
	}
	if msg.GetVenue() != "OKX" || msg.GetVenueAccountId() != "acct-1" {
		t.Errorf("attribution = (%q,%q), want (OKX,acct-1)", msg.GetVenue(), msg.GetVenueAccountId())
	}
	if cov := msg.GetCoverage(); cov.GetContributed() != 3 || cov.GetExcludedCount() != 0 {
		t.Errorf("coverage = contributed %d / excluded %d, want 3/0", cov.GetContributed(), cov.GetExcludedCount())
	}
}

// TestObservationTimeIsTheVenuesNotOurs: the envelope's EventTime is what a
// downstream freshness check reads. Stamping it with the publish clock would
// measure how recently WE ASKED, not how recently the exchange looked — the one
// direction of error a staleness bound cannot survive.
func TestObservationTimeIsTheVenuesNotOurs(t *testing.T) {
	pub := &capturePub{}
	r := reporterOver(&fakeSource{obs: full()}, pub)
	if err := r.Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	msg := onlyState(t, pub)

	if !msg.GetObservedAt().AsTime().Equal(observed) {
		t.Errorf("observed_at = %s, want the VENUE's %s", msg.GetObservedAt().AsTime(), observed)
	}
	if !pub.events[0].EventTime.Equal(observed) {
		t.Errorf("envelope EventTime = %s, want the venue's observation time %s",
			pub.events[0].EventTime, observed)
	}
	if !msg.GetKnowledgeTime().AsTime().Equal(observed.Add(time.Second)) {
		t.Errorf("knowledge_time = %s, want our clock %s",
			msg.GetKnowledgeTime().AsTime(), observed.Add(time.Second))
	}
	if pub.events[0].Subject != Subject || pub.events[0].PartitionKey != "acct-1" ||
		pub.events[0].EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event = subject %q key %q class %v, want %q / acct-1 / FACT",
			pub.events[0].Subject, pub.events[0].PartitionKey, pub.events[0].EventClass, Subject)
	}
}

// TestUnreportedQuantitiesAreAbsentNotZero is the heart of the control. A source
// that saw nothing must produce a message with NO margin fields — never a zero
// Decimal, which downstream reads as "this account needs no collateral".
func TestUnreportedQuantitiesAreAbsentNotZero(t *testing.T) {
	pub := &capturePub{}
	r := reporterOver(&fakeSource{obs: execution.VenueMargin{ObservedAt: observed}}, pub)
	if err := r.Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	msg := onlyState(t, pub)

	if msg.GetMaintenanceMargin() != nil {
		t.Errorf("maintenance margin = %v, want ABSENT — the venue reported none", msg.GetMaintenanceMargin())
	}
	if msg.GetMarginRatio() != nil {
		t.Errorf("margin ratio = %v, want ABSENT", msg.GetMarginRatio())
	}
	if cov := msg.GetCoverage(); cov.GetContributed() != 0 || cov.GetExcludedCount() != 2 {
		t.Fatalf("coverage = contributed %d / excluded %d, want 0/2", cov.GetContributed(), cov.GetExcludedCount())
	}
	reasons := map[string]bool{}
	for _, e := range msg.GetCoverage().GetExclusions() {
		reasons[e.GetReason()] = true
	}
	for _, want := range []string{SkipNoMaintenanceMargin, SkipNoMarginRatio} {
		if !reasons[want] {
			t.Errorf("coverage does not name %q — the gap is recorded as a COUNT with no cause", want)
		}
	}
}

// TestAFruitlessObservationIsStillPublished: "we asked and the exchange told us
// nothing" is a different operational state from this feed being silent, and the
// two are indistinguishable if a fruitless poll publishes nothing.
func TestAFruitlessObservationIsStillPublished(t *testing.T) {
	pub := &capturePub{}
	r := reporterOver(&fakeSource{obs: execution.VenueMargin{ObservedAt: observed}}, pub)
	if err := r.Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(pub.events) != 1 {
		t.Errorf("published %d events for an all-UNKNOWN observation, want 1 — silence would be "+
			"indistinguishable from the poller not running", len(pub.events))
	}
}

// TestPositionWithoutALiquidationPriceIsNamedNotDropped: a position silently
// absent from the list cannot be told apart from a position that does not exist,
// and it is exactly the one whose liquidation distance nobody can see.
func TestPositionWithoutALiquidationPriceIsNamedNotDropped(t *testing.T) {
	obs := full()
	obs.Positions = append(obs.Positions, execution.VenuePositionMargin{Symbol: "ETH-USDT-SWAP"})
	pub := &capturePub{}
	if err := reporterOver(&fakeSource{obs: obs}, pub).Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	msg := onlyState(t, pub)

	if len(msg.GetLiquidationPrices()) != 1 {
		t.Fatalf("liquidation prices = %d, want 1 — a position with no price must not be carried at zero",
			len(msg.GetLiquidationPrices()))
	}
	var named bool
	for _, e := range msg.GetCoverage().GetExclusions() {
		if e.GetInstrumentId() == "ETH-USDT-SWAP" && e.GetReason() == SkipNoLiquidationPrice {
			named = true
		}
	}
	if !named {
		t.Error("the position with no liquidation price is not named in coverage — it was dropped silently")
	}
}

// TestAHugeFigureIsScaledNotWrapped (#94). dec.ToProto wraps once the scaled
// coefficient passes an int64 — about 92.2 billion units at scale 8 — and a
// crypto account's valuation currency reaches that. A wrapped maintenance margin
// does not report a SMALLER requirement; it reports a different one, and it can
// turn an account about to be liquidated into one that looks comfortable. This
// pins WHICH conversion is used, and fails if someone "simplifies"
// ToProtoScaled back to ToProto.
func TestAHugeFigureIsScaledNotWrapped(t *testing.T) {
	huge := rat(t, "123456789012345678901234567890")
	obs := full()
	obs.MaintenanceMargin = huge
	pub := &capturePub{}
	if err := reporterOver(&fakeSource{obs: obs}, pub).Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	msg := onlyState(t, pub)

	got := dec.FromProto(msg.GetMaintenanceMargin())
	if got.Sign() <= 0 {
		t.Fatalf("maintenance margin came back %s — a wrapped coefficient, not a scaled one", got.FloatString(0))
	}
	// The right MAGNITUDE within rounding: ToProtoScaled sheds significant digits,
	// it does not change the number's size.
	diff := new(big.Rat).Sub(got, huge)
	diff.Abs(diff)
	tol := new(big.Rat).Quo(huge, rat(t, "1000000"))
	if diff.Cmp(tol) > 0 {
		t.Errorf("maintenance margin = %s, want approximately %s", got.FloatString(0), huge.FloatString(0))
	}
	// And the wrapped rendering is genuinely different, or this proves nothing
	// about which conversion was used.
	if wrapped := dec.FromProto(dec.ToProto(huge)); wrapped.Cmp(got) == 0 {
		t.Fatal("ToProto and ToProtoScaled agree on this input — the test no longer distinguishes them")
	}
}

// TestUndatedObservationIsRefusedAtPublish: stamping now() would make a figure
// of unknown age look freshly observed, and the freshness bound would never fire.
func TestUndatedObservationIsRefusedAtPublish(t *testing.T) {
	obs := full()
	obs.ObservedAt = time.Time{}
	pub := &capturePub{}
	err := reporterOver(&fakeSource{obs: obs}, pub).Report(context.Background())

	if !errors.Is(err, ErrUndated) {
		t.Errorf("Report err = %v, want ErrUndated", err)
	}
	if len(pub.events) != 0 {
		t.Errorf("published %d events for an undated observation, want 0", len(pub.events))
	}
}

// TestUnattributedReporterRefuses: an unattributed margin figure cannot be bound
// to the portfolio it gates, and the account IS the liquidation boundary.
func TestUnattributedReporterRefuses(t *testing.T) {
	pub := &capturePub{}
	r := reporterOver(&fakeSource{obs: full()}, pub, func(c *ReporterConfig) { c.Account = "" })
	if err := r.Report(context.Background()); err == nil {
		t.Error("a reporter with no account published margin state anyway")
	}
	if len(pub.events) != 0 {
		t.Errorf("published %d events, want 0", len(pub.events))
	}
}

// TestSourceErrorPublishesNothing: a failed observation must not become a
// published claim. It ages the account out to UNKNOWN, which fails closed.
func TestSourceErrorPublishesNothing(t *testing.T) {
	pub := &capturePub{}
	boom := errors.New("venue unreachable")
	err := reporterOver(&fakeSource{err: boom}, pub).Report(context.Background())
	if !errors.Is(err, boom) {
		t.Errorf("Report err = %v, want the source's error", err)
	}
	if len(pub.events) != 0 {
		t.Errorf("published %d events after a failed observation, want 0", len(pub.events))
	}
}

// TestNilSourceReportsNothing: the composition root's Announce owns the "no
// margin source" warning; a second one here would give an operator two
// half-answers to one question.
func TestNilSourceReportsNothing(t *testing.T) {
	if r := NewReporter(ReporterConfig{Pub: &capturePub{}, Venue: "OKX", Account: "a"}); r != nil {
		t.Fatal("NewReporter returned a reporter with no source")
	}
	var r *Reporter
	if err := r.Report(context.Background()); err != nil {
		t.Errorf("nil Reporter.Report = %v, want nil", err)
	}
}

// TestReportedObservationRoundTripsIntoTheView closes the loop the two halves
// are otherwise tested apart from: a reporter that published a field the view
// keys differently would leave both suites green and the control blind.
func TestReportedObservationRoundTripsIntoTheView(t *testing.T) {
	pub := &capturePub{}
	if err := reporterOver(&fakeSource{obs: full()}, pub).Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	v := New(WithClock(frozen))
	fold(t, v, onlyState(t, pub))

	q, ok := v.MaintenanceMargin("OKX", "acct-1")
	if !ok || q.Value().Cmp(rat(t, "1250.5")) != 0 {
		t.Errorf("round trip: maintenance margin = %v ok=%v, want 1250.5 true", q.Value(), ok)
	}
	if _, ok := v.LiquidationPrice("OKX", "acct-1", "BTC-USDT-SWAP"); !ok {
		t.Error("round trip: the liquidation price the reporter published is UNKNOWN in the view")
	}
}
