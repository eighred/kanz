package ingest

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/signal/translate"
)

// #240 at the perimeter — the one surface the internet talks to. The unit tests in
// internal/signal/translate pin the translator; these drive the whole authenticated
// path so the HTTP-facing sentinel (ErrBadRequest ⇒ 400) is proved too.

// cappedHarness is the issue's scenario verbatim: a cap of 10 written by an
// operator who meant "10 BTC", a $1,000,000,000 NAV, and a $50,000 mark.
func cappedHarness(t *testing.T, max *big.Rat) (*Pipeline, *capture) {
	t.Helper()
	cap := &capture{}
	auth := NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now)
	p, err := NewPipeline(Options{
		Auth:        auth,
		Symbols:     StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:      StaticPrices{"BTC-USD": big.NewRat(50_000, 1)},
		Equity:      StaticEquity{"fund-alpha": new(big.Rat).SetInt64(1_000_000_000)},
		Positions:   StaticPositions{},
		Alloc:       StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher:   cap,
		Gate:        translate.OpenGate(nil),
		MaxQuantity: NewQty(max),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, cap
}

// The exact failure #240 documents: max_quantity=10, size=9 as a percentage of a
// $1e9 NAV at $50k. Before the fix this answered 202 and fanned out 1,800 BTC of
// live SubmitOrder commands — 180x the operator's cap.
func TestPerimeter_PctOfEquityCannotWalkPastAQuantityCap(t *testing.T) {
	p, cap := cappedHarness(t, big.NewRat(10, 1))
	_, err := process(t, p, body("buy", "9", "pct_of_equity", "cap-1"))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("size=9 pct_of_equity against a cap of 10 = %v, want ErrBadRequest (400). "+
			"Unrefused it fans out 1800 BTC", err)
	}
	if n := len(cap.commands()); n != 0 {
		t.Fatalf("a refused oversize alert still produced %d live order commands", n)
	}
	if cap.signal() != nil {
		t.Fatal("a refused oversize alert recorded a StrategySignal FACT with no orders behind it")
	}
}

// The same cap must still admit what it was written to admit, and must still bite
// in its own unit — otherwise "the cap works" is unfalsifiable.
func TestPerimeter_CapBitesInTheResolvedUnit(t *testing.T) {
	p, cap := cappedHarness(t, big.NewRat(10, 1))
	if _, err := process(t, p, body("buy", "9", "absolute_qty", "cap-2")); err != nil {
		t.Fatalf("size=9 absolute_qty under a cap of 10 = %v, want accepted", err)
	}
	cmds := cap.commands()
	if len(cmds) != 1 || dec.FromProto(cmds[0].GetQuantity()).Cmp(big.NewRat(9, 1)) != 0 {
		t.Fatalf("commands = %v, want one leg of 9", cmds)
	}
	if _, err := process(t, p, body("buy", "11", "absolute_qty", "cap-3")); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("size=11 absolute_qty against a cap of 10 = %v, want ErrBadRequest", err)
	}
}

// leverage was accepted, bounds-checked, written to the immutable FACT, and then
// dropped — the order placed was spot. It is now refused at the edge.
//
// NOTE: this REJECTS webhooks that previously answered 202. They "succeeded" by
// silently mis-executing.
func TestPerimeter_LeverageIsRefusedNotDropped(t *testing.T) {
	p, cap := cappedHarness(t, nil)
	// The `ts` matters MORE on a refusal fixture than on an accepting one. Without
	// it, #416's mandatory-timestamp rule refuses this alert before the leverage
	// check is ever reached — the assertion below still passes, on the wrong
	// refusal, and would keep passing if the leverage guard were deleted outright.
	raw := `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
		`"action":"buy","size":"100000","size_type":"quote_notional","leverage":"10",` +
		`"margin_mode":"cross","nonce":"lev-1",` + freshTS()
	if _, err := process(t, p, raw); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("leverage=10 = %v, want ErrBadRequest — accepting it submits an UNLEVERED order "+
			"while the audit root asserts 10x", err)
	}
	if n := len(cap.events); n != 0 {
		t.Fatalf("a refused levered alert published %d events, want 0", n)
	}
}

func TestPerimeter_MarginModeAloneIsRefused(t *testing.T) {
	p, _ := cappedHarness(t, nil)
	// Stamped for the same reason as the leverage fixture above: an unstamped alert
	// is refused before margin_mode is looked at, so the assertion would hold
	// against a build with no margin_mode check at all.
	raw := `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
		`"action":"buy","size":"1","size_type":"absolute_qty","margin_mode":"isolated","nonce":"mm-1",` + freshTS()
	if _, err := process(t, p, raw); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("margin_mode=isolated = %v, want ErrBadRequest — SubmitOrder carries no margin "+
			"mode, so the order placed is spot while the FACT claims margin", err)
	}
}

// The spellings that still work: leverage omitted, leverage "1", margin_mode
// omitted / "spot" / "none". A refusal that also refuses ordinary spot alerts
// would take the whole surface down.
func TestPerimeter_UnleveredSpotStillTrades(t *testing.T) {
	for i, raw := range []string{
		`{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
			`"action":"buy","size":"1","size_type":"absolute_qty","nonce":"ok-0",` + freshTS(),
		`{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
			`"action":"buy","size":"1","size_type":"absolute_qty","leverage":"1","nonce":"ok-1",` + freshTS(),
		`{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
			`"action":"buy","size":"1","size_type":"absolute_qty","leverage":"1.0",` +
			`"margin_mode":"spot","nonce":"ok-2",` + freshTS(),
		`{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
			`"action":"buy","size":"1","size_type":"absolute_qty","margin_mode":"none","nonce":"ok-3",` + freshTS(),
	} {
		p, cap := cappedHarness(t, nil)
		if _, err := process(t, p, raw); err != nil {
			t.Fatalf("case %d: unlevered spot alert = %v, want accepted", i, err)
		}
		if len(cap.commands()) != 1 {
			t.Fatalf("case %d: got %d commands, want 1", i, len(cap.commands()))
		}
	}
}

// A refused alert is a VERDICT, so the nonce burns and a redelivery is a replay —
// not a second chance to place the same bad order.
func TestPerimeter_ARefusedAlertBurnsItsNonce(t *testing.T) {
	p, _ := cappedHarness(t, big.NewRat(10, 1))
	raw := body("buy", "9", "pct_of_equity", "burn-1")
	if _, err := process(t, p, raw); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("first = %v, want ErrBadRequest", err)
	}
	if _, err := process(t, p, raw); !errors.Is(err, ErrReplayed) {
		t.Fatalf("redelivery of a refused alert = %v, want ErrReplayed", err)
	}
}

// The composition root cannot wire a fund whose weights are not a split: the
// pipeline refuses to construct, so the process exits instead of scaling every
// order it ever places.
func TestNewPipeline_RefusesWholePercentWeights(t *testing.T) {
	_, err := NewPipeline(Options{
		Auth:      NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now),
		Symbols:   StaticSymbols{},
		Prices:    StaticPrices{},
		Equity:    StaticEquity{},
		Positions: StaticPositions{},
		Alloc: StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(60, 1)},
			{Venue: "OKX", Weight: big.NewRat(40, 1)},
		}},
		Publisher: &capture{},
		Gate:      translate.OpenGate(nil),
	})
	if !errors.Is(err, ErrBadAllocation) {
		t.Fatalf("NewPipeline with 60/40 weights = %v, want ErrBadAllocation — it would fan a "+
			"2 BTC signal out as 120 + 80 BTC", err)
	}
}

// A bound may only be compared against a resolved quantity. This is a compile-time
// property of translate.Qty, not a runtime check — the assertion here is that the
// unset bound is genuinely unbounded rather than a bound of zero, which is the one
// way the type could still refuse every order.
func TestQty_UnsetBoundIsUnbounded(t *testing.T) {
	if NewQty(nil).IsSet() {
		t.Fatal("NewQty(nil) is set — an absent bound would become a bound of zero")
	}
	p, cap := cappedHarness(t, nil)
	if _, err := process(t, p, body("buy", "1000000", "absolute_qty", "unbounded-1")); err != nil {
		t.Fatalf("unbounded pipeline refused a large order: %v", err)
	}
	if len(cap.commands()) != 1 {
		t.Fatalf("got %d commands, want 1", len(cap.commands()))
	}
}
