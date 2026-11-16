package binance

import (
	"context"
	"errors"
	"github.com/eighred/kanz/internal/execution"
	"math/big"
	"testing"
	"time"
)

func TestStartForwardsReconcileErrorObserver(t *testing.T) {
	f := newFakeBinance(t)
	f.accountBody = `{"balances":[{"asset":"USD","free":"invalid","locked":"0"}]}`
	c := NewBinanceConnector(VenueSettings{MIC: "BINANCE", BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Symbols: map[string]string{"BTC-USD": "BTCUSDT"}, WeightBudget: 1200}, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := make(chan error, 1)
	c.Start(ctx, WorkerDeps{Publisher: &reconCapture{}, Expected: staticOrders{}, Balances: staticBalances{"USD": big.NewRat(0, 1)}, Tenant: "test", TickerInterval: time.Hour, ReconcileInterval: time.Millisecond,
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
