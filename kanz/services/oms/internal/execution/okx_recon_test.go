//go:build okx

package execution

import (
	"context"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/kanz-eng/kanz-schemas-go/accounting/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/services/oms/internal/dec"
)

type okxStaticOrders []*orderpb.OrderState

func (s okxStaticOrders) OpenOrders() []*orderpb.OrderState { return s }

type okxStaticBalances map[string]*big.Rat

func (b okxStaticBalances) Balance(asset string) *big.Rat { return b[asset] }

func okxReconOver(f *fakeOKX, cap *okxCapture, exp okxStaticOrders, bal okxStaticBalances) *OKXReconciler {
	bucket := newWeightBucket(60, time.Minute, nil)
	rest := newOKXREST(okxRestConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p", Bucket: bucket})
	var balances ExpectedBalances // true nil interface when no balances configured
	if bal != nil {
		balances = bal
	}
	return newOKXReconciler(OKXReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"},
		Expected: exp, Balances: balances, Pub: cap, Venue: "OKX", Tenant: "fund-alpha",
	})
}

// Kanz believes an order is partially filled at 1; OKX says accFillSz 2 (filled)
// — reconciliation emits a StateHealed correcting FACT carrying the venue truth.
func TestOKXRecon_HealsDriftedOrder(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"2","avgPx":"50000"}]}`
	cap := &okxCapture{}
	exp := okxStaticOrders{{
		OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: odec("2"), FilledQuantity: odec("1"),
		Status: orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED,
	}}
	r := okxReconOver(f, cap, exp, nil)

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

func TestOKXRecon_NoDriftNoHeal(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"2","avgPx":"50000"}]}`
	cap := &okxCapture{}
	exp := okxStaticOrders{{
		OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		OrderedQuantity: odec("2"), FilledQuantity: odec("2"),
		Status: orderpb.OrderStatus_ORDER_STATUS_FILLED,
	}}
	r := okxReconOver(f, cap, exp, nil)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(cap.events) != 0 {
		t.Fatalf("in-parity order produced %d FACTs, want 0", len(cap.events))
	}
}

// OKX reports a cash balance Kanz's ledger doesn't match — reconciliation emits
// BalanceReconciled with the exact delta.
func TestOKXRecon_ReconcilesBalance(t *testing.T) {
	f := newFakeOKX(t)
	f.balanceBody = `{"code":"0","msg":"","data":[{"details":[{"ccy":"BTC","cashBal":"1.5"},{"ccy":"USDT","cashBal":"1000"}]}]}`
	cap := &okxCapture{}
	// Kanz thinks BTC=1 (an unrecorded 0.5 fill); USDT matches at 1000.
	r := okxReconOver(f, cap, okxStaticOrders{}, okxStaticBalances{"BTC": big.NewRat(1, 1), "USDT": big.NewRat(1000, 1)})

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
