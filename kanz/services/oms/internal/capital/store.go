// Package capital coordinates exact pending cash commitments in the same
// PostgreSQL transaction that admits or changes an OMS order.
package capital

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUnknown      = errors.New("capital: cash coverage unknown, stale, or incomplete")
	ErrConflict     = errors.New("capital: conflicting identity or version")
	ErrInsufficient = errors.New("capital: pending commitments exceed spendable cash")
	ErrInvalid      = errors.New("capital: invalid exact commitment")
)

// Applied is cumulative debit already INCLUDED in the event's cash total.
// Accounting must emit every change, including fee corrections and reversals.
// It is not an OMS fill observation and must never be built from one.
type Applied struct {
	OrderID string
	Debit   dec.Exact
}

// CashEvent is an ordered accounting commit. The transport must retain and
// replay its complete sequence; a compacted cash-level subscription is unsafe.
// First revision requires an authoritative opening balance and no unaccounted
// legacy orders. It is not permission to infer a zero opening balance.
type CashEvent struct {
	PortfolioID  string
	Currency     string
	Revision     int64
	Total        dec.Exact
	ObservedAt   time.Time
	Complete     bool     // explicitly established source and execution coverage
	SourceDigest [32]byte // binds the complete accounting payload, including source posture
	Applied      []Applied
}

type balance struct {
	revision, gap   int64
	conflicted      bool
	complete        bool
	total, reserved *big.Rat
	observed        *int64
}

func validID(s string) bool { return strings.TrimSpace(s) != "" && len(s) <= 256 }

func amount(s string, nonnegative bool) (*big.Rat, error) {
	r, err := dec.Exact(s).Rat()
	if err != nil || (nonnegative && r.Sign() < 0) {
		return nil, ErrInvalid
	}
	return r, nil
}

func need(required, booked *big.Rat) *big.Rat {
	n := new(big.Rat).Sub(required, booked)
	if n.Sign() < 0 {
		n.SetInt64(0)
	}
	return n
}

func lock(ctx context.Context, tx pgx.Tx, portfolio, currency string) (balance, error) {
	var b balance
	var total, reserved string
	err := tx.QueryRow(ctx, `SELECT revision,gap_revision,conflicted,coverage_complete,total,reserved,observed_at_ns FROM capital_balances
		WHERE portfolio_id=$1 AND currency=$2 FOR UPDATE`, portfolio, currency).Scan(&b.revision, &b.gap, &b.conflicted, &b.complete, &total, &reserved, &b.observed)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrUnknown
	}
	if err != nil {
		return b, err
	}
	b.total, err = amount(total, false)
	if err != nil {
		return b, err
	}
	b.reserved, err = amount(reserved, true)
	return b, err
}

// Reserve must run inside the order-admission transaction. A rollback releases
// both writes. Reusing an existing identity is refused even when amounts match;
// the order store, which owns idempotency, must detect its duplicate first.
func Reserve(ctx context.Context, tx pgx.Tx, portfolio, currency, orderID string, debit dec.Exact, now time.Time, maxAge time.Duration) error {
	if !validID(portfolio) || !validID(currency) || !validID(orderID) || now.IsZero() || maxAge <= 0 {
		return ErrInvalid
	}
	required, err := amount(string(debit), true)
	if err != nil {
		return err
	}
	b, err := lock(ctx, tx, portfolio, currency)
	if err != nil {
		return err
	}
	if !b.complete || b.conflicted || b.revision == 0 || b.gap > b.revision || b.observed == nil || time.Unix(0, *b.observed).After(now) || now.Sub(time.Unix(0, *b.observed)) > maxAge {
		return ErrUnknown
	}
	next := new(big.Rat).Add(b.reserved, required)
	if next.Cmp(b.total) > 0 {
		return ErrInsufficient
	}
	if _, err := amount(next.RatString(), true); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO capital_commitments(order_id,portfolio_id,currency,required_debit)
		VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, orderID, portfolio, currency, required.RatString())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return writeReserved(ctx, tx, portfolio, currency, next)
}

// Change replaces total required debit (executed debit plus the worst remaining
// debit). A terminal order still requires its executed debit until accounting
// proves inclusion. Cancellation is therefore not a request to set this to zero.
// Increasing exposure requires fresh complete cash; reducing exposure does not.
func Change(ctx context.Context, tx pgx.Tx, portfolio, currency, orderID string, version int64, debit dec.Exact, now time.Time, maxAge time.Duration) error {
	if !validID(portfolio) || !validID(currency) || !validID(orderID) || version <= 0 || now.IsZero() || maxAge <= 0 {
		return ErrInvalid
	}
	required, err := amount(string(debit), true)
	if err != nil {
		return err
	}
	b, err := lock(ctx, tx, portfolio, currency)
	if err != nil {
		return err
	}
	var oldText, bookedText, executedText string
	var actual int64
	err = tx.QueryRow(ctx, `SELECT required_debit,booked_debit,executed_debit,version FROM capital_commitments WHERE order_id=$1 AND portfolio_id=$2 AND currency=$3 FOR UPDATE`, orderID, portfolio, currency).Scan(&oldText, &bookedText, &executedText, &actual)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && actual != version {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	executed, err := amount(executedText, true)
	if err != nil {
		return err
	}
	if required.Cmp(executed) < 0 {
		return ErrInvalid
	}
	old, err := amount(oldText, true)
	if err != nil {
		return err
	}
	booked, err := amount(bookedText, true)
	if err != nil {
		return err
	}
	before, after := need(old, booked), need(required, booked)
	next := new(big.Rat).Add(new(big.Rat).Sub(b.reserved, before), after)
	if after.Cmp(before) > 0 {
		if !b.complete || b.conflicted || b.revision == 0 || b.gap > b.revision || b.observed == nil || time.Unix(0, *b.observed).After(now) || now.Sub(time.Unix(0, *b.observed)) > maxAge {
			return ErrUnknown
		}
		if next.Cmp(b.total) > 0 {
			return ErrInsufficient
		}
	}
	if _, err := amount(next.RatString(), true); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE capital_commitments SET required_debit=$2,version=version+1 WHERE order_id=$1`, orderID, required.RatString()); err != nil {
		return err
	}
	return writeReserved(ctx, tx, portfolio, currency, next)
}

// ObserveExecution records cumulative actual debit with the authoritative OMS
// fill/correction transaction. An execution above budget still happened: retain
// the liability, even if doing so makes all subsequent admissions unaffordable.
// This observation never releases a commitment; Change requires a separate
// confirmed reduction in the remaining executable exposure.
func ObserveExecution(ctx context.Context, tx pgx.Tx, portfolio, currency, orderID string, debit dec.Exact) error {
	if !validID(portfolio) || !validID(currency) || !validID(orderID) {
		return ErrInvalid
	}
	executed, err := amount(string(debit), true)
	if err != nil {
		return err
	}
	b, err := lock(ctx, tx, portfolio, currency)
	if err != nil {
		return err
	}
	var requiredText, bookedText, executedText string
	err = tx.QueryRow(ctx, `SELECT required_debit,booked_debit,executed_debit FROM capital_commitments WHERE order_id=$1 AND portfolio_id=$2 AND currency=$3 FOR UPDATE`, orderID, portfolio, currency).Scan(&requiredText, &bookedText, &executedText)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	previous, err := amount(executedText, true)
	if err != nil {
		return err
	}
	if previous.Cmp(executed) == 0 {
		return nil
	}
	required, err := amount(requiredText, true)
	if err != nil {
		return err
	}
	booked, err := amount(bookedText, true)
	if err != nil {
		return err
	}
	before := need(required, booked)
	if executed.Cmp(required) > 0 {
		required.Set(executed)
	}
	b.reserved.Sub(b.reserved, before)
	b.reserved.Add(b.reserved, need(required, booked))
	if _, err := amount(b.reserved.RatString(), true); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE capital_commitments SET executed_debit=$2,required_debit=$3,version=version+1 WHERE order_id=$1`, orderID, executed.RatString(), required.RatString()); err != nil {
		return err
	}
	return writeReserved(ctx, tx, portfolio, currency, b.reserved)
}

func writeReserved(ctx context.Context, tx pgx.Tx, portfolio, currency string, reserved *big.Rat) error {
	if _, err := amount(reserved.RatString(), true); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE capital_balances SET reserved=$3 WHERE portfolio_id=$1 AND currency=$2`, portfolio, currency, reserved.RatString())
	return err
}

// Apply commits an accounting event or a durable gap latch. ErrUnknown for a
// gap means admission was blocked successfully, while the source event must be
// retried after restoring the missing prefix. No wall-clock guess releases cash.
func Apply(ctx context.Context, pool *pgxpool.Pool, event CashEvent) error {
	if !validID(event.PortfolioID) || !validID(event.Currency) || event.Revision <= 0 || event.ObservedAt.IsZero() || len(event.Applied) > 4096 {
		return ErrInvalid
	}
	if !time.Unix(0, event.ObservedAt.UnixNano()).Equal(event.ObservedAt) {
		return ErrInvalid
	}
	total, err := amount(string(event.Total), false)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, a := range event.Applied {
		if !validID(a.OrderID) || seen[a.OrderID] {
			return ErrInvalid
		}
		seen[a.OrderID] = true
		if _, err := amount(string(a.Debit), true); err != nil {
			return err
		}
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `INSERT INTO capital_balances(portfolio_id,currency) VALUES($1,$2) ON CONFLICT DO NOTHING`, event.PortfolioID, event.Currency); err != nil {
		return err
	}
	b, err := lock(ctx, tx, event.PortfolioID, event.Currency)
	if err != nil {
		return err
	}
	// Retain identities even when a gap prevents application, so a later
	// conflicting delivery cannot replace the event that first exposed the gap.
	if _, err = tx.Exec(ctx, `INSERT INTO capital_cash_events(portfolio_id,currency,revision,digest) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, event.PortfolioID, event.Currency, event.Revision, digest[:]); err != nil {
		return err
	}
	var retained []byte
	if err = tx.QueryRow(ctx, `SELECT digest FROM capital_cash_events WHERE portfolio_id=$1 AND currency=$2 AND revision=$3`, event.PortfolioID, event.Currency, event.Revision).Scan(&retained); err != nil {
		return err
	}
	if !bytes.Equal(retained, digest[:]) {
		return latchConflict(ctx, tx, event.PortfolioID, event.Currency)
	}
	if event.Revision <= b.revision {
		return tx.Commit(ctx)
	}
	if event.Revision != b.revision+1 {
		if _, err = tx.Exec(ctx, `UPDATE capital_balances SET gap_revision=GREATEST(gap_revision,$3) WHERE portfolio_id=$1 AND currency=$2`, event.PortfolioID, event.Currency, event.Revision); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		return ErrUnknown
	}
	if b.observed != nil && event.ObservedAt.Before(time.Unix(0, *b.observed)) {
		return latchConflict(ctx, tx, event.PortfolioID, event.Currency)
	}
	if _, err = tx.Exec(ctx, `SAVEPOINT cash_proofs`); err != nil {
		return err
	}
	for _, a := range event.Applied {
		var requiredText, bookedText string
		err = tx.QueryRow(ctx, `SELECT required_debit,booked_debit FROM capital_commitments WHERE order_id=$1 AND portfolio_id=$2 AND currency=$3 FOR UPDATE`, a.OrderID, event.PortfolioID, event.Currency).Scan(&requiredText, &bookedText)
		// A proof for an unknown commitment cannot be silently discarded; that
		// would permit the same order identity to acquire new spending capacity.
		if errors.Is(err, pgx.ErrNoRows) {
			// Preserve the conflicting receipt and quarantine, but undo every
			// earlier application in this event before committing the latch.
			if _, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT cash_proofs`); err != nil {
				return err
			}
			return latchConflict(ctx, tx, event.PortfolioID, event.Currency)
		}
		if err != nil {
			return err
		}
		required, err := amount(requiredText, true)
		if err != nil {
			return err
		}
		before, err := amount(bookedText, true)
		if err != nil {
			return err
		}
		after, err := amount(string(a.Debit), true)
		if err != nil {
			return err
		}
		b.reserved.Sub(b.reserved, need(required, before))
		b.reserved.Add(b.reserved, need(required, after))
		if _, err = tx.Exec(ctx, `UPDATE capital_commitments SET booked_debit=$2 WHERE order_id=$1`, a.OrderID, after.RatString()); err != nil {
			return err
		}
	}
	if err = writeReserved(ctx, tx, event.PortfolioID, event.Currency, b.reserved); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE capital_balances SET revision=$3,total=$4,observed_at_ns=$5,coverage_complete=$6 WHERE portfolio_id=$1 AND currency=$2`, event.PortfolioID, event.Currency, event.Revision, total.RatString(), event.ObservedAt.UnixNano(), event.Complete); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Replaying the first payload does not resolve contradictory accounting
// evidence. Keep admission quarantined until an operator repairs the source
// and rebuilds this projection from verified history under the launch gates.
func latchConflict(ctx context.Context, tx pgx.Tx, portfolio, currency string) error {
	if _, err := tx.Exec(ctx, `UPDATE capital_balances SET conflicted=true WHERE portfolio_id=$1 AND currency=$2`, portfolio, currency); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return ErrConflict
}
