package ledger

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
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
// statement lists it. A consequence worth stating: the scoped books do NOT sum to
// Book — the un-attributed entries are in none of them. That is correct for a
// comparison against exchange custodians, and Book.CashBalance remains the
// portfolio total for every other reader.
//
// THIS RULE IS NOW THE ONLY ONE, AT ONE CUSTODIAN AS AT SEVERAL (#1073). The
// single-custodian path used to compare the WHOLE book, so the same entry was in
// the basis under one configuration and out of it under another, each documented
// as correct. Both cannot be: a run against a custodian statement over a book
// carrying the +1000 of an investor subscription reported a cash break for the
// full 1000, every run, on the DEFAULT configuration. MaterializeAttributed
// derives the scope from the accounts the journal touched, so a one-custodian
// portfolio reaches the same basis without an operator declaring anything.
//
// The excluded entries are RETURNED rather than dropped in silence, as an
// UnattributedResidue: cash genuinely held away from every custodian is real, it
// is reconciled by nothing, and "in no basis" has to be a stated bucket rather
// than an absence a reader infers from two numbers that do not add up.
//
// # The unmapped return
//
// A NON-EMPTY account the journal touched that appears in no custodian's scope is
// a MISCONFIGURATION, and it is returned rather than ignored so the caller can
// refuse the run. Silently folding without it produces exactly the partial book
// the proto warns about. Sorted, so an operator reads the same list every time.
//
// # It returns the transaction grain as well as the fold (#1049)
//
// A CustodyBasis carries the executions behind the book, not only the book. The
// position pass and the transaction pass must be over the SAME slice of the
// journal or they disagree about what they are reconciling, and one return value
// out of one filter is what makes that structural rather than remembered.
func MaterializeForAccounts(
	ctx context.Context, st Store, portfolioID string, scope, claimed AccountScope,
) (CustodyBasis, error) {
	events, err := st.Journal(ctx, portfolioID)
	if err != nil {
		return CustodyBasis{}, err
	}
	basis := foldForAccounts(portfolioID, events, scope, claimed)
	basis.Accounts = scope
	return basis, nil
}

// MaterializeAttributed folds the entries that settled against SOME exchange
// account, with the scope DERIVED from the accounts the journal actually touched.
//
// IT IS THE BASIS FOR A PORTFOLIO WITH ONE CUSTODIAN (#1073), and it exists so
// that basis is scoped BY CONSTRUCTION rather than by operator diligence. A
// portfolio custodied in one place holds every exchange account it touches at
// that custodian — that is what "one custodian" means — so there is nothing for a
// declaration to add and no account this can fail to claim. What a declaration
// could never supply is the other half: the entries that settled against NO
// exchange account are excluded here exactly as MaterializeForAccounts excludes
// them, so the comparison basis follows one rule at one custodian and at several.
//
// The alternative shipped until #1073 was the whole book, and it manufactured a
// cash break for every investor subscription into the fund's own bank — on the
// default configuration, every run, in the queue that exists to surface the one
// break meaning a fill never reached the ledger.
//
// IT RETURNS THE DERIVED SCOPE, AND A CALLER MUST READ IT. An EMPTY scope over a
// journal that holds value is the mirror-image defect: every entry declares it
// settled against no exchange account, so the basis folds to nothing and every
// position the custodian holds comes back as MISSING_IN_IBOR. That is the state
// NewBookScope already refuses an operator who declares an empty account set, and
// it must not arrive by derivation instead. This returns the evidence; the custody
// loader is where the refusal belongs, because "compared against a custodian" is
// what makes an empty basis wrong rather than merely small.
func MaterializeAttributed(ctx context.Context, st Store, portfolioID string) (CustodyBasis, error) {
	events, err := st.Journal(ctx, portfolioID)
	if err != nil {
		return CustodyBasis{}, err
	}
	scope := AttributedAccounts(events)
	basis := foldForAccounts(portfolioID, events, scope, scope)
	basis.Accounts = scope
	if len(basis.Unmapped) > 0 {
		// UNREACHABLE, AND LOUD RATHER THAN IGNORED. scope and claimed are both
		// the set of accounts this journal touched, so no account it touched can
		// be unclaimed. Reaching it means AttributedAccounts and foldForAccounts
		// disagree about what an account is, and the fold would then be silently
		// missing holdings — the partial book that breaks every position it
		// omitted.
		return CustodyBasis{Accounts: scope, Residue: basis.Residue}, fmt.Errorf("ledger: portfolio %s: the "+
			"derived custody scope does not claim account(s) %s that its own journal touched — the fold "+
			"and the derivation disagree", portfolioID, strings.Join(basis.Unmapped, ", "))
	}
	return basis, nil
}

// AttributedAccounts returns every non-empty exchange account the journal
// touched.
//
// THE EMPTY ACCOUNT IS NOT A MEMBER, for NewAccountScope's reason: "" is the
// positive declaration that an entry settled against no exchange account, so
// admitting it would put the fund's own bank cash into a custodian's book through
// the derivation instead of through a declaration — the same claim NewAccountScope
// refuses an operator, arriving by a route nobody typed.
func AttributedAccounts(events []*Event) AccountScope {
	s := AccountScope{}
	for _, e := range events {
		if e == nil || e.EntryID == "" || e.VenueAccountID == "" {
			continue
		}
		s[e.VenueAccountID] = true
	}
	return s
}

// UnattributedResidue is what a custodian-scoped fold deliberately left OUT: the
// entries that settled against no exchange account at all.
//
// IT IS A BUCKET, NOT A BREAK, AND NOT NOTHING. Cash genuinely held away from
// every custodian — an investor subscription sitting in the fund's own bank — is
// real money in the book of record that NO custodian statement can confirm. Book
// still carries it for every other reader; the reconciliation basis does not, and
// so nothing reconciles it. Returning it is what makes that an explicit
// unreconciled bucket rather than a silent omission, which is the half of #1073
// that neither of the two disagreeing rules stated.
type UnattributedResidue struct {
	// Entries is how many distinct journal entries were excluded.
	Entries int
	// Cash is currency -> the net cash those entries moved.
	Cash map[string]*big.Rat
	// Instruments names the instruments whose quantity they moved, sorted. A
	// position held away from every exchange is rarer than cash and worth naming
	// separately: it is a holding no custodian will ever confirm.
	Instruments []string
}

// Empty reports whether the fold excluded nothing that moves value.
func (r UnattributedResidue) Empty() bool { return len(r.Cash) == 0 && len(r.Instruments) == 0 }

// Describe renders the residue for an operator, sorted so the same journal reads
// the same way every run.
func (r UnattributedResidue) Describe() string {
	parts := make([]string, 0, len(r.Cash)+len(r.Instruments))
	ccys := make([]string, 0, len(r.Cash))
	for ccy := range r.Cash {
		ccys = append(ccys, ccy)
	}
	sort.Strings(ccys)
	for _, ccy := range ccys {
		parts = append(parts, ccy+" "+r.Cash[ccy].FloatString(8))
	}
	parts = append(parts, r.Instruments...)
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

// foldForAccounts is the ONE filter both entry points share, so "which entries are
// in a custodian's comparison basis" has a single answer rather than one per
// caller. Two answers is the shape #1073 was filed on: the scoped path excluded an
// un-attributed entry, the single-custodian path included it, and each documented
// itself as correct.
func foldForAccounts(portfolioID string, events []*Event, scope, claimed AccountScope) CustodyBasis {
	unmappedSet := map[string]bool{}
	residue := UnattributedResidue{Cash: map[string]*big.Rat{}}
	instruments := map[string]bool{}
	// Deduped for VenueAccountCash's reason: the journal legitimately returns a
	// restated entry alongside the original, and summing both would overstate the
	// residue. Book.Apply applies the same rule to the entries that are kept.
	seen := map[string]bool{}
	kept := make([]*Event, 0, len(events))
	for _, e := range events {
		if e == nil || e.EntryID == "" {
			continue
		}
		acct := e.VenueAccountID
		if acct == "" {
			// Settled against no exchange account — see above. MEASURED rather
			// than dropped in silence: this is the bucket nothing reconciles.
			if seen[e.EntryID] {
				continue
			}
			seen[e.EntryID] = true
			residue.Entries++
			if e.Cash != nil && e.Cash.Sign() != 0 {
				if residue.Cash[e.CashCurrency] == nil {
					residue.Cash[e.CashCurrency] = new(big.Rat)
				}
				residue.Cash[e.CashCurrency].Add(residue.Cash[e.CashCurrency], e.Cash)
			}
			if e.InstrumentID != "" && e.Quantity != nil && e.Quantity.Sign() != 0 {
				instruments[e.InstrumentID] = true
			}
			continue
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

	residue.Instruments = make([]string, 0, len(instruments))
	for i := range instruments {
		residue.Instruments = append(residue.Instruments, i)
	}
	sort.Strings(residue.Instruments)

	// ORDERED ONCE, CONSUMED TWICE. The fold is order-sensitive (weighted-average
	// cost realizes P&L in sequence) and the execution list dedupes a restatement
	// against its original, so both must see the entries in the SAME canonical
	// order or the copy the transaction pass keeps is not the one the fold
	// applied. Replay re-sorts what it is given, which is idempotent here.
	ordered := sortedFor(kept, time.Time{}, time.Time{}, false)
	executions, unreferenced := executionsOf(ordered)

	// Replay, not a quantity-only sum: the scoped book's AvgCost and Realized are
	// then the cost basis OF THAT CUSTODIAN'S SLICE — a meaningful number — rather
	// than a half-populated struct the next caller would misread as the
	// portfolio's.
	return CustodyBasis{
		Book:         Replay(portfolioID, ordered),
		Executions:   executions,
		Unreferenced: unreferenced,
		Unmapped:     unmapped,
		Residue:      residue,
	}
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
