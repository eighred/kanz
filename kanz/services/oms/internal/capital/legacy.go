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
func ImportLegacy(ctx context.Context, tx pgx.Tx, portfolio, currency, orderID string, required, executed dec.Exact) error {
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
	tag, err := tx.Exec(ctx, `INSERT INTO capital_commitments(order_id,portfolio_id,currency,required_debit,executed_debit)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, orderID, portfolio, currency, want.RatString(), actual.RatString())
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

// ImportLegacyChild attributes executions to an existing parent without
// reserving a second copy of the same scheduled quantity.
func ImportLegacyChild(ctx context.Context, tx pgx.Tx, portfolio, currency, owner, child string) error {
	if !validID(portfolio) || !validID(currency) || !validID(owner) || !validID(child) || owner == child {
		return ErrInvalid
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
	tag, err := tx.Exec(ctx, `INSERT INTO capital_members(order_id,owner_order_id,currency) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, child, owner, currency)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
