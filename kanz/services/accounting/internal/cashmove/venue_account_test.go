package cashmove

import (
	"testing"
	"time"
)

// CASH MUST BE ABLE TO SAY WHICH EXCHANGE ACCOUNT IT LANDED IN (#415).
//
// The owner's sentence is "PF1 holds 100 of the 1,000 USDT in okx-sub-1". The
// ledger could express the first half and not the second: a subscription said
// "PF1 has 100 USDT" and nothing recorded WHERE.
//
// EVERY OTHER LAYER WAS ALREADY BUILT FOR IT. accounting.v1.LedgerEntry has
// venue_account_id (field 12); ledger.Event has VenueAccountID; the FILL path
// sets it (ledger/fill.go:67); ledger/postgres.go declares it to the transaction
// via app.venue_account_id; migration 0003 adds the column, a write-guard that
// REFUSES an undeclared account, and an index whose own comment says it exists
// for "per-account cash and position folds — what is actually in okx-sub-1".
//
// Only the cash producer could not fill it in. So the index for per-account CASH
// had no cash to index.
//
// WHY THE ACCOUNT IS OPTIONAL AND NOT REQUIRED. The two cases are genuinely
// different and the ledger already distinguishes them: '' is a POSITIVE
// DECLARATION that an entry touches no exchange account (migration 0003), which
// is the truth for an investor subscription into the fund's own bank. A transfer
// that funds okx-sub-1 is the other case. Forcing one answer would make one of
// them a lie.

func TestEncodeCarriesTheVenueAccount(t *testing.T) {
	m := mv(Subscription, "100")
	m.VenueAccountID = "okx-sub-1"

	entry, _, err := encode(m, time.Now())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := entry.GetVenueAccountId(); got != "okx-sub-1" {
		t.Fatalf("venue_account_id = %q, want %q.\n"+
			"Without it the ledger can say a portfolio holds cash but never WHERE, so the "+
			"per-account index migration 0003 built for exactly this has no cash to index.",
			got, "okx-sub-1")
	}
}

// AN OMITTED ACCOUNT STAYS EMPTY, and empty MEANS something here: migration 0003
// treats the EMPTY STRING as the positive declaration "this entry touches no
// exchange account". Defaulting it to anything else would post an investor subscription
// against collateral it never reached.
func TestEncodeLeavesTheVenueAccountEmptyWhenUnset(t *testing.T) {
	entry, _, err := encode(mv(Subscription, "100"), time.Now())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := entry.GetVenueAccountId(); got != "" {
		t.Fatalf("venue_account_id = %q, want empty — an unscoped movement must declare "+
			"that it touched no exchange account, not guess one", got)
	}
}

// It rides every kind, because money leaves an exchange account as well as
// entering one. A redemption or a fee paid out of okx-sub-1 reduces THAT
// account's collateral, and a ledger that recorded only the deposits would drift
// in the direction that overstates what is available.
func TestEncodeCarriesTheVenueAccountOnEveryKind(t *testing.T) {
	for _, k := range []Kind{Subscription, Redemption, Fee} {
		m := mv(k, "50")
		m.VenueAccountID = "binance-alpha"
		entry, _, err := encode(m, time.Now())
		if err != nil {
			t.Fatalf("kind %v: encode: %v", k, err)
		}
		if got := entry.GetVenueAccountId(); got != "binance-alpha" {
			t.Errorf("kind %v: venue_account_id = %q, want binance-alpha", k, got)
		}
	}
}
