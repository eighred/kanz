package ingest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	signalpb "github.com/kanz-eng/kanz-schemas-go/signal/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/signal/translate"
	"github.com/eighred/kanz/pkg/bus"
)

const testSecret = "topsecret"

// capture records every published event; Payload is the concrete proto already,
// so assertions type-assert it directly.
type capture struct{ events []bus.Event }

func (c *capture) Publish(_ context.Context, e bus.Event) error {
	c.events = append(c.events, e)
	return nil
}

func (c *capture) commands() []*orderpb.SubmitOrder {
	var out []*orderpb.SubmitOrder
	for _, e := range c.events {
		if cmd, ok := e.Payload.(*orderpb.SubmitOrder); ok {
			out = append(out, cmd)
		}
	}
	return out
}

func (c *capture) signal() *signalpb.StrategySignal {
	for _, e := range c.events {
		if s, ok := e.Payload.(*signalpb.StrategySignal); ok {
			return s
		}
	}
	return nil
}

func sign(body, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

// harness builds a pipeline over a two-venue fund with static price/equity.
func harness(t *testing.T) (*Pipeline, *capture) {
	t.Helper()
	cap := &capture{}
	auth := NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now)
	p, err := NewPipeline(Options{
		Auth:    auth,
		Symbols: StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:  StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:  StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{
			"fund-alpha/BINANCE/BTC-USD": big.NewRat(3, 10), // long 0.3 for CLOSE test
			"fund-alpha/OKX/BTC-USD":     big.NewRat(2, 10), // long 0.2
		},
		Alloc: StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(6, 10)},
			{Venue: "OKX", Weight: big.NewRat(4, 10)},
		}},
		Publisher: cap,
		Gate:      translate.OpenGate(nil),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, cap
}

func body(action, size, sizeType, nonce string) string {
	return `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
		`"action":"` + action + `","size":"` + size + `","size_type":"` + sizeType + `","nonce":"` + nonce + `"}`
}

func process(t *testing.T, p *Pipeline, raw string) (*Result, error) {
	t.Helper()
	return p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
}

func TestFanOut_AbsoluteQty(t *testing.T) {
	p, cap := harness(t)
	res, err := process(t, p, body("buy", "1", "absolute_qty", "n1"))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	cmds := cap.commands()
	if len(cmds) != 2 || len(res.OrderIDs) != 2 {
		t.Fatalf("want 2 fanned-out orders, got %d", len(cmds))
	}
	// The signal FACT was recorded as the audit root.
	if sig := cap.signal(); sig == nil || sig.GetSizeType() != signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY {
		t.Fatalf("signal FACT missing/wrong: %+v", cap.signal())
	}
	// Base qty 1 split 0.6 / 0.4 by venue weight; both BUY.
	for _, c := range cmds {
		if c.GetSide() != orderpb.Side_SIDE_BUY {
			t.Errorf("side = %v, want BUY", c.GetSide())
		}
		if c.GetMetadata().GetIssuer() != "strategy:momentum" {
			t.Errorf("issuer = %q, want strategy:momentum", c.GetMetadata().GetIssuer())
		}
	}
	assertSplit(t, cmds, big.NewRat(6, 10), big.NewRat(4, 10))
}

func TestFanOut_StampsAllocatedVenue(t *testing.T) {
	p, cap := harness(t)
	if _, err := process(t, p, body("buy", "1", "absolute_qty", "venue-1")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	venues := map[string]bool{}
	for _, c := range cap.commands() {
		venues[c.GetVenue()] = true
	}
	// Each leg carries its allocated venue so the OMS router sends it there (M4).
	if !venues["BINANCE"] || !venues["OKX"] || len(venues) != 2 {
		t.Fatalf("fanned venues = %v, want {BINANCE, OKX}", venues)
	}
}

func TestFanOut_QuoteNotional(t *testing.T) {
	p, cap := harness(t)
	// 5000 USDT notional / 50000 price = 0.1 base → 0.06 / 0.04.
	if _, err := process(t, p, body("buy", "5000", "quote_notional", "n2")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	assertSplit(t, cap.commands(), big.NewRat(6, 100), big.NewRat(4, 100))
}

func TestFanOut_PctOfEquity(t *testing.T) {
	p, cap := harness(t)
	// 5% of 1,000,000 NAV = 50,000 notional / 50,000 price = 1 base → 0.6 / 0.4.
	if _, err := process(t, p, body("sell", "5", "pct_of_equity", "n3")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	cmds := cap.commands()
	for _, c := range cmds {
		if c.GetSide() != orderpb.Side_SIDE_SELL {
			t.Errorf("side = %v, want SELL", c.GetSide())
		}
	}
	assertSplit(t, cmds, big.NewRat(6, 10), big.NewRat(4, 10))
}

func TestClose_FlattensPerVenuePosition(t *testing.T) {
	p, cap := harness(t)
	// CLOSE ignores size; each venue sells its own long position (0.3 / 0.2).
	if _, err := process(t, p, body("close", "1", "absolute_qty", "n4")); err != nil {
		t.Fatalf("Process: %v", err)
	}
	cmds := cap.commands()
	if len(cmds) != 2 {
		t.Fatalf("want 2 closing orders, got %d", len(cmds))
	}
	for _, c := range cmds {
		if c.GetSide() != orderpb.Side_SIDE_SELL { // long → sell to flatten
			t.Errorf("close side = %v, want SELL", c.GetSide())
		}
	}
	assertSplit(t, cmds, big.NewRat(3, 10), big.NewRat(2, 10))
}

func TestReplayIsDropped(t *testing.T) {
	p, _ := harness(t)
	raw := body("buy", "1", "absolute_qty", "dup")
	if _, err := process(t, p, raw); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := process(t, p, raw); !errors.Is(err, ErrReplayed) {
		t.Fatalf("second = %v, want ErrReplayed", err)
	}
}

func TestBadSignatureRejected(t *testing.T) {
	p, cap := harness(t)
	raw := body("buy", "1", "absolute_qty", "n5")
	_, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, "wrong-secret"))
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if len(cap.events) != 0 {
		t.Fatal("a rejected webhook produced events")
	}
}

// The kill-switch at the perimeter: a halted gate rejects an otherwise perfectly
// valid, correctly-signed webhook, and NOTHING reaches the bus — no signal FACT,
// no order commands.
func TestHaltedGateRejectsValidWebhook(t *testing.T) {
	cap := &capture{}
	auth := NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now)
	gate := translate.NewGate(nil) // CLOSED — no lifecycle FACT has opened it
	p, err := NewPipeline(Options{
		Auth: auth, Symbols: StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{}, Alloc: StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: cap,
		Gate:      gate,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	raw := body("buy", "1", "absolute_qty", "halt-1")
	_, err = p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if !errors.Is(err, ErrHalted) {
		t.Fatalf("halted pipeline accepted a webhook: err = %v, want ErrHalted", err)
	}
	if n := len(cap.events); n != 0 {
		t.Fatalf("a halted pipeline published %d events, want 0 — the kill-switch let traffic through", n)
	}

	// Once an operator opens the gate, traffic flows again: the brake stops trading,
	// it does not permanently break the service.
	//
	// Note the fresh nonce. Authenticate runs BEFORE the gate check, so the rejected
	// webhook already burned "halt-1" into the replay cache — a TradingView retry of
	// that same alert after the resume would be dropped as a replay. That is the
	// behaviour we want (an alert that fired during a halt is stale by the time the
	// halt clears, and re-firing it into a moved market is worse than dropping it),
	// but it is load-bearing enough to pin down here.
	gate.Resume("operator:akif", "cleared")
	fresh := body("buy", "1", "absolute_qty", "halt-2")
	if _, err := p.Process(context.Background(), []byte(fresh), net.ParseIP("10.0.0.1"), sign(fresh, testSecret)); err != nil {
		t.Fatalf("after operator resume, Process = %v, want nil", err)
	}
	if len(cap.commands()) == 0 {
		t.Fatal("after operator resume, no order commands were produced")
	}

	// The stale alert that arrived during the halt stays dropped.
	if _, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay of the halted alert = %v, want ErrReplayed", err)
	}
}

func TestIPAllowlistRejects(t *testing.T) {
	cap := &capture{}
	_, cidr, _ := net.ParseCIDR("203.0.113.0/24")
	auth := NewAuthenticator(StaticSecrets{"momentum": testSecret}, []*net.IPNet{cidr}, time.Minute, time.Now)
	p, err := NewPipeline(Options{
		Auth: auth, Symbols: StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices: StaticPrices{"BTC-USD": big.NewRat(50000, 1)}, Equity: StaticEquity{},
		Positions: StaticPositions{}, Alloc: StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: cap,
		Gate:      translate.OpenGate(nil),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	raw := body("buy", "1", "absolute_qty", "n6")
	_, err = p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("off-allowlist IP = %v, want ErrUnauthorized", err)
	}
}

func TestUnknownSymbolRejected(t *testing.T) {
	p, _ := harness(t)
	raw := `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"NASDAQ:AAPL","action":"buy","size":"1","size_type":"absolute_qty","nonce":"n7"}`
	_, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown symbol = %v, want ErrBadRequest", err)
	}
}

func TestUnmappedFundDenied(t *testing.T) {
	p, _ := harness(t)
	raw := `{"strategy_id":"momentum","fund_id":"ghost-fund","symbol":"BINANCE:BTCUSDT","action":"buy","size":"1","size_type":"absolute_qty","nonce":"n8"}`
	_, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if !errors.Is(err, ErrNoAllocation) {
		t.Fatalf("unmapped fund = %v, want ErrNoAllocation", err)
	}
}

func TestDeterministicOrderIDs(t *testing.T) {
	p, cap := harness(t)
	if _, err := process(t, p, body("buy", "1", "absolute_qty", "same")); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range cap.commands() {
		if ids[c.GetOrderId()] {
			t.Fatal("duplicate order id across venues — fan-out must be distinct")
		}
		ids[c.GetOrderId()] = true
		// order_id is the envelope idempotency key so a re-fan dedups.
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 distinct order ids, got %d", len(ids))
	}
}

// assertSplit checks the two fanned-out commands carry quantities {a, b} in some
// order (the map iteration over venues is unordered).
func assertSplit(t *testing.T, cmds []*orderpb.SubmitOrder, a, b *big.Rat) {
	t.Helper()
	if len(cmds) != 2 {
		t.Fatalf("want 2 commands, got %d", len(cmds))
	}
	q0 := dec.FromProto(cmds[0].GetQuantity())
	q1 := dec.FromProto(cmds[1].GetQuantity())
	ok := (q0.Cmp(a) == 0 && q1.Cmp(b) == 0) || (q0.Cmp(b) == 0 && q1.Cmp(a) == 0)
	if !ok {
		t.Fatalf("split = {%s, %s}, want {%s, %s}", q0.RatString(), q1.RatString(), a.RatString(), b.RatString())
	}
}
