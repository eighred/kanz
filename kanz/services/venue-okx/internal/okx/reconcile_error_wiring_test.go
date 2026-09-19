package okx

import (
	"context"
	"errors"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"math/big"
	"testing"
	"time"
)

func TestStartForwardsReconcileErrorObserver(t *testing.T) {
	f := newFakeOKX(t)
	f.balanceBody = `{"code":"0","data":[{"details":[{"ccy":"USD","cashBal":"invalid"}]}]}`
	c := NewOKXConnector(VenueSettings{MIC: "OKX", BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p", Symbols: map[string]string{"BTC-USD": "BTC-USDT"}, WeightBudget: 1200}, "", exchangeauth.OKXDemo)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := make(chan error, 1)
	c.Start(ctx, WorkerDeps{Publisher: &okxCapture{}, Expected: okxStaticOrders{}, Balances: okxStaticBalances{"USD": big.NewRat(0, 1)}, Tenant: "test", TickerInterval: time.Hour, ReconcileInterval: time.Millisecond,
		OnReconcileError: func(_ context.Context, loop string, err error) {
			if err != nil {
				cancel()
				select {
				case seen <- err:
				default:
				}
			}
		}})
	select {
	case err := <-seen:
		if !errors.Is(err, execution.ErrReconcileEvidence) {
			t.Fatalf("lost typed evidence failure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connector dropped observer")
	}
}
