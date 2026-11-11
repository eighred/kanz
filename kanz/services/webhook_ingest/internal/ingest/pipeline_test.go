package ingest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/halt"
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

// ingestTestAuthority is the strategy→fund→tenant binding every pipeline in this
// package is built over (#632).
//
// `momentum` — the strategy whose secret the test authenticator holds — is bound
// to fund-alpha only. `fund-victim` belongs to a DIFFERENT tenant and no strategy
// here may trade it; that is the attacker's target in
// TestPerimeter_ASignedAlertForAnotherTenantsFundIsForbidden.
//
// The tenants are deliberately NOT the fund ids. Before this change the tenant WAS
// the fund id, so a test that used one string for both would pass whether or not
// the defect was fixed.
func ingestTestAuthority(t *testing.T) FundAuthority {
	t.Helper()
	a, err := NewFundAuthority(
		map[string]string{"fund-alpha": "acme", "fund-victim": "globex"},
		map[string][]string{"momentum": {"fund-alpha"}},
	)
	if err != nil {
		t.Fatalf("NewFundAuthority: %v", err)
	}
	return a
}

// A PIPELINE WITH NO BINDING TABLE MUST NOT CONSTRUCT (#632).
//
// This is the state the estate was actually in: the tenant seam existed, no
// composition root assigned it, and the nil default made every deployment look
// configured while routing orders by a field of the request body. There is no
// default now — the internet-facing binary refuses to start.
func TestNewPipelineRefusesWithoutAFundAuthority(t *testing.T) {
	_, err := NewPipeline(Options{
		Auth:      NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now),
		Symbols:   StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{},
		Alloc:     StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: &capture{},
		Gate:      halt.OpenGate(nil),
		// Authority deliberately absent — everything else is wired, which is exactly
		// how this shipped.
	})
	if err == nil {
		t.Fatal("NewPipeline accepted a nil FundAuthority. The perimeter cannot say whose capital " +
			"a signal trades, and the tenant falls back to whatever the caller put in fund_id.")
	}
	// THIS LAYER'S OWN REFUSAL, not the translator's. translate.New would also
	// reject a nil Authority, so an assertion on the word "FundAuthority" alone
	// passes whether or not this check exists — measured, by deleting it. The
	// perimeter states the operational fact only IT knows: the thing this binary
	// authenticated is a strategy.
	if !strings.Contains(err.Error(), "the HMAC authenticates the") {
		t.Errorf("error %q is not the perimeter's own refusal — an operator reading a startup "+
			"failure on the internet-facing pod needs to be told what the HMAC actually proved, "+
			"which is the whole of #632", err)
	}
}

// harness builds a pipeline over a two-venue fund with static price/equity.
func harness(t *testing.T) (*Pipeline, *capture) {
	t.Helper()
	cap := &capture{}
	auth := NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now)
	p, err := NewPipeline(Options{
		Authority: ingestTestAuthority(t),
		Auth:      auth,
		Symbols:   StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{
			"fund-alpha/BINANCE/BTC-USD": big.NewRat(3, 10), // long 0.3 for CLOSE test
			"fund-alpha/OKX/BTC-USD":     big.NewRat(2, 10), // long 0.2
		},
		Alloc: StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(6, 10)},
			{Venue: "OKX", Weight: big.NewRat(4, 10)},
		}},
		Publisher: cap,
		Gate:      halt.OpenGate(nil),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, cap
}

// body builds a well-formed alert.
//
// IT CARRIES A FRESH `ts`, because #416 made that field mandatory: an alert with
// no timestamp has no age, the freshness bound cannot judge it, and it is
// refused. Every test using this helper is about something else — a size cap, a
// nonce, a fan-out — so each needs an alert that is otherwise beyond reproach.
// unstampedBody below is for the tests that are about the missing field itself.
func body(action, size, sizeType, nonce string) string {
	return stampedAt(action, size, sizeType, nonce, time.Now().UTC())
}

func stampedAt(action, size, sizeType, nonce string, ts time.Time) string {
	return `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
		`"action":"` + action + `","size":"` + size + `","size_type":"` + sizeType + `","nonce":"` + nonce + `",` +
		`"ts":"` + ts.Format(time.RFC3339) + `"}`
}

// freshTS closes a hand-written alert literal with a current timestamp. #416
// made `ts` mandatory, so a fixture written before it is now refused for the
// missing field rather than exercising whatever it was written to test.
func freshTS() string {
	return `"ts":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
}

// unstampedBody omits `ts` entirely — the shape the platform now refuses.
func unstampedBody(action, size, sizeType, nonce string) string {
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
	gate := halt.NewGate(nil) // CLOSED — no lifecycle FACT has opened it
	p, err := NewPipeline(Options{
		Authority: ingestTestAuthority(t),
		Auth:      auth, Symbols: StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
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
		Authority: ingestTestAuthority(t),
		Auth:      auth, Symbols: StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices: StaticPrices{"BTC-USD": big.NewRat(50000, 1)}, Equity: StaticEquity{},
		Positions: StaticPositions{}, Alloc: StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: cap,
		Gate:      halt.OpenGate(nil),
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
	// Stamped so the refusal below is the SYMBOL's. Unstamped, #416 refuses this
	// with the same ErrBadRequest and the assertion cannot tell the two apart.
	raw := `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"NASDAQ:AAPL","action":"buy","size":"1","size_type":"absolute_qty","nonce":"n7",` + freshTS()
	_, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown symbol = %v, want ErrBadRequest", err)
	}
}

// A FUND THIS STRATEGY IS NOT BOUND TO IS REFUSED BEFORE THE ALLOCATION IS EVEN
// CONSULTED (#632).
//
// This test used to expect ErrNoAllocation, and that ordering was the whole
// exposure: the ALLOCATION table was what bounded the blast radius, so any fund
// the deployment happened to serve — including other tenants' — was reachable
// with one strategy's secret, and only a fund NOBODY had configured was refused.
// The binding is now the first substantive check, so an unknown fund and another
// tenant's fund are one answer.
func TestUnboundFundDenied(t *testing.T) {
	p, cap := harness(t)
	raw := `{"strategy_id":"momentum","fund_id":"ghost-fund","symbol":"BINANCE:BTCUSDT","action":"buy","size":"1","size_type":"absolute_qty","nonce":"n8",` + freshTS()
	_, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if !errors.Is(err, ErrUnboundFund) {
		t.Fatalf("unbound fund = %v, want ErrUnboundFund", err)
	}
	if len(cap.events) != 0 {
		t.Fatalf("%d event(s) published for an unbound fund", len(cap.events))
	}
}

// AND THE SAME ANSWER FOR ANOTHER TENANT'S FUND, which the deployment serves in
// full. `fund-victim` is in the binding table under tenant globex; momentum has
// no claim on it. If this returned anything other than the refusal above, the
// response would be a fund-id oracle.
func TestAnotherTenantsFundIsRefusedIdentically(t *testing.T) {
	p, cap := harness(t)
	raw := `{"strategy_id":"momentum","fund_id":"fund-victim","symbol":"BINANCE:BTCUSDT","action":"buy","size":"1","size_type":"absolute_qty","nonce":"n8b",` + freshTS()
	_, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if !errors.Is(err, ErrUnboundFund) {
		t.Fatalf("another tenant's fund = %v, want ErrUnboundFund — one strategy secret reached "+
			"another tenant's book", err)
	}
	if len(cap.events) != 0 {
		t.Fatalf("%d event(s) published for another tenant's fund", len(cap.events))
	}
}

// AN UNMAPPED FUND IS A VERDICT: 400, AND THE NONCE BURNS.
//
// The sentinel above was pinned and its STATUS was not, so this answered 502 Bad
// Gateway — the same gap ErrStaleSignal had. `fund_id` is a field the SENDER
// chose, and deny-by-default means an unmapped one can never trade: no retry of
// the identical body resolves differently, because Alloc is static config read at
// startup.
//
// So 5xx is wrong twice over. It tells a sender with a typo'd fund that this
// platform is broken, and it tells them to try again — forever, since the nonce
// was released rather than burnt and each redelivery re-entered the whole
// pipeline instead of answering ErrReplayed.
//
// It burns for the reason written on `decided`: this is a deliberate refusal, not
// a failure to reach one. If the allocation was dropped by mistake rather than by
// intent, the alert is stale by the time anyone notices and #416's freshness
// bound would refuse it on the retry anyway — the identical argument the halt
// case already makes.
func TestPerimeter_AnUnmappedFundIs400AndBurnsItsNonce(t *testing.T) {
	// A BOUND FUND WITH NO ALLOCATION. Since #632 the binding is checked first, so
	// an UNKNOWN fund never reaches the allocation table at all — the sentinel this
	// test pins is now only reachable for a fund the strategy IS entitled to whose
	// venue split is missing, which is what this harness constructs. (config.Load
	// cannot produce that state: ValidateAllocation refuses an empty split at
	// startup. The seam is still reachable from a dynamic AllocationPolicy and from
	// the native alpha path, and its STATUS is what this pins.)
	cap := &capture{}
	authority, err := NewFundAuthority(
		map[string]string{"fund-alpha": "acme", "fund-unallocated": "acme"},
		map[string][]string{"momentum": {"fund-alpha", "fund-unallocated"}},
	)
	if err != nil {
		t.Fatalf("NewFundAuthority: %v", err)
	}
	p, err := NewPipeline(Options{
		Authority: authority,
		Auth:      NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now),
		Symbols:   StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    StaticEquity{"fund-unallocated": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{},
		Alloc:     StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: cap,
		Gate:      halt.OpenGate(nil),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	raw := `{"strategy_id":"momentum","fund_id":"fund-unallocated","symbol":"BINANCE:BTCUSDT","action":"buy","size":"1","size_type":"absolute_qty","nonce":"ghost-400",` + freshTS()

	_, err = p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("an unmapped fund = %v, want ErrBadRequest so the server answers 400. Unmapped it "+
			"answers 502, which blames this platform for the sender's fund_id and invites a retry "+
			"that can never succeed.", err)
	}
	// The reason must survive the re-map, or the operator cannot tell an unmapped
	// fund from any other 400.
	if !errors.Is(err, ErrNoAllocation) {
		t.Errorf("err = %v, want ErrNoAllocation to remain wrapped alongside ErrBadRequest", err)
	}
	if n := len(cap.commands()); n != 0 {
		t.Fatalf("%d order command(s) reached the bus for an unmapped fund", n)
	}
	if _, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("redelivery for an unmapped fund = %v, want ErrReplayed", err)
	}
}

// AN UNBOUND FUND ALSO BURNS ITS NONCE (#632). The binding table is static config
// read at startup, so no redelivery of the identical body can ever resolve
// differently — releasing it would let a prober re-enter the whole pipeline on
// every retry instead of collapsing to ErrReplayed.
func TestPerimeter_AnUnboundFundBurnsItsNonceAndCountsTheStrategy(t *testing.T) {
	cap := &capture{}
	var counted []string
	p, err := NewPipeline(Options{
		Authority: ingestTestAuthority(t),
		Auth:      NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now),
		Symbols:   StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    StaticEquity{"fund-victim": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{},
		Alloc:     StaticAllocation{"fund-victim": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: cap,
		Gate:      halt.OpenGate(nil),
		OnUnboundFund: func(strategyID string) {
			counted = append(counted, strategyID)
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	raw := `{"strategy_id":"momentum","fund_id":"fund-victim","symbol":"BINANCE:BTCUSDT","action":"buy","size":"1","size_type":"absolute_qty","nonce":"unbound-burn",` + freshTS()

	if _, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret)); !errors.Is(err, ErrUnboundFund) {
		t.Fatalf("first attempt = %v, want ErrUnboundFund", err)
	}
	// COUNTED, BY STRATEGY. A refusal nobody can see is a leaked secret nobody finds.
	if len(counted) != 1 || counted[0] != "momentum" {
		t.Errorf("OnUnboundFund saw %v, want exactly [momentum] — the operator's question is "+
			"WHICH SENDER is reaching for a fund it does not own", counted)
	}
	if _, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("redelivery = %v, want ErrReplayed — the nonce was released, so a prober can "+
			"re-enter the whole pipeline on every retry", err)
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
