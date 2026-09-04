package ledger

import (
	"context"
	"fmt"
	"sort"
)

// AccountScope is a set of exchange accounts, used to restrict a fold to the
// entries that settled against them.
type AccountScope map[string]bool

// Has reports whether the scope contains the account. A nil scope contains
// nothing, which is the safe direction: an unconfigured scope folds an EMPTY
// book rather than the whole one, and the caller's unmapped-account check turns
// that into a refusal instead of a book that looks flat.
func (s AccountScope) Has(account string) bool { return s != nil && s[account] }

// NewAccountScope builds a scope from account ids, refusing an empty one.
//
// An empty account id is not a member of any scope: migration 0003 defines "" as
// the positive declaration that an entry touched NO exchange account, so
// admitting it here would let a declaration claim the fund's own bank cash for a
// custodian that does not hold it.
func NewAccountScope(accounts ...string) (AccountScope, error) {
	s := AccountScope{}
	for _, a := range accounts {
		if a == "" {
			return nil, fmt.Errorf("ledger: an empty account id cannot be scoped to a custodian — " +
				"an entry carrying none settled against no exchange account at all")
		}
		s[a] = true
	}
	return s, nil
}

// MaterializeForAccounts folds a portfolio's journal restricted to the entries
// that settled against one of scope's accounts, and reports any account the
// journal touched that NO custodian claims.
//
// # Why this exists (#1006)
//
// custody.Subject is (portfolio, custodian, business date) and every part of the
// control was custodian-aware except the book side, which loaded the WHOLE
// portfolio and compared it against ONE custodian's statement. For a portfolio
// custodied in two places — which ACCOUNTING_CUSTODY_PAIRS accepts and the
// scheduler is built to iterate — custodian A's run reports every position held
// at B as MISSING_AT_CUSTODIAN and B's run reports every position held at A the
// same way. Every position in the book becomes a break, twice, and the one break
// that means a fill never reached the ledger is buried in it.
//
// accounting.proto names the mirror-image error on the statement side of the same
// comparison: "a partial level read as complete manufactures a break for every
// position it omitted, which is worse than no reconciliation at all because it
// buries the real breaks in noise." The statements were kept separate; the books
// were not.
//
// # Why it derives the custodian rather than storing one
//
// The entry already carries the EXCHANGE ACCOUNT it settled against, migration
// 0003 indexes exactly this fold, and VenueAccountCash already folds per account.
// Putting a custodian on ledger.Event instead would require the FILL producer —
// the OMS, in the execution plane — to know which custodian an account settles
// to, which is custody knowledge in a plane that must not hold it.
//
// # Why it does not resume from a checkpoint
//
// Snapshot is per-PORTFOLIO. A custodian-scoped fold resumed from one would be
// correct on a full replay and would silently cover ONLY THE TAIL once a
// checkpoint existed — understating what a custodian holds, with nothing
// reporting it. That is the trap VenueAccountCash documents and refuses, and this
// refuses it the same way: read the journal, pay for it, be right. Reconciliation
// is a once-per-business-date batch, not a request path.
//
// # Entries carrying no account
//
// EXCLUDED from every custodian's book, not bucketed under one — the same rule
// VenueAccountCash applies, for the same reason. "" is the positive declaration
// that an entry settled against no exchange account (an investor subscription
// into the fund's own bank, a corporate action), and no exchange custodian's
// statement lists it. A consequence worth stating: for a multi-custodian
// portfolio the scoped books do NOT sum to Book — the un-attributed entries are
// in neither. That is correct for a comparison against exchange custodians, and
// Book.CashBalance remains the portfolio total for every other reader.
//
// # The unmapped return
//
// A NON-EMPTY account the journal touched that appears in no custodian's scope is
// a MISCONFIGURATION, and it is returned rather than ignored so the caller can
// refuse the run. Silently folding without it produces exactly the partial book
// the proto warns about. Sorted, so an operator reads the same list every time.
func MaterializeForAccounts(
	ctx context.Context, st Store, portfolioID string, scope, claimed AccountScope,
) (*Book, []string, error) {
	events, err := st.Journal(ctx, portfolioID)
	if err != nil {
		return nil, nil, err
	}

	unmappedSet := map[string]bool{}
	kept := make([]*Event, 0, len(events))
	for _, e := range events {
		if e == nil || e.EntryID == "" {
			continue
		}
		acct := e.VenueAccountID
		if acct == "" {
			continue // settled against no exchange account — see above
		}
		if !claimed.Has(acct) {
			// Only an entry that MOVES something can distort a book. An entry
			// with neither a position nor a cash effect on an unclaimed account
			// is a bookkeeping artefact, not a holding nobody has attributed.
			if movesValue(e) {
				unmappedSet[acct] = true
			}
			continue
		}
		if scope.Has(acct) {
			kept = append(kept, e)
		}
	}

	unmapped := make([]string, 0, len(unmappedSet))
	for a := range unmappedSet {
		unmapped = append(unmapped, a)
	}
	sort.Strings(unmapped)

	// Replay, not a quantity-only sum: the scoped book's AvgCost and Realized are
	// then the cost basis OF THAT CUSTODIAN'S SLICE — a meaningful number — rather
	// than a half-populated struct the next caller would misread as the
	// portfolio's.
	return Replay(portfolioID, kept), unmapped, nil
}

// movesValue reports whether an entry changes a position or a cash balance.
//
// A corporate action counts: it is evaluated against the position at fold time,
// so it moves the holding even though the entry carries no quantity of its own.
func movesValue(e *Event) bool {
	if e.Type == EntryCorporateAction && e.Action != nil {
		return true
	}
	if e.InstrumentID != "" && e.Quantity != nil && e.Quantity.Sign() != 0 {
		return true
	}
	return e.Cash != nil && e.Cash.Sign() != 0
}
