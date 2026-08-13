package ledger

import (
	"math/big"
	"sort"
)

// VenueAccountCash answers "what does this portfolio hold in okx-sub-1", per
// asset — the read model #415 step 3 asks for, and the one #418's exchange
// balance reconciliation has no source for.
//
// The ledger has always segregated cash by PORTFOLIO. An exchange does not: it
// margins and LIQUIDATES per ACCOUNT. So "PF1 holds 100 USDT" is not a number
// anyone can reconcile against a venue; "PF1 holds 100 USDT in okx-sub-1" is.
// migration 0003 built the index for exactly this fold — its comment names it
// "per-account cash and position folds — what is actually in okx-sub-1" — and
// until the cash producer could stamp an account and the journal could read one
// back, it had nothing to index.
//
// # Why this is a free function and not a field on Book
//
// Book's state is CHECKPOINTED. Snapshot carries Positions, Cash and Accrued,
// and MaterializeCurrent resumes from a checkpoint plus the tail of the journal.
// A per-account map added to Book but not to Snapshot would therefore be correct
// on a full replay and silently cover ONLY THE TAIL whenever a checkpoint
// existed — the balance would be wrong, in the direction that understates what
// an account holds, and nothing would report it.
//
// Extending Snapshot is a real option and a bigger change: the struct, its
// Postgres serialization, and a migration. Doing it later is cheap; discovering
// that a projection quietly halved after the first checkpoint is not. So this
// folds the journal directly and takes the full read, which is honest about its
// cost rather than fast and occasionally wrong.
//
// # Ordering
//
// Unlike the position fold, this one is ORDER-INSENSITIVE: it sums signed cash
// per (account, currency), and addition commutes. That is why it can be a fold
// over any journal slice without the (effective, knowledge, entry_id) fence
// MaterializeCurrent needs. It still dedupes on entry_id, because the journal
// legitimately returns a restated entry alongside the original and Book.Apply
// applies the same rule.
//
// Entries carrying no account are EXCLUDED, not bucketed under "". Migration
// 0003 defines the empty string as the positive declaration that an entry
// touched no exchange account — an investor subscription into the fund's own
// bank — so folding those into a bucket would invent an exchange account that
// holds the fund's uninvested cash. Callers wanting the portfolio total already
// have Book.CashBalance.
func VenueAccountCash(events []*Event) map[string]map[string]*big.Rat {
	out := map[string]map[string]*big.Rat{}
	seen := map[string]bool{}
	for _, e := range events {
		if e == nil || e.EntryID == "" || e.VenueAccountID == "" {
			continue
		}
		if seen[e.EntryID] {
			continue
		}
		seen[e.EntryID] = true
		if e.Cash == nil || e.Cash.Sign() == 0 || e.CashCurrency == "" {
			continue
		}
		// ACCRUALS ARE NOT BALANCES. Book keeps them in a separate map for the
		// same reason: an accrued fee is money the fund OWES and has not paid, so
		// it is not sitting in the exchange account. Counting it here would report
		// collateral the venue does not hold — the direction that overstates.
		if e.Type == EntryAccrual {
			continue
		}
		byCcy := out[e.VenueAccountID]
		if byCcy == nil {
			byCcy = map[string]*big.Rat{}
			out[e.VenueAccountID] = byCcy
		}
		add(byCcy, e.CashCurrency, e.Cash)
	}
	return out
}

// VenueAccounts returns the exchange accounts a journal touched, sorted, so a
// caller can enumerate them without ranging a map in nondeterministic order.
func VenueAccounts(balances map[string]map[string]*big.Rat) []string {
	out := make([]string, 0, len(balances))
	for a := range balances {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
