package ingest

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/signal/translate"
)

// THE OWNER'S CASE, THROUGH THE REAL PERIMETER (#416): a TradingView alert
// delayed thirty minutes by a webhook retry, arriving with a valid HMAC and a
// fresh nonce, and executing at full size because nothing compared its own
// timestamp to now.
//
// These drive Pipeline.Process — the same path the HTTP server calls — so the
// refusal is proven where it has to happen, not only in the translator.
func agedHarness(t *testing.T, max time.Duration, now time.Time, allowUnstamped bool) (*Pipeline, *capture) {
	t.Helper()
	cap := &capture{}
	p, err := NewPipeline(Options{
		Authority:            ingestTestAuthority(t),
		Auth:                 NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, func() time.Time { return now }),
		Symbols:              StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:               StaticPrices{"BTC-USD": big.NewRat(50_000, 1)},
		Equity:               StaticEquity{"fund-alpha": new(big.Rat).SetInt64(1_000_000)},
		Positions:            StaticPositions{},
		Alloc:                StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher:            cap,
		Gate:                 translate.OpenGate(nil),
		MaxSignalAge:         max,
		AllowUnstampedSignal: allowUnstamped,
		Now:                  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, cap
}

var alertFired = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

func stampedBody(nonce string, ts time.Time) string {
	return `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
		`"action":"buy","size":"1","size_type":"absolute_qty","nonce":"` + nonce + `",` +
		`"ts":"` + ts.Format(time.RFC3339) + `"}`
}

func TestPerimeter_AThirtyMinuteOldAlertIsRefusedAndPublishesNothing(t *testing.T) {
	p, cap := agedHarness(t, 2*time.Minute, alertFired.Add(30*time.Minute), false)

	_, err := p.Process(t.Context(), []byte(stampedBody("stale-1", alertFired)),
		nil, sign(stampedBody("stale-1", alertFired), testSecret))
	if err == nil {
		t.Fatal("a 30-minute-old alert was ACCEPTED at the perimeter. It fans out live orders at " +
			"the size the strategy chose for a price that has since moved.")
	}
	if !errors.Is(err, translate.ErrStaleSignal) {
		t.Errorf("err = %v, want ErrStaleSignal", err)
	}
	// NOTHING ON THE BUS. A refusal that still published the FACT would leave an
	// audit root asserting the platform acted on a signal it refused.
	if n := len(cap.commands()); n != 0 {
		t.Fatalf("%d order command(s) reached the bus for a refused alert", n)
	}
}

// A FRESH ALERT STILL TRADES. Without this the test above is satisfied by a
// perimeter that refuses everything — a trading outage wearing the shape of a fix.
func TestPerimeter_AFreshAlertStillFansOut(t *testing.T) {
	p, cap := agedHarness(t, 2*time.Minute, alertFired.Add(5*time.Second), false)

	raw := stampedBody("fresh-1", alertFired)
	if _, err := p.Process(t.Context(), []byte(raw), nil, sign(raw, testSecret)); err != nil {
		t.Fatalf("a 5-second-old alert was refused: %v", err)
	}
	if n := len(cap.commands()); n == 0 {
		t.Fatal("a fresh alert produced no orders")
	}
}

// AN ALERT WITH NO ts IS REFUSED, and is counted anyway — counted REGARDLESS of
// the outcome, because the operator's question is the same either way: which
// sender is misconfigured? The refusal reaches the sender in a 400; the counter
// is what reaches us.
func TestPerimeter_AnUnstampedAlertIsRefusedAndCounted(t *testing.T) {
	var counted []string
	cap := &capture{}
	now := alertFired
	p, err := NewPipeline(Options{
		Authority:         ingestTestAuthority(t),
		Auth:              NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, func() time.Time { return now }),
		Symbols:           StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:            StaticPrices{"BTC-USD": big.NewRat(50_000, 1)},
		Equity:            StaticEquity{"fund-alpha": new(big.Rat).SetInt64(1_000_000)},
		Positions:         StaticPositions{},
		Alloc:             StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher:         cap,
		Gate:              translate.OpenGate(nil),
		MaxSignalAge:      2 * time.Minute,
		Now:               func() time.Time { return now },
		OnUnstampedSignal: func(strategyID string) { counted = append(counted, strategyID) },
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	raw := unstampedBody("buy", "1", "absolute_qty", "nots-1")
	_, perr := p.Process(t.Context(), []byte(raw), nil, sign(raw, testSecret))
	if !errors.Is(perr, translate.ErrStaleSignal) {
		t.Fatalf("an alert with no ts = %v, want ErrStaleSignal. Its age cannot be established, "+
			"so the bound has nothing to judge.", perr)
	}
	if n := len(cap.commands()); n != 0 {
		t.Fatalf("%d command(s) published for an alert with no timestamp", n)
	}
	if len(counted) != 1 || counted[0] != "momentum" {
		t.Fatalf("unstamped alerts counted = %v, want [momentum] — an unjudgeable alert that is "+
			"accepted must not also be silent, or nobody can name the templates to fix", counted)
	}
}

// A STALE ALERT IS A VERDICT: 400, AND THE NONCE BURNS.
//
// It answered 502 Bad Gateway. ErrStaleSignal was the one refusal mapTranslateErr
// did not re-map, so it fell to the server's default arm — the one reserved for
// "a real backend fault". Three things followed, and the third is the expensive
// one:
//
//	It blamed us. A misconfigured sender's alert read as kanz being broken, and
//	  502 is what an operator pages on. The template that needs fixing is the
//	  sender's.
//	IT IS RETRYABLE, AND 4xx IS NOT. Every webhook sender and proxy retries a 5xx.
//	  So the alerts that retry hardest are exactly the delayed ones — the ones
//	  already too old to act on — and each retry is refused and retried again.
//	THE NONCE DID NOT BURN, so nothing collapsed that storm either: `decided`
//	  admits only ErrHalted and ErrBadRequest, so each redelivery was released and
//	  re-entered the whole pipeline instead of answering ErrReplayed.
//
// A halt already burns the nonce for the reason given on `decided`: an alert that
// fired before a halt is stale once it clears, and re-firing it into a moved
// market is worse than dropping it. A STALE ALERT IS THAT ARGUMENT DIRECTLY — it
// is not "we could not decide", it is "we decided, and the answer is no".
func TestPerimeter_AStaleAlertIs400AndBurnsItsNonce(t *testing.T) {
	p, cap := agedHarness(t, 2*time.Minute, alertFired.Add(30*time.Minute), false)
	raw := stampedBody("stale-verdict-1", alertFired)

	_, err := p.Process(t.Context(), []byte(raw), nil, sign(raw, testSecret))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a 30-minute-old alert = %v, want ErrBadRequest so the server answers 400. "+
			"Unmapped it answers 502, which blames this platform for the sender's clock AND "+
			"invites the retry storm a 4xx would end.", err)
	}
	// The specific reason must SURVIVE the re-map, or the operator loses the one
	// thing that names the fault. Both sentinels, one error.
	if !errors.Is(err, translate.ErrStaleSignal) {
		t.Errorf("err = %v, want ErrStaleSignal to remain wrapped alongside ErrBadRequest", err)
	}
	if n := len(cap.commands()); n != 0 {
		t.Fatalf("%d order command(s) reached the bus for a refused alert", n)
	}
	// THE VERDICT STICKS. A redelivery of a refused alert is a replay, not a second
	// chance — the same rule TestPerimeter_ARefusedAlertBurnsItsNonce pins for the
	// other 400s.
	if _, err := p.Process(t.Context(), []byte(raw), nil, sign(raw, testSecret)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("redelivery of a stale alert = %v, want ErrReplayed. Released instead of burnt, "+
			"a retrying sender re-enters the full pipeline on every delivery.", err)
	}
}

// An alert with NO ts is the same verdict by the same route — it is refused with
// ErrStaleSignal too, so it took the same 502.
func TestPerimeter_AnUnstampedAlertIs400(t *testing.T) {
	p, _ := agedHarness(t, 2*time.Minute, alertFired, false)
	raw := unstampedBody("buy", "1", "absolute_qty", "nots-400")

	_, err := p.Process(t.Context(), []byte(raw), nil, sign(raw, testSecret))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("an alert with no ts = %v, want ErrBadRequest (400). This is the refusal #416 "+
			"introduced and the one every misconfigured template will hit on its first alert; "+
			"answering 502 tells that sender to retry forever.", err)
	}
}

// A MALFORMED ts IS A 400, NOT A SILENT nil (#416). It used to parse to nil,
// which made "sent nothing" and "sent garbage" identical one layer up — and
// under a freshness bound that is a bypass with a clear message attached.
func TestPerimeter_AMalformedTimestampIsRefused(t *testing.T) {
	p, cap := agedHarness(t, 2*time.Minute, alertFired, false)

	raw := `{"strategy_id":"momentum","fund_id":"fund-alpha","symbol":"BINANCE:BTCUSDT",` +
		`"action":"buy","size":"1","size_type":"absolute_qty","nonce":"bad-ts","ts":"not-a-time"}`
	_, err := p.Process(t.Context(), []byte(raw), nil, sign(raw, testSecret))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a malformed ts = %v, want ErrBadRequest (400)", err)
	}
	if n := len(cap.commands()); n != 0 {
		t.Fatalf("%d command(s) published for an alert with an unparseable ts", n)
	}
}

// A PIPELINE BUILT WITHOUT THE FIELD IS STILL BOUNDED (#416).
//
// This is the assertion that protects the DEFAULT, and nothing else in this file
// covers it: every other test sets MaxSignalAge explicitly, so removing the
// default in NewPipeline leaves them all green. A binary that wires this
// pipeline and forgets the option would otherwise run unbounded — and an
// unbounded deployment is the one this issue is about.
func TestPerimeter_APipelineThatWasNotToldTheBoundIsStillBounded(t *testing.T) {
	cap := &capture{}
	now := alertFired.Add(30 * time.Minute)
	p, err := NewPipeline(Options{
		Authority: ingestTestAuthority(t),
		Auth:      NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, func() time.Time { return now }),
		Symbols:   StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"},
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50_000, 1)},
		Equity:    StaticEquity{"fund-alpha": new(big.Rat).SetInt64(1_000_000)},
		Positions: StaticPositions{},
		Alloc:     StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: cap,
		Gate:      translate.OpenGate(nil),
		Now:       func() time.Time { return now },
		// MaxSignalAge deliberately UNSET.
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	raw := stampedBody("default-1", alertFired)
	_, perr := p.Process(t.Context(), []byte(raw), nil, sign(raw, testSecret))
	if !errors.Is(perr, translate.ErrStaleSignal) {
		t.Fatalf("a 30-minute-old alert through a pipeline with no configured bound = %v, want "+
			"ErrStaleSignal. Zero must mean \"not set\" and take the default, never \"disabled\" — "+
			"otherwise forgetting one field silently restores the bug.", perr)
	}
	if n := len(cap.commands()); n != 0 {
		t.Fatalf("%d command(s) published", n)
	}
}
