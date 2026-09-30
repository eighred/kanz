package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// ActionLifecycle is immutable announcement evidence. PaidAt is an observed
// payment, not a forecast inferred from PayDate. Revisions replace economics at
// the knowledge cutoff; cancellation retains the history without its effect.
type ActionLifecycle struct {
	ActionID   string    `json:"action_id,omitempty"`
	Revision   uint64    `json:"revision,omitempty"`
	Cancelled  bool      `json:"cancelled,omitempty"`
	PayDate    time.Time `json:"pay_date,omitempty"`
	PaidAt     time.Time `json:"paid_at,omitempty"`
	PaymentRef string    `json:"payment_ref,omitempty"`
}

var ErrActionConflict = errors.New("ledger: conflicting corporate-action revision")

// ActionEntryID scopes the immutable identity to the book as well as the event.
func ActionEntryID(portfolio, action string, revision uint64) string {
	b, _ := json.Marshal([]any{portfolio, action, revision})
	return fmt.Sprintf("corpact:%x", sha256.Sum256(b))
}

// ValidateActionEntry is shared by the mapper and both durable-store seams.
// Legacy unversioned records remain readable, but cannot be newly appended.
func ValidateActionEntry(e *Event) error {
	if e == nil || e.Type != EntryCorporateAction || e.Action == nil {
		return errors.New("ledger: missing corporate action")
	}
	a := e.Action
	if a.ActionID == "" || a.Revision == 0 || e.PortfolioID == "" || e.InstrumentID == "" || e.Effective.IsZero() || e.Knowledge.IsZero() || e.actionStage != 0 || e.EntryID != ActionEntryID(e.PortfolioID, a.ActionID, a.Revision) {
		return errors.New("ledger: invalid corporate-action identity or times")
	}
	if a.Kind < CorpActSplit || a.Kind > CorpActCoupon || e.Quantity != nil || e.Price != nil || e.Cash != nil || e.VenueAccountID != "" || e.SettlementBasis != SettlementUnknown || !e.SettlementDate.IsZero() || e.CashCurrency != "" || len(e.ExecutionEvidence) != 0 {
		return errors.New("ledger: invalid corporate-action shape")
	}
	for _, at := range []time.Time{e.Effective, e.Knowledge, a.PayDate, a.PaidAt} {
		if !at.IsZero() && (at.Year() < 1 || at.Year() > 9999 || !at.Equal(at.Truncate(time.Microsecond))) {
			return errors.New("ledger: corporate-action timestamps require PostgreSQL microsecond precision")
		}
	}
	if a.Cancelled {
		if !a.PaidAt.IsZero() || a.PaymentRef != "" {
			return errors.New("ledger: cancellation cannot assert payment")
		}
		return nil
	}
	if a.Kind == CorpActSplit && (a.PerUnit != nil || a.Target != "" || a.Currency != "") {
		return errors.New("ledger: unexpected split consideration")
	}
	if (a.Kind == CorpActDividend || a.Kind == CorpActCoupon) && (a.Ratio != nil || a.Target != "") {
		return errors.New("ledger: unexpected income conversion")
	}
	if a.Kind == CorpActSplit || a.Kind == CorpActMerger {
		if a.Ratio == nil || a.Ratio.Sign() <= 0 {
			return errors.New("ledger: positive conversion ratio required")
		}
		if a.Kind == CorpActMerger && (a.Target == "" || a.Target == e.InstrumentID) {
			return errors.New("ledger: merger target required")
		}
	}
	if a.Kind == CorpActDividend || a.Kind == CorpActCoupon {
		if a.PerUnit == nil || a.PerUnit.Sign() < 0 || a.Currency == "" || a.PayDate.IsZero() || a.PayDate.Before(e.Effective) {
			return errors.New("ledger: income requires exact periodic amount, currency and pay date")
		}
	} else if !a.PayDate.IsZero() || !a.PaidAt.IsZero() || a.PaymentRef != "" {
		return errors.New("ledger: payment lifecycle applies only to dividend/coupon")
	}
	if a.PerUnit != nil && (a.PerUnit.Sign() < 0 || a.Currency == "") {
		return errors.New("ledger: invalid cash consideration")
	}
	if a.PaidAt.IsZero() != (a.PaymentRef == "") || (!a.PaidAt.IsZero() && (a.PaidAt.Before(a.PayDate) || a.PaidAt.After(e.Knowledge))) {
		return errors.New("ledger: invalid payment evidence")
	}
	return nil
}

func sameActionEntry(a, b *Event) bool {
	x, err := encodeAction(a.Action)
	if err != nil {
		return false
	}
	y, err := encodeAction(b.Action)
	if err != nil {
		return false
	}
	xb, xok := x.([]byte)
	yb, yok := y.([]byte)
	return a.EntryID == b.EntryID && a.PortfolioID == b.PortfolioID && a.InstrumentID == b.InstrumentID && a.Effective.Equal(b.Effective) && a.Knowledge.Equal(b.Knowledge) && a.SourceRef == b.SourceRef && xok && yok && bytes.Equal(xb, yb)
}

func checkActionRevision(e, head *Event) (bool, error) {
	if err := ValidateActionEntry(e); err != nil {
		return false, err
	}
	if head == nil {
		if e.Action.Revision != 1 {
			return false, ErrActionConflict
		}
		return false, nil
	}
	if e.Action.Revision == head.Action.Revision && sameActionEntry(e, head) {
		return true, nil
	}
	if e.Action.Revision != head.Action.Revision+1 || !e.Knowledge.After(head.Knowledge) {
		return false, ErrActionConflict
	}
	return false, nil
}

// actionEvents resolves revisions BEFORE filtering effective dates. Otherwise
// moving an ex-date forward resurrects a superseded announcement in a PIT read.
// Staged copies make this transformation idempotent for scoped/PIT readers.
func actionEvents(events []*Event, knowBound time.Time) []*Event {
	heads := make(map[[2]string]uint64)
	for _, e := range events {
		if e == nil || (!knowBound.IsZero() && e.Knowledge.After(knowBound)) {
			continue
		}
		if a := e.Action; a != nil && a.ActionID != "" {
			key := [2]string{e.PortfolioID, a.ActionID}
			if a.Revision > heads[key] {
				heads[key] = a.Revision
			}
		}
	}
	out := make([]*Event, 0, len(events))
	for _, e := range events {
		if e == nil || (!knowBound.IsZero() && e.Knowledge.After(knowBound)) {
			continue
		}
		a := e.Action
		if a == nil || a.ActionID == "" {
			out = append(out, e)
			continue
		}
		if a.Revision != heads[[2]string{e.PortfolioID, a.ActionID}] || a.Cancelled {
			continue
		}
		if e.actionStage != 0 || (a.Kind != CorpActDividend && a.Kind != CorpActCoupon) {
			out = append(out, e)
			continue
		}
		accrual := *e
		accrual.actionStage = 1
		out = append(out, &accrual)
		if !a.PaidAt.IsZero() {
			payment := *e
			payment.actionStage = 2
			payment.EntryID += ":payment"
			payment.Effective = a.PaidAt
			out = append(out, &payment)
		}
	}
	return out
}

func (b *Book) foldIncome(e *Event) {
	a := e.Action
	if e.actionStage != 2 {
		if a.ActionID == "" {
			b.unknownSettlement++
		} else {
			b.pendingSettlement++
		}
	}
	key := ActionEntryID(e.PortfolioID, a.ActionID, a.Revision)
	if e.actionStage == 2 {
		b.pendingSettlement--
		amount := b.entitlements[key]
		if amount == nil {
			return
		}
		add(b.Accrued, a.Currency, new(big.Rat).Neg(amount))
		add(b.Cash, a.Currency, amount)
		add(b.SettledCash, a.Currency, amount)
		delete(b.entitlements, key)
		return
	}
	lot := b.Positions[e.InstrumentID]
	if lot == nil || a.PerUnit == nil {
		return
	}
	amount := new(big.Rat).Mul(lot.Qty, a.PerUnit)
	b.entitlements[key] = amount
	add(b.Accrued, a.Currency, amount)
}

func checkPostgresAction(ctx context.Context, tx pgx.Tx, e *Event) (bool, error) {
	if err := ValidateActionEntry(e); err != nil {
		return false, err
	}
	read := func(duplicate bool) (*Event, error) {
		var old Event
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT entry_id, portfolio_id, instrument_id, effective_time, knowledge_time, source_ref, action
		 FROM ledger_entries WHERE
		 ($1 AND entry_id = $2) OR
		 (NOT $1 AND portfolio_id = $3 AND action->>'action_id' = $4)
		 ORDER BY (action->>'revision')::numeric DESC NULLS LAST LIMIT 1`, duplicate, e.EntryID, e.PortfolioID, e.Action.ActionID).
			Scan(&old.EntryID, &old.PortfolioID, &old.InstrumentID, &old.Effective, &old.Knowledge, &old.SourceRef, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		old.Action, err = decodeAction(raw)
		return &old, err
	}
	old, err := read(true)
	if err != nil {
		return false, err
	}
	if old != nil {
		if old.Action == nil || !sameActionEntry(e, old) {
			return false, ErrActionConflict
		}
		return true, nil
	}
	head, err := read(false)
	if err != nil {
		return false, err
	}
	return checkActionRevision(e, head)
}
