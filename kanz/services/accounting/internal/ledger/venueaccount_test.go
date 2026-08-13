package ledger

import (
	"math/big"
	"testing"
	"time"
)

func cashEvent(id, account, ccy string, amount int64, typ EntryType) *Event {
	return &Event{
		EntryID:        id,
		PortfolioID:    "PF1",
		VenueAccountID: account,
		Type:           typ,
		Cash:           big.NewRat(amount, 1),
		CashCurrency:   ccy,
		Effective:      time.Unix(1_700_000_000, 0).UTC(),
		Knowledge:      time.Unix(1_700_000_000, 0).UTC(),
	}
}

// THE SENTENCE THE PLATFORM COULD NOT SAY (#415).
//
// "PF1 holds 100 of the USDT in okx-sub-1." The ledger segregates cash by
// PORTFOLIO; an exchange margins and liquidates per ACCOUNT, so a per-portfolio
// number is not one anyone can reconcile against a venue.
func TestVenueAccountCash_SumsPerAccountAndAsset(t *testing.T) {
	got := VenueAccountCash([]*Event{
		cashEvent("cash:1", "okx-sub-1", "USDT", 100, EntryCash),
		cashEvent("cash:2", "okx-sub-1", "USDT", 40, EntryCash),
		cashEvent("cash:3", "okx-sub-1", "BTC", 2, EntryCash),
		cashEvent("cash:4", "binance-alpha", "USDT", 700, EntryCash),
	})

	if n := len(got); n != 2 {
		t.Fatalf("accounts = %d, want 2: %v", n, VenueAccounts(got))
	}
	if v := got["okx-sub-1"]["USDT"]; v == nil || v.Cmp(big.NewRat(140, 1)) != 0 {
		t.Errorf("okx-sub-1 USDT = %v, want 140", v)
	}
	if v := got["okx-sub-1"]["BTC"]; v == nil || v.Cmp(big.NewRat(2, 1)) != 0 {
		t.Errorf("okx-sub-1 BTC = %v, want 2 — assets must not be merged across currencies", v)
	}
	if v := got["binance-alpha"]["USDT"]; v == nil || v.Cmp(big.NewRat(700, 1)) != 0 {
		t.Errorf("binance-alpha USDT = %v, want 700 — accounts must not be merged", v)
	}
}

// MONEY LEAVES AN ACCOUNT TOO. A fold that summed only deposits would drift in
// the direction that overstates what an exchange holds, which is the direction
// that gets a fund liquidated while its books look funded.
func TestVenueAccountCash_NetsOutflows(t *testing.T) {
	got := VenueAccountCash([]*Event{
		cashEvent("cash:1", "okx-sub-1", "USDT", 1000, EntryCash),
		cashEvent("cash:2", "okx-sub-1", "USDT", -250, EntryCash),
		cashEvent("fee:1", "okx-sub-1", "USDT", -50, EntryFee),
	})
	if v := got["okx-sub-1"]["USDT"]; v == nil || v.Cmp(big.NewRat(700, 1)) != 0 {
		t.Fatalf("okx-sub-1 USDT = %v, want 700 (1000 − 250 − 50)", v)
	}
}

// AN ENTRY THAT TOUCHED NO EXCHANGE ACCOUNT IS EXCLUDED, NOT BUCKETED UNDER "".
//
// Migration 0003 defines the empty string as the positive declaration that an
// entry touched no exchange account — an investor subscription into the fund's
// own bank. Bucketing those would invent an exchange account holding the fund's
// uninvested cash, and it would be the largest one on the page.
func TestVenueAccountCash_ExcludesUnscopedEntries(t *testing.T) {
	got := VenueAccountCash([]*Event{
		cashEvent("cash:1", "", "USD", 1_000_000, EntryCash),
		cashEvent("cash:2", "okx-sub-1", "USDT", 100, EntryCash),
	})
	if _, ok := got[""]; ok {
		t.Fatal(`an entry with no exchange account was bucketed under "" — that invents an ` +
			`account holding the fund's uninvested cash`)
	}
	if n := len(got); n != 1 {
		t.Fatalf("accounts = %v, want only okx-sub-1", VenueAccounts(got))
	}
}

// ACCRUALS ARE NOT BALANCES. An accrued fee is money the fund OWES and has not
// paid, so it is not sitting in the exchange account. Book keeps accruals in a
// separate map for the same reason; counting them here reports collateral the
// venue does not hold.
func TestVenueAccountCash_ExcludesAccruals(t *testing.T) {
	got := VenueAccountCash([]*Event{
		cashEvent("cash:1", "okx-sub-1", "USDT", 100, EntryCash),
		cashEvent("acc:1", "okx-sub-1", "USDT", -30, EntryAccrual),
	})
	if v := got["okx-sub-1"]["USDT"]; v == nil || v.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("okx-sub-1 USDT = %v, want 100 — an accrual is owed, not held", v)
	}
}

// A RESTATED ENTRY IS NOT A SECOND ENTRY. The journal legitimately returns a
// restatement alongside the original, and Book.Apply dedupes on entry_id for
// exactly this reason. Double-counting here would report an account holding
// twice what it does.
func TestVenueAccountCash_DedupesOnEntryID(t *testing.T) {
	e := cashEvent("cash:1", "okx-sub-1", "USDT", 100, EntryCash)
	got := VenueAccountCash([]*Event{e, e, e})
	if v := got["okx-sub-1"]["USDT"]; v == nil || v.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("okx-sub-1 USDT = %v, want 100 after three deliveries of one entry", v)
	}
}

// The fold is order-insensitive by construction — it sums, and addition
// commutes — so it is safe over any journal slice without the (effective,
// knowledge, entry_id) fence the POSITION fold needs. Pinned so a future change
// that introduces order-sensitivity (a running balance, a high-water mark) has
// to break this deliberately.
func TestVenueAccountCash_IsOrderInsensitive(t *testing.T) {
	a := cashEvent("cash:1", "okx-sub-1", "USDT", 1000, EntryCash)
	b := cashEvent("cash:2", "okx-sub-1", "USDT", -250, EntryCash)
	c := cashEvent("cash:3", "okx-sub-1", "USDT", 30, EntryCash)

	forward := VenueAccountCash([]*Event{a, b, c})["okx-sub-1"]["USDT"]
	reverse := VenueAccountCash([]*Event{c, b, a})["okx-sub-1"]["USDT"]
	if forward.Cmp(reverse) != 0 {
		t.Fatalf("order changed the balance: %v vs %v", forward, reverse)
	}
}

func TestVenueAccounts_IsSorted(t *testing.T) {
	got := VenueAccounts(VenueAccountCash([]*Event{
		cashEvent("c:1", "okx-sub-1", "USDT", 1, EntryCash),
		cashEvent("c:2", "binance-alpha", "USDT", 1, EntryCash),
		cashEvent("c:3", "coinbase-1", "USDT", 1, EntryCash),
	}))
	want := []string{"binance-alpha", "coinbase-1", "okx-sub-1"}
	if len(got) != len(want) {
		t.Fatalf("accounts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("accounts = %v, want %v (sorted, so a caller is not ranging a map)", got, want)
		}
	}
}
