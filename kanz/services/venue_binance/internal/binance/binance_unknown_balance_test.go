package binance

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
)

// unknownCapture records what the connector's reconciler reported it could not
// check. A slice rather than a count, because the reason is the half that was
// missing and asserting only "something fired" would pass on an empty label.
type unknownCapture struct {
	mu   sync.Mutex
	seen []string // "asset/reason"
}

func (c *unknownCapture) observe(asset, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, asset+"/"+reason)
}

func (c *unknownCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// unknownBalances answers UNKNOWN for everything, naming the reason.
type unknownBalances struct{ reason string }

func (b unknownBalances) Balance(string) (*big.Rat, bool, string) { return nil, false, b.reason }

// THE COMPOSITION PATH IS WHAT BROKE, SO THE COMPOSITION PATH IS WHAT IS TESTED
// (#1063).
//
// The reconciler has honoured OnUnknownBalance since #418 and its own unit tests
// have always passed. What nobody wired was the connector: BinanceConnector.Start
// built a ReconcilerConfig that never named the field, and the composition root
// had no field to name. Go zero-fills an omitted field in a composite literal, so
// there was no compile error and no log line — the callback was simply nil and
// r.unknownBalance did nothing.
//
// A test that constructs a Reconciler directly cannot see that: it names the
// field itself and proves the seam the connector was not using. So this drives
// Start with the exact WorkerDeps the composition root supplies and asserts the
// signal arrives at the other end.
func TestStartWiresTheUnknownBalanceSeamThroughToTheReconciler(t *testing.T) {
	f := newFakeBinance(t)
	// The exchange holds USDT. Kanz has never been told what this account holds.
	f.accountBody = `{"balances":[{"asset":"USDT","free":"1000.00","locked":"0.00"}]}`
	capt := &reconCapture{}
	unknown := &unknownCapture{}

	c := NewBinanceConnector(VenueSettings{
		MIC: "BINANCE", BaseURL: f.srv.URL, APIKey: "k", APISecret: "s",
		Symbols: map[string]string{"BTC-USD": "BTCUSDT"}, WeightBudget: 1200,
	}, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, WorkerDeps{
		Publisher:         capt,
		Expected:          staticOrders{},
		Balances:          unknownBalances{reason: execution.BalanceUnknownNeverAnnounced},
		OnUnknownBalance:  unknown.observe,
		Tenant:            "fund-alpha",
		ReconcileInterval: 10 * time.Millisecond,
		// An hour, so the mark feed does not publish into the capture and turn this
		// into an assertion about the ticker.
		TickerInterval: time.Hour,
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(unknown.snapshot()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	got := unknown.snapshot()
	if len(got) == 0 {
		t.Fatal("the reconciliation ran over an asset whose expected balance is UNKNOWN and " +
			"reported NOTHING — no FACT, no callback, no log. That is identical to an asset " +
			"that reconciled cleanly, on the last layer that can notice a mis-booked position. " +
			"BinanceConnector.Start is not passing WorkerDeps.OnUnknownBalance into the " +
			"ReconcilerConfig (#1063)")
	}
	if want := "USDT/" + execution.BalanceUnknownNeverAnnounced; got[0] != want {
		t.Fatalf("observed %q, want %q — the asset and the REASON both have to arrive: a "+
			"counter that cannot say whether the cash spine never started or has stopped "+
			"sends an operator to the wrong place", got[0], want)
	}
	// And no break was fabricated for it. A BalanceReconciled carrying expected=0
	// is the #418 fabrication, folded bitemporally into the book of record.
	if len(capt.events) != 0 {
		t.Fatalf("an unknown balance published %d FACT(s) — an unchecked asset must not be "+
			"reported as a break, because the break's expected figure would be invented",
			len(capt.events))
	}
}

// THE REASON IS NORMALISED AT THE RECONCILER, so a seam cannot widen a Prometheus
// label by returning a new string. An unbounded label is a per-value time series
// forever, and this one is reached from an exchange response.
func TestAnUnrecognisedUnknownReasonIsNamedRatherThanPassedThrough(t *testing.T) {
	f := newFakeBinance(t)
	f.accountBody = `{"balances":[{"asset":"USDT","free":"1.00","locked":"0.00"}]}`
	capt := &reconCapture{}
	unknown := &unknownCapture{}

	r := newReconciler(ReconcilerConfig{
		REST: newBinanceREST(restConfig{
			BaseURL: f.srv.URL, APIKey: "k", APISecret: "s",
			Bucket: newWeightBucket(1200, time.Minute, nil),
		}),
		Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"}, Expected: staticOrders{},
		Balances:         unknownBalances{reason: "whatever-this-seam-felt-like"},
		Pub:              capt,
		OnUnknownBalance: unknown.observe,
		Venue:            "BINANCE", Tenant: "fund-alpha",
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := unknown.snapshot()
	if len(got) != 1 {
		t.Fatalf("observed %v, want exactly one unchecked asset", got)
	}
	if want := "USDT/" + execution.BalanceUnknownUnattributed; got[0] != want {
		t.Fatalf("observed %q, want %q — an unrecognised reason must become the named "+
			"'the seam did not say' label, not reach Prometheus verbatim", got[0], want)
	}
}
