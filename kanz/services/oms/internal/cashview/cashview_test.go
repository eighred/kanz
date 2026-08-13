package cashview

import (
	"context"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

var t0 = time.Unix(1_700_000_000, 0).UTC()

func announcement(t *testing.T, portfolio string, total int64, asOf time.Time, accounts ...*accountingpb.VenueAccountCash) []byte {
	t.Helper()
	b, err := proto.Marshal(&accountingpb.PortfolioCashBalance{
		PortfolioId:    portfolio,
		BaseCurrency:   "USD",
		Total:          &commonpb.Decimal{Coefficient: total},
		ByVenueAccount: accounts,
		AsOf:           timestamppb.New(asOf),
		KnowledgeTime:  timestamppb.New(asOf),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestHandle_FoldsTheAnnouncedBalance(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if err := v.Handle(context.Background(), nil, announcement(t, "PF1", 750, t0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	total, ccy, ok := v.Spendable("PF1")
	if !ok {
		t.Fatal("the balance was not folded")
	}
	if got := dec.FromProto(total).RatString(); got != "750" {
		t.Fatalf("total = %s, want 750", got)
	}
	if ccy != "USD" {
		t.Fatalf("currency = %q, want USD", ccy)
	}
}

// AN UNANNOUNCED PORTFOLIO IS UNKNOWN, NOT ZERO.
//
// A portfolio with no balance has not been shown to be empty; it has not been
// shown at all. Returning zero would read as an account with nothing in it — a
// breach — and the difference decides whether an order is refused for a reason
// or for a fiction.
func TestSpendable_UnannouncedIsUnknownNotZero(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if _, _, ok := v.Spendable("never-seen"); ok {
		t.Fatal("an unannounced portfolio reported a balance — unknown must not become zero")
	}
}

// A STALE BALANCE AGES OUT TO UNKNOWN.
//
// Announcements are derived state and can be lost: a publish failure must never
// fail the ledger write that caused it. Without a bound, the last balance
// received would be served forever — and a portfolio that had since spent
// everything would keep passing a buying-power check against a number from
// before the spending.
func TestSpendable_StaleBalanceIsUnknown(t *testing.T) {
	now := t0
	var staleFor string
	var staleAge time.Duration
	v := New(
		WithClock(func() time.Time { return now }),
		WithMaxAge(5*time.Minute),
		WithOnStale(func(pf string, age time.Duration) { staleFor, staleAge = pf, age }),
	)
	if err := v.Handle(context.Background(), nil, announcement(t, "PF1", 750, t0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, ok := v.Spendable("PF1"); !ok {
		t.Fatal("a fresh balance was treated as stale")
	}

	now = t0.Add(6 * time.Minute)
	if _, _, ok := v.Spendable("PF1"); ok {
		t.Fatal("a balance older than the bound was served as current — a portfolio that has " +
			"since spent everything would keep passing a buying-power check")
	}
	// AND IT SAYS SO. An operator must learn that announcements stopped, not
	// infer it from orders being refused.
	if staleFor != "PF1" || staleAge < 6*time.Minute {
		t.Fatalf("stale callback = (%q, %v), want PF1 and an age past the bound", staleFor, staleAge)
	}
}

// A LEVEL, NOT A DELTA: a redelivery is a no-op, and a later announcement
// REPLACES rather than accumulates. That is what makes a lost publish
// survivable — the next one is right regardless of how many were missed.
func TestHandle_IsAReplaceNotAnAccumulate(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := v.Handle(ctx, nil, announcement(t, "PF1", 750, t0)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	total, _, _ := v.Spendable("PF1")
	if got := dec.FromProto(total).RatString(); got != "750" {
		t.Fatalf("after three deliveries of one level, total = %s, want 750", got)
	}

	if err := v.Handle(ctx, nil, announcement(t, "PF1", 500, t0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	total, _, _ = v.Spendable("PF1")
	if got := dec.FromProto(total).RatString(); got != "500" {
		t.Fatalf("a later level did not replace the earlier one: total = %s, want 500", got)
	}
}

// THE PER-ACCOUNT HALF IS KEPT for exchange balance reconciliation (#418).
func TestHandle_KeepsPerVenueAccountBalances(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	payload := announcement(t, "PF1", 840, t0,
		&accountingpb.VenueAccountCash{VenueAccountId: "okx-sub-1", Asset: "USDT", Amount: &commonpb.Decimal{Coefficient: 140}},
		&accountingpb.VenueAccountCash{VenueAccountId: "binance-alpha", Asset: "USDT", Amount: &commonpb.Decimal{Coefficient: 700}},
	)
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	b, ok := v.Lookup("PF1")
	if !ok {
		t.Fatal("not folded")
	}
	if got := dec.FromProto(b.ByVenueAccount["okx-sub-1"]["USDT"]).RatString(); got != "140" {
		t.Fatalf("okx-sub-1 USDT = %s, want 140", got)
	}
	if got := dec.FromProto(b.ByVenueAccount["binance-alpha"]["USDT"]).RatString(); got != "700" {
		t.Fatalf("binance-alpha USDT = %s, want 700", got)
	}
}

// A MALFORMED ANNOUNCEMENT IS ACKED, NOT REDELIVERED FOREVER. It is a permanent
// defect: nacking would replay the same bad bytes indefinitely while the balance
// simply ages out to unknown, which fails closed downstream anyway.
func TestHandle_AcksGarbage(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	if err := v.Handle(context.Background(), nil, []byte("not a proto")); err != nil {
		t.Fatalf("Handle(garbage) = %v, want nil (ack) — a nack replays it forever", err)
	}
	if _, _, ok := v.Spendable("PF1"); ok {
		t.Fatal("garbage produced a balance")
	}
}

// AN OUT-OF-DOMAIN EXPONENT IS REFUSED BEFORE ANY NUMBER IS READ (#95).
// dec.FromProto materialises 10^abs(exponent), so a corrupt announcement would
// hang the handler rather than post a wrong balance.
func TestHandle_RefusesAnOutOfDomainExponent(t *testing.T) {
	v := New(WithClock(func() time.Time { return t0 }))
	payload, err := proto.Marshal(&accountingpb.PortfolioCashBalance{
		PortfolioId:  "PF1",
		BaseCurrency: "USD",
		Total:        &commonpb.Decimal{Coefficient: 1, Exponent: 2_000_000_000},
		AsOf:         timestamppb.New(t0),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- v.Handle(context.Background(), nil, payload) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Handle = %v, want nil (ack)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handle did not return — the exponent was materialised instead of refused (#95)")
	}
	if _, _, ok := v.Spendable("PF1"); ok {
		t.Fatal("an out-of-domain balance was folded")
	}
}
