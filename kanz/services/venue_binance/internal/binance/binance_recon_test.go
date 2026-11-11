package binance

import (
	"context"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/bus"
)

type reconCapture struct{ events []bus.Event }

func (c *reconCapture) Publish(_ context.Context, e bus.Event) error {
	c.events = append(c.events, e)
	return nil
}

type staticOrders []*orderpb.OrderState

func (s staticOrders) OpenOrders() []*orderpb.OrderState { return s }

type staticBalances map[string]*big.Rat

// A missing key is UNKNOWN, not zero (#418) — the distinction the reconciler
// now depends on, so the double must make it too. It names a REASON for the
// unknown (#1063) for the same argument: a fixture that answered "" would prove
// the callback fires without proving the label an operator is paged on is real.
func (b staticBalances) Balance(asset string) (*big.Rat, bool, string) {
	v, ok := b[asset]
	if !ok {
		return nil, false, execution.BalanceUnknownNeverAnnounced
	}
	return v, true, ""
}

func reconOver(f *fakeBinance, cap *reconCapture, exp staticOrders, bal staticBalances) *Reconciler {
	bucket := newWeightBucket(1200, time.Minute, nil)
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
	var balances ExpectedBalances // true nil interface when no balances configured
	if bal != nil {
		balances = bal
	}
	return newReconciler(ReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"},
		Expected: exp, Balances: balances, Pub: cap, Venue: "BINANCE", Tenant: "fund-alpha",
	})
}

// Kanz believes an order is filled 1; Binance says 2 — reconciliation emits a
// StateHealed correcting FACT (never edits state), carrying the venue truth.
func TestRecon_HealsDriftedOrder(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"FILLED","executedQty":"2.00000000"}`
	cap := &reconCapture{}
	exp := staticOrders{{
		OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: bdec("2"), FilledQuantity: bdec("1"),
		Status: orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED,
	}}
	r := reconOver(f, cap, exp, nil)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var healed *orderpb.StateHealed
	for _, e := range cap.events {
		if h, ok := e.Payload.(*orderpb.StateHealed); ok {
			healed = h
		}
	}
	if healed == nil {
		t.Fatal("no StateHealed emitted for the drifted order")
	}
	if got := dec.FromProto(healed.GetState().GetFilledQuantity()); got.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("healed filled = %s, want 2 (venue truth)", got.RatString())
	}
	if healed.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("healed status = %v, want FILLED", healed.GetState().GetStatus())
	}
	if healed.GetState().GetPortfolioId() != "fund-alpha" {
		t.Fatal("healed state lost Kanz's account context")
	}
}

func TestRecon_NoDriftNoHeal(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"FILLED","executedQty":"2.00000000"}`
	cap := &reconCapture{}
	exp := staticOrders{{
		OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		OrderedQuantity: bdec("2"), FilledQuantity: bdec("2"),
		Status: orderpb.OrderStatus_ORDER_STATUS_FILLED,
	}}
	r := reconOver(f, cap, exp, nil)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(cap.events) != 0 {
		t.Fatalf("in-parity order produced %d FACTs, want 0", len(cap.events))
	}
}

// Binance reports a balance Kanz's ledger doesn't match — reconciliation emits
// BalanceReconciled with the exact delta.
func TestRecon_ReconcilesBalance(t *testing.T) {
	f := newFakeBinance(t)
	f.accountBody = `{"balances":[{"asset":"BTC","free":"1.5","locked":"0"},{"asset":"USDT","free":"1000","locked":"0"}]}`
	cap := &reconCapture{}
	// Kanz thinks BTC=1 (an unrecorded 0.5 fill); USDT matches at 1000.
	r := reconOver(f, cap, staticOrders{}, staticBalances{"BTC": big.NewRat(1, 1), "USDT": big.NewRat(1000, 1)})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var recon *accountingpb.BalanceReconciled
	for _, e := range cap.events {
		if b, ok := e.Payload.(*accountingpb.BalanceReconciled); ok {
			recon = b
		}
	}
	if recon == nil {
		t.Fatal("no BalanceReconciled emitted for the BTC drift")
	}
	if recon.GetAsset() != "BTC" {
		t.Fatalf("reconciled asset = %s, want BTC (USDT matched, no FACT)", recon.GetAsset())
	}
	if got := dec.FromProto(recon.GetDelta()); got.Cmp(big.NewRat(1, 2)) != 0 { // 1.5 - 1.0
		t.Fatalf("delta = %s, want 0.5", got.RatString())
	}
}
