package okx

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

// okxUnknownCapture records what the connector's reconciler reported it could not
// check. A slice rather than a count, because the reason is the half that was
// missing and asserting only "something fired" would pass on an empty label.
type okxUnknownCapture struct {
	mu   sync.Mutex
	seen []string // "asset/reason"
}

func (c *okxUnknownCapture) observe(asset, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, asset+"/"+reason)
}

func (c *okxUnknownCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// okxUnknownBalances answers UNKNOWN for everything, naming the reason.
type okxUnknownBalances struct{ reason string }

func (b okxUnknownBalances) Balance(string) (*big.Rat, bool, string) { return nil, false, b.reason }

// THE COMPOSITION PATH IS WHAT BROKE, SO THE COMPOSITION PATH IS WHAT IS TESTED
// (#1063) — the OKX half of the identical defect.
//
// OKXReconciler has honoured OnUnknownBalance since #418 and OKXConnector.Start
// built an OKXReconcilerConfig that never named it. An omitted field in a keyed
// composite literal is zero-filled with no error anywhere, so the callback was
// nil and the skip was silent — output identical to an asset that reconciled
// cleanly. A test that constructs the reconciler directly names the field itself
// and therefore cannot see the omission; this drives Start.
func TestStartWiresTheUnknownBalanceSeamThroughToTheOKXReconciler(t *testing.T) {
	f := newFakeOKX(t)
	// OKX holds USDT. Kanz has never been told what this account holds.
	f.balanceBody = `{"code":"0","msg":"","data":[{"details":[{"ccy":"USDT","cashBal":"1000"}]}]}`
	capt := &okxCapture{}
	unknown := &okxUnknownCapture{}

	c := NewOKXConnector(VenueSettings{
		MIC: "OKX", BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p",
		Symbols: map[string]string{"BTC-USD": "BTC-USDT"}, WeightBudget: 60,
	}, "", exchangeauth.OKXDemo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, WorkerDeps{
		Publisher:         capt,
		Expected:          okxStaticOrders{},
		Balances:          okxUnknownBalances{reason: execution.BalanceUnknownStale},
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
			"OKXConnector.Start is not passing WorkerDeps.OnUnknownBalance into the " +
			"OKXReconcilerConfig (#1063)")
	}
	if want := "USDT/" + execution.BalanceUnknownStale; got[0] != want {
		t.Fatalf("observed %q, want %q — the asset and the REASON both have to arrive: a "+
			"counter that cannot say whether the cash spine never started or has stopped "+
			"sends an operator to the wrong place", got[0], want)
	}
	if len(capt.events) != 0 {
		t.Fatalf("an unknown balance published %d FACT(s) — an unchecked asset must not be "+
			"reported as a break, because the break's expected figure would be invented",
			len(capt.events))
	}
}
