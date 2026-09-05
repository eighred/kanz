package balancerecon

import (
	"context"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/execution"
)

var at0 = time.Unix(1_700_000_000, 0).UTC()

func announcement(t *testing.T, asOf time.Time, accounts ...*accountingpb.VenueAccountCash) []byte {
	t.Helper()
	b, err := proto.Marshal(&accountingpb.PortfolioCashBalance{
		PortfolioId:    "PF1",
		BaseCurrency:   "USD",
		Total:          &commonpb.Decimal{Coefficient: 840},
		ByVenueAccount: accounts,
		AsOf:           timestamppb.New(asOf),
		KnowledgeTime:  timestamppb.New(asOf),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func acct(account, asset string, amount int64) *accountingpb.VenueAccountCash {
	return &accountingpb.VenueAccountCash{
		VenueAccountId: account, Asset: asset,
		Amount: &commonpb.Decimal{Coefficient: amount},
	}
}

// BEFORE ANY ANNOUNCEMENT, EVERY ASSET IS UNKNOWN (#418).
//
// This is the state every adapter boots in, and it is the one that matters:
// answering zero here makes reconciliation report every asset the exchange holds
// as a break on the first pass — a storm that teaches an operator to ignore the
// layer.
func TestView_UnknownBeforeAnyAnnouncement(t *testing.T) {
	v := NewView("okx-sub-1", WithViewClock(func() time.Time { return at0 }))
	if _, ok, _ := v.Balance("USDT"); ok {
		t.Fatal("an adapter that has never been told its balance reported one — unknown must " +
			"not become zero, or the first reconciliation pass breaks on every asset")
	}
}

func TestView_FoldsItsOwnAccountOnly(t *testing.T) {
	v := NewView("okx-sub-1", WithViewClock(func() time.Time { return at0 }))
	payload := announcement(t, at0,
		acct("okx-sub-1", "USDT", 140),
		acct("binance-alpha", "USDT", 700),
	)
	if err := v.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, ok, _ := v.Balance("USDT")
	if !ok {
		t.Fatal("the announcement was not folded")
	}
	if got.Cmp(big.NewRat(140, 1)) != 0 {
		t.Fatalf("USDT = %s, want 140 — taking another account's figure would reconcile this "+
			"exchange against another exchange's collateral", got.RatString())
	}
}

// AN ASSET ABSENT FROM AN ARRIVED ANNOUNCEMENT IS KNOWN-ZERO, NOT UNKNOWN.
//
// This is the discrepancy that matters most: the exchange holding something we
// believe we do not. Reporting it as unknown would skip precisely the case
// reconciliation exists to catch.
func TestView_AbsentAssetIsKnownZero(t *testing.T) {
	v := NewView("okx-sub-1", WithViewClock(func() time.Time { return at0 }))
	if err := v.Handle(context.Background(), nil, announcement(t, at0, acct("okx-sub-1", "USDT", 140))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, ok, _ := v.Balance("BTC")
	if !ok {
		t.Fatal("an asset absent from an ARRIVED announcement was reported unknown — that skips " +
			"the exchange holding something we do not think we have")
	}
	if got.Sign() != 0 {
		t.Fatalf("BTC = %s, want 0", got.RatString())
	}
}

// A LEVEL REPLACES, IT DOES NOT MERGE. A zero balance is announced by ABSENCE, so
// merging would leave an asset that fell to zero showing its last non-zero figure
// forever — a permanent phantom break.
func TestView_ReplacesRatherThanMerges(t *testing.T) {
	v := NewView("okx-sub-1", WithViewClock(func() time.Time { return at0 }))
	ctx := context.Background()
	if err := v.Handle(ctx, nil, announcement(t, at0, acct("okx-sub-1", "USDT", 140), acct("okx-sub-1", "BTC", 2))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// BTC has since gone to zero, so it is simply absent from the next level.
	if err := v.Handle(ctx, nil, announcement(t, at0, acct("okx-sub-1", "USDT", 140))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, ok, _ := v.Balance("BTC")
	if !ok {
		t.Fatal("BTC became unknown after a level that omitted it — it is known to be zero")
	}
	if got.Sign() != 0 {
		t.Fatalf("BTC = %s, want 0 — a merge left the previous level's figure standing forever",
			got.RatString())
	}
}

// A STALE BALANCE IS UNKNOWN. Serving the last announcement forever would compare
// the exchange against a figure from before whatever moved it, and report a break
// that is really staleness.
func TestView_StaleIsUnknown(t *testing.T) {
	now := at0
	var staleAge time.Duration
	v := NewView("okx-sub-1",
		WithViewClock(func() time.Time { return now }),
		WithViewMaxAge(15*time.Minute),
		WithViewOnStale(func(age time.Duration) { staleAge = age }),
	)
	if err := v.Handle(context.Background(), nil, announcement(t, at0, acct("okx-sub-1", "USDT", 140))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok, _ := v.Balance("USDT"); !ok {
		t.Fatal("a fresh balance was treated as stale")
	}
	now = at0.Add(16 * time.Minute)
	if _, ok, _ := v.Balance("USDT"); ok {
		t.Fatal("a balance past the bound was served as current")
	}
	if staleAge < 16*time.Minute {
		t.Fatalf("stale callback age = %v, want past the bound", staleAge)
	}
}

// It satisfies the seam the reconcilers consume — the whole point of #418.
func TestView_IsAnExpectedBalances(t *testing.T) {
	v := NewView("okx-sub-1")
	if err := v.Handle(context.Background(), nil, announcement(t, time.Now().UTC(), acct("okx-sub-1", "USDT", 5))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Announce reports the posture as CONFIGURED once a real seam is bound.
	g := NewGauge("okx")
	if got := Announce(g, nil, "okx", v); got == nil {
		t.Fatal("Announce dropped a bound view")
	}
}

// THE UNKNOWN NAMES ITSELF, AND THE TWO ARE DIFFERENT INCIDENTS (#1063).
//
// Both answers are ok=false and they send an operator to different places: never
// announced means the cash spine has not delivered once, so this account has NEVER
// been compared against the exchange; stale means it delivered and has stopped.
// A counter that cannot tell them apart says only "something", which is the state
// this seam was in when it said nothing at all.
func TestTheUnknownNamesWhichUnknownItIs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	v := NewView("acct-1",
		WithViewClock(func() time.Time { return now }),
		WithViewMaxAge(15*time.Minute))

	if _, ok, reason := v.Balance("USDT"); ok || reason != execution.BalanceUnknownNeverAnnounced {
		t.Fatalf("cold view answered (ok=%v, reason=%q), want (false, %q)",
			ok, reason, execution.BalanceUnknownNeverAnnounced)
	}

	if err := v.Handle(context.Background(), nil, announcement(t, now, acct("acct-1", "USDT", 100))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok, reason := v.Balance("USDT"); !ok || reason != "" {
		t.Fatalf("fresh view answered (ok=%v, reason=%q), want (true, \"\") — a KNOWN balance "+
			"must not carry a reason, or every asset looks like a skip", ok, reason)
	}

	now = now.Add(16 * time.Minute)
	if _, ok, reason := v.Balance("USDT"); ok || reason != execution.BalanceUnknownStale {
		t.Fatalf("aged-out view answered (ok=%v, reason=%q), want (false, %q)",
			ok, reason, execution.BalanceUnknownStale)
	}
}
