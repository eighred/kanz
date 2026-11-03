package capital

import (
	"context"
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	"github.com/jackc/pgx/v5"
)

// ImportLegacy creates a conservative obligation BEFORE retained accounting
// history is replayed. It cannot run after a cash prefix has been applied: a
// historical receipt for an unknown order would already have quarantined that
// currency. This intentionally does not require a known cash total; the zero
// revision balance stays incomplete until the real accounting opening arrives.
func ImportLegacy(ctx context.Context, tx pgx.Tx, portfolio, currency, orderID string, required, executed, admissionLimit dec.Exact) error {
	if !validID(portfolio) || !validID(currency) || !validID(orderID) {
		return ErrInvalid
	}
	want, err := amount(string(required), true)
	if err != nil {
		return err
	}
	actual, err := amount(string(executed), true)
	if err != nil || want.Cmp(actual) < 0 {
		return ErrInvalid
	}
	limit, err := amount(string(admissionLimit), true)
	if err != nil {
		return err
	}
	// Import precedes the first accounting cash receipt. Initialize the tenant's
	// corruption latch exactly as Apply does; revision zero still leaves every
	// cash balance UNKNOWN until authenticated history establishes coverage.
	if _, err := tx.Exec(ctx, `INSERT INTO capital_source_health DEFAULT VALUES ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	faulted, err := sourceFault(ctx, tx)
	if err != nil {
		return err
	}
	if faulted {
		return ErrUnknown
	}
	if _, err := tx.Exec(ctx, `INSERT INTO capital_balances(portfolio_id,currency) VALUES($1,$2) ON CONFLICT DO NOTHING`, portfolio, currency); err != nil {
		return err
	}
	b, err := lock(ctx, tx, portfolio, currency)
	if err != nil {
		return err
	}
	if b.revision != 0 || b.gap != 0 || b.conflicted || b.complete || b.observed != nil {
		return ErrUnknown
	}
	next := new(big.Rat).Add(b.reserved, want)
	if _, err := amount(next.RatString(), true); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO capital_commitments(order_id,portfolio_id,currency,required_debit,executed_debit,admission_debit)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, orderID, portfolio, currency, want.RatString(), actual.RatString(), limit.RatString())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO capital_members(order_id,owner_order_id,currency,executed_debit) VALUES($1,$1,$2,$3)`, orderID, currency, actual.RatString()); err != nil {
		return err
	}
	return writeReserved(ctx, tx, portfolio, currency, next)
}

// ImportLegacyChild attributes executions and approved child allocation to an
// existing parent without reserving a second copy of scheduled cash. A later
// child must see this historical allocation before it can use the parent cap.
func ImportLegacyChild(ctx context.Context, tx pgx.Tx, portfolio, currency, owner, child string, debit dec.Exact) error {
	if !validID(portfolio) || !validID(currency) || !validID(owner) || !validID(child) || owner == child {
		return ErrInvalid
	}
	allocation, err := amount(string(debit), true)
	if err != nil {
		return err
	}
	faulted, err := sourceFault(ctx, tx)
	if err != nil {
		return err
	}
	if faulted {
		return ErrUnknown
	}
	b, err := lock(ctx, tx, portfolio, currency)
	if err != nil {
		return err
	}
	if b.revision != 0 || b.gap != 0 || b.conflicted || b.complete {
		return ErrUnknown
	}
	m, err := loadMember(ctx, tx, portfolio, currency, owner)
	if err != nil {
		return err
	}
	if m.owner != owner {
		return ErrInvalid
	}
	var limitText, allocatedText *string
	if err := tx.QueryRow(ctx, `SELECT admission_debit,allocated_debit FROM capital_commitments WHERE order_id=$1 AND currency=$2 AND portfolio_id=$3 FOR UPDATE`, owner, currency, portfolio).Scan(&limitText, &allocatedText); err != nil {
		return err
	}
	if limitText == nil || allocatedText == nil {
		return ErrUnknown
	}
	limit, err := amount(*limitText, true)
	if err != nil {
		return err
	}
	allocated, err := amount(*allocatedText, true)
	if err != nil {
		return err
	}
	next := new(big.Rat).Add(allocated, allocation)
	if next.Cmp(limit) > 0 {
		return ErrInvalid
	}
	tag, err := tx.Exec(ctx, `INSERT INTO capital_members(order_id,owner_order_id,currency,allocated_debit) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, child, owner, currency, allocation.RatString())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE capital_commitments SET allocated_debit=$2 WHERE order_id=$1 AND currency=$3`, owner, next.RatString(), currency)
	return err
}
