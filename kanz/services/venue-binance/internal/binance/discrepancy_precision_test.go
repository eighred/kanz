package binance

import (
	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"math/big"
	"testing"
	"time"
)

func TestDiscrepancyPrecisionAndInvalidQuantity(t *testing.T) {
	for _, raw := range []string{"", "garbage", "-1", "1e1000000"} {
		if _, err := binanceHealedFromQuery(CloseIntent{OrderID: "o"}, &orderResponse{ExecutedQty: raw, Status: "FILLED"}, "BINANCE", time.Now()); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	state, err := binanceHealedFromQuery(CloseIntent{OrderID: "o"}, &orderResponse{ExecutedQty: "0.000000001", Status: "FILLED"}, "BINANCE", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if dec.FromProto(state.FilledQuantity).Cmp(big.NewRat(1, 1000000000)) != 0 {
		t.Fatal("rounded fill evidence")
	}
	cap := &reconCapture{}
	r := newReconciler(ReconcilerConfig{Pub: cap, Venue: "BINANCE", Tenant: "t"})
	if err := r.emitBalanceReconciled(t.Context(), "USD", big.NewRat(0, 1), big.NewRat(1, 1000000000)); err != nil {
		t.Fatal(err)
	}
	msg := cap.events[0].Payload.(*accountingpb.BalanceReconciled)
	if dec.FromProto(msg.Actual).Cmp(big.NewRat(1, 1000000000)) != 0 || dec.FromProto(msg.Delta).Cmp(big.NewRat(1, 1000000000)) != 0 {
		t.Fatal("rounded balance evidence")
	}
	if err := r.emitBalanceReconciled(t.Context(), "USD", big.NewRat(0, 1), big.NewRat(1, 3)); err == nil || len(cap.events) != 1 {
		t.Fatal("published unrepresentable balance")
	}
}
