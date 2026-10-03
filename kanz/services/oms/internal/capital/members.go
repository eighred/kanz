package capital

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

type member struct {
	owner                        string
	required, booked, executed   *big.Rat
	memberBooked, memberExecuted *big.Rat
}

// loadMember follows an immutable ownership link under the cash-balance lock.
// All members of an owner share that lock, so sibling updates cannot lose one
// another's debit or acquire commitment/member locks in opposite orders.
func loadMember(ctx context.Context, tx pgx.Tx, portfolio, currency, orderID string) (member, error) {
	var m member
	var required, booked, executed, memberBooked, memberExecuted string
	read := func() error {
		return tx.QueryRow(ctx, `SELECT c.order_id,c.required_debit,c.booked_debit,c.executed_debit,m.booked_debit,m.executed_debit
		FROM capital_members m JOIN capital_commitments c ON c.tenant_id=m.tenant_id AND c.order_id=m.owner_order_id
		WHERE m.order_id=$1 AND c.portfolio_id=$2 AND c.currency=$3 FOR UPDATE OF c,m`, orderID, portfolio, currency).
			Scan(&m.owner, &required, &booked, &executed, &memberBooked, &memberExecuted)
	}
	err := read()
	if errors.Is(err, pgx.ErrNoRows) {
		// Materialize pre-membership owners within the caller's tenant, under the
		// already-held cash lock. Copy the exact retained heads, never zero them.
		// An owner with existing children but no self-member is inconsistent: its
		// aggregate cannot safely be attributed to itself, so leave it refused.
		if _, insertErr := tx.Exec(ctx, `INSERT INTO capital_members(order_id,owner_order_id,booked_debit,executed_debit)
		SELECT c.order_id,c.order_id,c.booked_debit,c.executed_debit FROM capital_commitments c
		WHERE c.order_id=$1 AND c.portfolio_id=$2 AND c.currency=$3
		AND NOT EXISTS (SELECT 1 FROM capital_members m WHERE m.tenant_id=c.tenant_id AND m.owner_order_id=c.order_id)
		ON CONFLICT DO NOTHING`, orderID, portfolio, currency); insertErr != nil {
			return m, insertErr
		}
		err = read()
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrConflict
	}
	if err != nil {
		return m, err
	}
	values := []string{required, booked, executed, memberBooked, memberExecuted}
	outputs := []**big.Rat{&m.required, &m.booked, &m.executed, &m.memberBooked, &m.memberExecuted}
	for i, value := range values {
		*outputs[i], err = amount(value, true)
		if err != nil {
			return m, err
		}
	}
	return m, nil
}

// RegisterChild assigns an already-funded parent's budget to a child without
// increasing reserved cash. The order store must validate the scheduled slice
// and persist this link in the same admission transaction, under its parent
// cancellation barrier. This method is not an alternative admission authority.
func RegisterChild(ctx context.Context, tx pgx.Tx, portfolio, currency, owner, child string, now time.Time, maxAge time.Duration) error {
	if !validID(portfolio) || !validID(currency) || !validID(owner) || !validID(child) || owner == child || now.IsZero() || maxAge <= 0 {
		return ErrInvalid
	}
	fault, err := sourceFault(ctx, tx)
	if err != nil {
		return err
	}
	if fault {
		return ErrUnknown
	}
	b, err := lock(ctx, tx, portfolio, currency)
	if err != nil {
		return err
	}
	if !cashKnown(b, now, maxAge) {
		return ErrUnknown
	}
	if b.reserved.Cmp(b.total) > 0 {
		return ErrInsufficient
	}
	m, err := loadMember(ctx, tx, portfolio, currency, owner)
	if err != nil {
		return err
	}
	if m.owner != owner {
		return ErrInvalid
	} // nested ownership is never a schedule
	tag, err := tx.Exec(ctx, `INSERT INTO capital_members(order_id,owner_order_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, child, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
