package consume

import (
	"context"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// testTenant is the tenant these folders serve. It is the SYSTEM tenant, so
// RequireTenantScope's shared-bucket branch applies and these tests exercise the
// folding logic exactly as they did before #223. The cross-tenant refusal itself
// is proven in cross_tenant_test.go, which uses a real tenant.
const testTenant = "__system__"

func decv(coef int64, exp int32) *commonpb.Decimal {
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
	f, err := NewFolder(testTenant, st, "USD")
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
	payload := filledPayload(t, "PORT-1", "F1", "AAPL", orderpb.Side_SIDE_BUY, decv(100, 0), decv(150, 0), t0)
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

func cashPayload(t *testing.T, entryID, portfolioID string, entryType accountingpb.EntryType, cash *commonpb.Decimal, ccy string, eff time.Time) []byte {
	t.Helper()
	le := &accountingpb.LedgerEntry{
		EntryId:       entryID,
		PortfolioId:   portfolioID,
		EntryType:     entryType,
		Cash:          cash,
		CashCurrency:  ccy,
		EffectiveTime: timestamppb.New(eff),
		KnowledgeTime: timestamppb.New(eff),
	}
	b, err := proto.Marshal(le)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// WIRE-01f: cash-movement FACTs fold into the same journal — a subscription adds
// cash, a fee removes it — and the fold is idempotent on the entry id.
func TestFolderFoldsCashMovements(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(testTenant, st, "USD")
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0).UTC()

	sub := cashPayload(t, "cash:S1", "PORT-1", accountingpb.EntryType_ENTRY_TYPE_CASH, decv(100000, 0), "USD", t0)
	subEnv := &envelopepb.Envelope{EventType: cashEventSubscription, IngestionTime: timestamppb.New(t0)}
	if err := f.HandleCash(ctx, subEnv, sub); err != nil {
		t.Fatalf("handle subscription: %v", err)
	}
	// Idempotent redelivery.
	if err := f.HandleCash(ctx, subEnv, sub); err != nil {
		t.Fatalf("handle redelivery: %v", err)
	}

	fee := cashPayload(t, "cash:F1", "PORT-1", accountingpb.EntryType_ENTRY_TYPE_FEE, decv(-250, 0), "USD", t0)
	feeEnv := &envelopepb.Envelope{EventType: cashEventFee, IngestionTime: timestamppb.New(t0)}
	if err := f.HandleCash(ctx, feeEnv, fee); err != nil {
		t.Fatalf("handle fee: %v", err)
	}

	book, err := ledger.MaterializeCurrent(ctx, st, "PORT-1")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// 100000 subscription (idempotent) − 250 fee = 99750.
	if got := book.CashBalance("USD"); got.Cmp(big.NewRat(99750, 1)) != 0 {
		t.Fatalf("cash = %v, want 99750", got)
	}
}

// A non-cash event is acked and ignored by the cash handler.
func TestHandleCashIgnoresNonCashEvent(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(testTenant, st, "USD")
	env := &envelopepb.Envelope{EventType: orderEventFilled}
	if err := f.HandleCash(context.Background(), env, []byte("ignored")); err != nil {
		t.Fatalf("want ack (nil) for non-cash event, got %v", err)
	}
}

// A malformed cash payload (or one missing the cash leg) is returned (nack/DLQ).
func TestHandleCashRejectsMalformed(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(testTenant, st, "USD")
	env := &envelopepb.Envelope{EventType: cashEventSubscription}
	if err := f.HandleCash(context.Background(), env, []byte("not a proto")); err == nil {
		t.Fatal("expected decode error for a malformed cash payload")
	}
	// Valid proto but no cash leg → rejected.
	noCash := cashPayload(t, "cash:X", "PORT", accountingpb.EntryType_ENTRY_TYPE_CASH, nil, "", time.Unix(1, 0))
	if err := f.HandleCash(context.Background(), env, noCash); err == nil {
		t.Fatal("expected rejection of a cash entry with no cash leg")
	}
}

func TestFolderIgnoresNonFillEvent(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(testTenant, st, "USD")
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
	f, _ := NewFolder(testTenant, st, "USD")
	env := &envelopepb.Envelope{EventType: orderEventFilled}
	if err := f.Handle(context.Background(), env, []byte("not-a-proto")); err == nil {
		t.Fatal("malformed fill should surface an error (DLQ), not ack")
	}
}

// A FEE THE CASH LEG CANNOT REPRESENT DLQs — IT DOES NOT POST A CLOSE-ENOUGH
// NUMBER (#221).
//
// OKX charges a spot BUY's fee in the base asset. Netting 0.0008 BTC off a USD
// cash leg posts +1.0 BTC where 0.9992 arrived and −50000.0008 USD where 50000.00
// moved: $40 of phantom NAV per BTC, and the journal is append-only so it is
// permanent and compounds per fill. Held in a DLQ, one fill is re-drivable; posted
// wrong, it is a correcting entry someone has to notice first.
func TestFolderRefusesAFeeInAnotherCurrency(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(st, "USD")
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	env := &envelopepb.Envelope{EventType: orderEventFilled, IngestionTime: timestamppb.New(t0)}

	ev := &orderpb.OrderFilled{
		State: &orderpb.OrderState{PortfolioId: "PORT-1"},
		Fill: &orderpb.Fill{
			FillId: "F-BTCFEE", InstrumentId: "BTC-USDT", Side: orderpb.Side_SIDE_BUY,
			Quantity: decv(1, 0), Price: decv(50000, 0),
			Fee:        &commonpb.Money{Amount: decv(8, -4), CurrencyCode: "BTC"},
			ExecutedAt: timestamppb.New(t0),
		},
	}
	payload, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := f.Handle(ctx, env, payload); err == nil {
		t.Fatal("a BTC fee on a USD book was ACKED — it must nack to the DLQ, " +
			"because a fee netted against a currency it was not charged in is permanent phantom NAV")
	}
	if j, _ := st.Journal(ctx, "PORT-1"); len(j) != 0 {
		t.Fatalf("refused fill still wrote %d journal entries, want 0", len(j))
	}
}

// THE LEDGER REFUSES AN OUT-OF-DOMAIN CASH LEG RATHER THAN GRINDING ON IT (#95).
//
// Decimal.exponent is an unvalidated wire field, and dec.FromProto materialises
// 10^abs(exponent). Before the domain check this did not post a wrong balance —
// HandleCash never returned, so the consumer stopped acking and the whole cash
// subscription stalled behind one message while the service still reported
// healthy. The timeout is what distinguishes "refused" from "still computing".
func TestHandleCashRefusesAnOutOfDomainExponent(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(testTenant, st, "USD")
	env := &envelopepb.Envelope{EventType: cashEventSubscription}
	payload := cashPayload(t, "cash:absurd", "PORT-1", accountingpb.EntryType_ENTRY_TYPE_CASH,
		decv(1, 2000000000), "USD", time.Unix(1, 0))

	done := make(chan error, 1)
	go func() { done <- f.HandleCash(context.Background(), env, payload) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an out-of-domain cash entry was ACKED — it must nack to the DLQ, " +
				"because a cash movement dropped in silence is book-of-record loss")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("HandleCash did not return within 4s — the domain check is not in front of the conversion")
	}

	if j, _ := st.Journal(context.Background(), "PORT-1"); len(j) != 0 {
		t.Fatalf("a refused cash entry still wrote %d journal entries — refusing must not "+
			"half-apply, and it must never substitute zero", len(j))
	}
}

// The same for the FILL path, which decodes through a different function.
func TestFolderRefusesAnOutOfDomainFill(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(testTenant, st, "USD")
	env := &envelopepb.Envelope{EventType: orderEventFilled}
	payload := filledPayload(t, "PORT-1", "fill-absurd", "BTC-USD", orderpb.Side_SIDE_BUY,
		decv(1, 2000000000), decv(50000, 0), time.Unix(1, 0))

	done := make(chan error, 1)
	go func() { done <- f.Handle(context.Background(), env, payload) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an out-of-domain fill quantity was ACKED rather than sent to the DLQ")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Handle did not return within 4s — the fill path is still converting unbounded")
	}
	if j, _ := st.Journal(context.Background(), "PORT-1"); len(j) != 0 {
		t.Fatalf("a refused fill still wrote %d journal entries", len(j))
	}
}

// NON-VACUITY: an ordinary cash entry at the SAME code path still folds. A folder
// that refused everything would satisfy both tests above.
func TestHandleCashStillFoldsAnOrdinaryEntry(t *testing.T) {
	st := ledger.NewMemoryStore()
	f, _ := NewFolder(testTenant, st, "USD")
	env := &envelopepb.Envelope{EventType: cashEventSubscription}
	payload := cashPayload(t, "cash:ok", "PORT-2", accountingpb.EntryType_ENTRY_TYPE_CASH,
		decv(100000, -2), "USD", time.Unix(1, 0))
	if err := f.HandleCash(context.Background(), env, payload); err != nil {
		t.Fatalf("an ordinary cash entry was refused: %v", err)
	}
	if j, _ := st.Journal(context.Background(), "PORT-2"); len(j) != 1 {
		t.Fatalf("ordinary cash entry wrote %d journal entries, want 1", len(j))
	}
}
