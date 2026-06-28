// Package corpact processes corporate actions (IBOR-01c): it turns a
// CorporateAction (split / dividend / merger / coupon) into the journal entry the
// book folds to adjust positions and cash. The entries are bitemporal — each
// carries the ex-date (effective time) and the announcement time (knowledge
// time), so a late or amended announcement RESTATES history: a point-in-time read
// as-of an earlier knowledge time never sees it, and a read after it does. The
// action's effect is evaluated against the position-at-ex-date during the fold
// (ledger.foldCorpAct), so a restatement that changes the holding at the ex-date
// changes the action's effect too — knowledge-time correct by construction.
package corpact

import (
	"math/big"
	"time"

	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
)

// Kind is the kind of corporate action.
type Kind int

const (
	Split Kind = iota
	Dividend
	Merger
	Coupon
)

// CorporateAction is the internal working shape behind accounting.v1
// .CorporateAction. Cash factors are exact (*big.Rat); ratios are exact rationals
// too (a 3:2 split is 1.5 exactly).
type CorporateAction struct {
	ActionID     string
	PortfolioID  string
	Kind         Kind
	InstrumentID string

	// Ratio is the share factor for a Split (2 = 2:1) or a Merger exchange.
	Ratio *big.Rat
	// PerUnit is dividend-per-share, coupon cash per unit of face, or merger
	// cash-per-share.
	PerUnit *big.Rat
	// Target is the instrument received in a Merger.
	Target   string
	Currency string

	// ExDate is when the action takes economic effect; AnnouncedAt is when it
	// became known (the knowledge time). A late AnnouncedAt with an early ExDate
	// is a restatement.
	ExDate      time.Time
	AnnouncedAt time.Time
}

// ToEntry builds the journal entry for the action. The entry is an
// EntryCorporateAction whose Action the book folds against the position at the
// ex-date; the entry is bitemporal (ExDate effective, AnnouncedAt knowledge).
func (c CorporateAction) ToEntry() *ledger.Event {
	return &ledger.Event{
		EntryID:      "corpact:" + c.ActionID,
		PortfolioID:  c.PortfolioID,
		Type:         ledger.EntryCorporateAction,
		InstrumentID: c.InstrumentID,
		Action: &ledger.Action{
			Kind:     toLedgerKind(c.Kind),
			Ratio:    c.Ratio,
			PerUnit:  c.PerUnit,
			Target:   c.Target,
			Currency: c.Currency,
		},
		Effective: c.ExDate,
		Knowledge: c.AnnouncedAt,
		SourceRef: c.ActionID,
	}
}

func toLedgerKind(k Kind) ledger.CorpActKind {
	switch k {
	case Split:
		return ledger.CorpActSplit
	case Dividend:
		return ledger.CorpActDividend
	case Merger:
		return ledger.CorpActMerger
	case Coupon:
		return ledger.CorpActCoupon
	default:
		return ledger.CorpActUnspecified
	}
}
