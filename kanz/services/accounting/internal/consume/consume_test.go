package consume

import (
	"context"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
)

func dec(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

func filledPayload(t *testing.T, portfolioID, fillID, instrument string, side orderpb.Side, qty, price *commonpb.Decimal, executed time.Time) []byte {
	t.Helper()
	ev := &orderpb.OrderFilled{
		State: &orderpb.OrderState{PortfolioId: portfolioID},
		Fill: &orderpb.Fill{
			FillId:       fillID,
			InstrumentId: instrument,
			Side:         side,
			Quantity:     qty,
			Price:        price,
			ExecutedAt:   timestamppb.New(executed),
		},
	}
	b, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestFolderFoldsFillIntoLedger(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, err := NewFolder(st, "USD")
	if err != nil {
		t.Fatalf("new folder: %v", err)
	}
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	env := &envelopepb.Envelope{
		EventType:     orderEventFilled,
		IngestionTime: timestamppb.New(t0),
	}

	// BUY 100 @ 150 → +100 position, -15000 cash.
	payload := filledPayload(t, "PORT-1", "F1", "AAPL", orderpb.Side_SIDE_BUY, dec(100, 0), dec(150, 0), t0)
	if err := f.Handle(ctx, env, payload); err != nil {
		t.Fatalf("handle buy: %v", err)
	}
	// Redelivery is idempotent (Append dedups on the entry id).
	if err := f.Handle(ctx, env, payload); err != nil {
		t.Fatalf("handle redelivery: %v", err)
	}

	book, err := ledger.MaterializeCurrent(ctx, st, "PORT-1")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := book.Positions["AAPL"]; got == nil || got.Qty.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("position qty = %v, want 100 (idempotent)", got)
	}
	if got := book.CashBalance("USD"); got.Cmp(big.NewRat(-15000, 1)) != 0 {
		t.Fatalf("cash = %v, want -15000", got)
	}
}

func TestFolderIgnoresNonFillEvent(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(st, "USD")
	env := &envelopepb.Envelope{EventType: "order.order.created"}
	if err := f.Handle(context.Background(), env, []byte("anything")); err != nil {
		t.Fatalf("non-fill event should ack: %v", err)
	}
	if j, _ := st.Journal(context.Background(), "PORT-1"); len(j) != 0 {
		t.Fatalf("non-fill event wrote %d entries, want 0", len(j))
	}
}

func TestFolderRejectsMalformedFill(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(st, "USD")
	env := &envelopepb.Envelope{EventType: orderEventFilled}
	if err := f.Handle(context.Background(), env, []byte("not-a-proto")); err == nil {
		t.Fatal("malformed fill should surface an error (DLQ), not ack")
	}
}
