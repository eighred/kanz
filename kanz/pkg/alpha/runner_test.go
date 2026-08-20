package alpha

import (
	"context"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"

	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/internal/marketedge/trades"
	"github.com/eighred/kanz/internal/signal/translate"
	"github.com/eighred/kanz/pkg/bus"
)

// The native-alpha boundary: an engine's decision must reach the venues as real
// order COMMANDS through the same path a TradingView signal takes — stamped with
// NATIVE_ENGINE provenance, sized exactly, and never through a private fan-out.

type capture struct {
	mu     sync.Mutex
	events []bus.Event
}

func (c *capture) Publish(_ context.Context, e bus.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *capture) byType(t string) []bus.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []bus.Event
	for _, e := range c.events {
		if e.EventType == t {
			out = append(out, e)
		}
	}
	return out
}

// staticEngine fires one fixed intent on its first Evaluate, then goes quiet —
// the stand-in for the restricted layer, which Kanz never sees inside.
type staticEngine struct {
	mu     sync.Mutex
	intent Intent
	fired  bool
	sawN   int
}

func (e *staticEngine) Name() string { return "test-engine" }

func (e *staticEngine) Evaluate(_ context.Context, views []MarketView) []Intent {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sawN = len(views)
	if e.fired {
		return nil
	}
	e.fired = true
	return []Intent{e.intent}
}

func (e *staticEngine) views() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sawN
}

// alphaTestAuthority binds the test engine (whose Name() is the strategy id when
// an Intent carries none) to fund-alpha, owned by tenant acme.
func alphaTestAuthority(t *testing.T) translate.FundAuthority {
	t.Helper()
	a, err := translate.NewFundAuthority(
		map[string]string{"fund-alpha": "acme"},
		map[string][]string{"test-engine": {"fund-alpha"}, "obi-v1": {"fund-alpha"}},
	)
	if err != nil {
		t.Fatalf("NewFundAuthority: %v", err)
	}
	return a
}

func testIntent() Intent {
	return Intent{
		FundID: "fund-alpha", InstrumentID: "BTC-USD",
		Action:      signalpb.SignalAction_SIGNAL_ACTION_BUY,
		Size:        big.NewRat(2, 1),
		SizeType:    signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY,
		OrderType:   orderpb.OrderType_ORDER_TYPE_MARKET,
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_IOC,
		Nonce:       "tick-1",
	}
}

func runnerWith(t *testing.T, eng Engine, feeds []Feed) (*Runner, *capture) {
	t.Helper()
	return runnerWithGate(t, eng, feeds, translate.OpenGate(nil))
}

func runnerWithGate(t *testing.T, eng Engine, feeds []Feed, gate *translate.Gate) (*Runner, *capture) {
	t.Helper()
	cap := &capture{}
	r, err := New(Config{
		Feeds:     feeds,
		Engines:   []Engine{eng},
		Prices:    translate.StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    translate.StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: translate.StaticPositions{},
		Alloc: translate.StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(1, 1)},
		}},
		Publisher: cap,
		Gate:      gate,
		// The engine's own fund binding (#632). A native engine's "strategy" is
		// in-process, so this is not an authentication decision — it is the statement
		// of whose capital that engine may commit, and of which tenant's book its
		// orders are routed onto. The tenant is deliberately not the fund id.
		Authority:        alphaTestAuthority(t),
		TickInterval:     5 * time.Millisecond,
		SnapshotInterval: time.Hour, // keep book snapshots out of this test's way
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, cap
}

// The autonomous loop fires every 100ms in production. A halted gate must stop it
// dead — and not merely discard its output: a halted system produces no decisions
// at all, so the engines are never even evaluated.
func TestRunner_HaltedGateSuppressesTheTickLoop(t *testing.T) {
	eng := &staticEngine{intent: testIntent()}
	gate := translate.NewGate(nil) // CLOSED — never opened by a lifecycle FACT
	r, cap := runnerWithGate(t, eng, []Feed{simFeed()}, gate)
	runBriefly(t, r)

	if n := len(cap.byType(translate.SubjectSubmit)); n != 0 {
		t.Fatalf("a halted alpha runner emitted %d order commands, want 0 — the kill-switch does not stop autonomous execution", n)
	}
	if n := len(cap.byType(translate.SubjectSignal)); n != 0 {
		t.Fatalf("a halted alpha runner recorded %d signal FACTs, want 0", n)
	}
	if eng.views() != 0 {
		t.Fatal("engines were evaluated while halted — a halted system must decide nothing, not decide and discard")
	}
}

// The gate is shared, so a mid-flight trip stops the loop that is already running.
func TestRunner_TripMidFlightStopsExecution(t *testing.T) {
	eng := &staticEngine{intent: testIntent()}
	gate := translate.OpenGate(nil)
	r, cap := runnerWithGate(t, eng, []Feed{simFeed()}, gate)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(20 * time.Millisecond)
		gate.TripOnBusLoss(nil) // the spine drops mid-session
	}()
	_ = r.Run(ctx)

	// staticEngine fires exactly once, so the pre-trip window may legitimately have
	// produced one order. What must NOT happen is the loop continuing to trade after
	// the trip — and the gate must still be latched shut at the end.
	if !gate.Halted() {
		t.Fatal("gate reopened after a mid-flight bus loss")
	}
	if n := len(cap.byType(translate.SubjectSubmit)); n > 1 {
		t.Fatalf("emitted %d commands, want <=1 — execution continued after the halt tripped", n)
	}
}

func simFeed() Feed {
	return Feed{
		InstrumentID: "BTC-USD", MIC: "SIM",
		Depth:  depth.NewSimSource(depth.SimConfig{InstrumentID: "BTC-USD", Symbol: "BTC-USD", Interval: time.Millisecond}),
		Trades: trades.NewSimSource(trades.SimConfig{Interval: time.Millisecond}),
	}
}

func runBriefly(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)
}

// An engine intent becomes the StrategySignal FACT (the audit root) AND the
// SubmitOrder commands the OMS actually executes. The FACT alone would trade
// nothing — nothing subscribes to it.
func TestRunner_EngineIntentReachesTheOMSAsCommands(t *testing.T) {
	eng := &staticEngine{intent: Intent{
		FundID: "fund-alpha", InstrumentID: "BTC-USD",
		Action:   signalpb.SignalAction_SIGNAL_ACTION_BUY,
		Size:     big.NewRat(2, 1),
		SizeType: signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY,
		// An arbitrage leg must not rest: IOC or the hedge silently becomes naked.
		OrderType:   orderpb.OrderType_ORDER_TYPE_MARKET,
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_IOC,
		Nonce:       "tick-1",
	}}
	r, cap := runnerWith(t, eng, []Feed{simFeed()})
	runBriefly(t, r)

	facts := cap.byType(translate.SubjectSignal)
	if len(facts) != 1 {
		t.Fatalf("StrategySignal FACTs = %d, want 1 (the audit root)", len(facts))
	}
	sig := facts[0].Payload.(*signalpb.StrategySignal)
	if sig.GetSource() != signalpb.SignalSource_SIGNAL_SOURCE_NATIVE_ENGINE {
		t.Fatalf("source = %v, want NATIVE_ENGINE — provenance must never be inferred", sig.GetSource())
	}
	if sig.GetStrategyId() != "test-engine" {
		t.Fatalf("strategy_id = %q, want the engine name (the audit traces to it)", sig.GetStrategyId())
	}
	if facts[0].EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Fatal("the signal must be a FACT, not a command")
	}

	cmds := cap.byType(translate.SubjectSubmit)
	if len(cmds) != 1 {
		t.Fatalf("SubmitOrder COMMANDs = %d, want 1 — a FACT alone executes nothing", len(cmds))
	}
	cmd := cmds[0].Payload.(*orderpb.SubmitOrder)
	if cmds[0].EventClass != envelopepb.EventClass_EVENT_CLASS_COMMAND {
		t.Fatal("the order must be a COMMAND the OMS gates")
	}
	if cmd.GetVenue() != "BINANCE" {
		t.Fatalf("venue = %q, want BINANCE (the allocated leg)", cmd.GetVenue())
	}
	if cmd.GetSide() != orderpb.Side_SIDE_BUY {
		t.Fatalf("side = %v, want BUY", cmd.GetSide())
	}
	// IOC must survive the translation — a resting arb leg is naked exposure.
	if cmd.GetTimeInForce() != orderpb.TimeInForce_TIME_IN_FORCE_IOC {
		t.Fatalf("time_in_force = %v, want IOC (an arb leg must never rest)", cmd.GetTimeInForce())
	}
	// The issuer principal is what the OMS attributes the order to.
	if got := cmd.GetMetadata().GetIssuer(); got != "strategy:test-engine" {
		t.Fatalf("issuer = %q, want strategy:test-engine", got)
	}
	// Sizing is exact: 2 BTC absolute × weight 1.
	if got := cmd.GetQuantity(); got.GetCoefficient() != 200000000 || got.GetExponent() != -8 {
		t.Fatalf("quantity = %v, want exactly 2", got)
	}
	if cmds[0].IdempotencyKey == "" || cmds[0].IdempotencyKey != cmd.GetOrderId() {
		t.Fatal("the command must carry its deterministic order_id as the idempotency key")
	}
}

// The engine sees ONE view per (instrument, venue) — never a merged book. Merging
// venues would destroy exactly the spread a cross-venue engine exists to see.
func TestRunner_OneViewPerVenueNeverMerged(t *testing.T) {
	eng := &staticEngine{intent: Intent{
		FundID: "fund-alpha", InstrumentID: "BTC-USD",
		Action: signalpb.SignalAction_SIGNAL_ACTION_BUY,
		Size:   big.NewRat(1, 1), SizeType: signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY,
		OrderType: orderpb.OrderType_ORDER_TYPE_MARKET, Nonce: "n1",
	}}
	binance := simFeed()
	binance.MIC = "BINANCE"
	okx := simFeed()
	okx.MIC = "OKX"

	r, _ := runnerWith(t, eng, []Feed{binance, okx})
	runBriefly(t, r)

	if got := eng.views(); got != 2 {
		t.Fatalf("engine saw %d views, want 2 (one per venue, unmerged)", got)
	}
	mics := map[string]bool{}
	for _, v := range r.Views() {
		mics[v.MIC()] = true
		if v.InstrumentID() != "BTC-USD" {
			t.Fatalf("view instrument = %q, want BTC-USD", v.InstrumentID())
		}
	}
	if !mics["BINANCE"] || !mics["OKX"] {
		t.Fatalf("views cover %v, want both BINANCE and OKX separately", mics)
	}
}

// An intent with no nonce cannot dedup, so it must be refused rather than risk
// double-trading the same decision on a later tick.
func TestRunner_IntentWithoutNonceIsRefused(t *testing.T) {
	eng := &staticEngine{intent: Intent{
		FundID: "fund-alpha", InstrumentID: "BTC-USD",
		Action: signalpb.SignalAction_SIGNAL_ACTION_BUY,
		Size:   big.NewRat(1, 1), SizeType: signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY,
		OrderType: orderpb.OrderType_ORDER_TYPE_MARKET,
		// Nonce deliberately empty.
	}}
	r, cap := runnerWith(t, eng, []Feed{simFeed()})
	runBriefly(t, r)

	if n := len(cap.byType(translate.SubjectSubmit)); n != 0 {
		t.Fatalf("emitted %d commands for a nonce-less intent, want 0 (it could not dedup)", n)
	}
}

// Kanz's OWN binary registers no engines: it folds and snapshots and decides
// nothing. It must not need the signal path wired at all.
func TestRunner_NoEnginesNeedsNoSignalPath(t *testing.T) {
	cap := &capture{}
	r, err := New(Config{
		Feeds:            []Feed{simFeed()},
		Publisher:        cap,
		SnapshotInterval: 10 * time.Millisecond,
		SnapshotDepth:    5,
		// No Engines, and deliberately no Prices/Equity/Positions/Alloc.
	})
	if err != nil {
		t.Fatalf("New with no engines = %v, want nil (the open edge wires no signal path)", err)
	}
	runBriefly(t, r)

	if n := len(cap.byType(translate.SubjectSubmit)); n != 0 {
		t.Fatalf("an engine-less edge emitted %d commands, want 0", n)
	}
	// It still does its real job: folding depth and publishing bounded snapshots.
	if len(cap.byType("market.book.snapshot")) == 0 {
		t.Fatal("the open edge must still publish bounded book snapshots")
	}
}

// AN ENGINE-DRIVEN RUNNER WITH NO FUND BINDING MUST NOT CONSTRUCT (#632).
//
// pkg/alpha shared the webhook path's exposure through the identical seam: its
// old `TenantOf func(fundID) string` went straight to translate.Options, whose
// nil default made the fund id the tenant. No shipped binary reaches it —
// market-ingest registers no engines — so this is the guard that keeps the
// restricted layer from inheriting the repaired defect the moment it supplies one.
func TestRunner_EnginesWithoutAFundBindingFailFast(t *testing.T) {
	_, err := New(Config{
		Feeds:     []Feed{simFeed()},
		Engines:   []Engine{&staticEngine{}},
		Publisher: &capture{},
		Prices:    translate.StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    translate.StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: translate.StaticPositions{},
		Alloc: translate.StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(1, 1)},
		}},
		Gate: translate.OpenGate(nil),
		// Authority deliberately absent — everything else is wired.
	})
	if err == nil {
		t.Fatal("New accepted engines with no FundAuthority. Every signal this runner emitted " +
			"would take its tenant from the fund id, which is #632 in the second brain.")
	}
	if !strings.Contains(err.Error(), "FundAuthority") {
		t.Errorf("error %q does not name the missing seam", err)
	}
}

// Registering an engine without the signal path wired must fail at CONSTRUCTION,
// not at the first live signal.
func TestRunner_EnginesWithoutSignalPathFailFast(t *testing.T) {
	_, err := New(Config{
		Feeds:     []Feed{simFeed()},
		Engines:   []Engine{&staticEngine{}},
		Publisher: &capture{},
		// Prices/Equity/Positions/Alloc missing.
	})
	if err == nil {
		t.Fatal("New = nil error — an engine with no signal path would fail at the first trade")
	}
}
