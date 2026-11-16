package custody

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/jackc/pgx/v5"
)

// ApplyAction serializes retries before locking the break. The action, immutable
// evidence and FACT outbox entry commit together, or none of them do.
func (p *Postgres) ApplyAction(ctx context.Context, a Action) (ActionEvidence, error) {
	if err := a.validate(); err != nil {
		return ActionEvidence{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return ActionEvidence{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenant string
	if err = tx.QueryRow(ctx, `SELECT app_current_tenant()`).Scan(&tenant); err != nil {
		return ActionEvidence{}, err
	}
	if tenant != a.Tenant {
		return ActionEvidence{}, ErrNoBreak
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(app_current_tenant() || ':' || $1, 0))`, a.RequestID); err != nil {
		return ActionEvidence{}, err
	}
	// Reauthorize before even looking up a receipt: retries can outlive a
	// principal's portfolio grant. Ownership columns are immutable in this store.
	if err = authorizeActionBreak(ctx, tx, a, a.BreakID); err != nil {
		return ActionEvidence{}, err
	}
	var digest, receiptBreakID string
	var blob []byte
	err = tx.QueryRow(ctx, `SELECT request_digest, evidence, break_id FROM custody_actions WHERE request_id=$1`, a.RequestID).Scan(&digest, &blob, &receiptBreakID)
	if err == nil {
		// A reused request ID must not disclose whether another portfolio has a
		// receipt, or return its reviewed financial values.
		if receiptBreakID != a.BreakID {
			if err = authorizeActionBreak(ctx, tx, a, receiptBreakID); err != nil {
				return ActionEvidence{}, err
			}
		}
		if digest != a.digest() {
			return ActionEvidence{}, ErrActionConflict
		}
		var e ActionEvidence
		if err = json.Unmarshal(blob, &e); err != nil {
			return ActionEvidence{}, err
		}
		return e, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ActionEvidence{}, err
	}
	var b Break
	var status string
	var ibor, custodian, difference string
	b.BreakID = a.BreakID
	err = tx.QueryRow(ctx, `SELECT portfolio_id, custodian_id, status, assignee, explanation, revision, values_verified, ibor, custodian, difference FROM custody_breaks WHERE break_id=$1 FOR UPDATE`, a.BreakID).Scan(&b.PortfolioID, &b.CustodianID, &status, &b.Assignee, &b.Explanation, &b.Revision, &b.ValuesVerified, &ibor, &custodian, &difference)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActionEvidence{}, ErrNoBreak
	}
	if err != nil {
		return ActionEvidence{}, err
	}
	if !a.permits(b.PortfolioID) {
		return ActionEvidence{}, ErrNoBreak
	}
	b.Status = parseStatus(status)
	if !b.ValuesVerified {
		return ActionEvidence{}, ErrUnverifiedPrecision
	}
	texts := []string{ibor, custodian, difference}
	for i, text := range texts {
		value, err := parseStored(text)
		if err != nil {
			return ActionEvidence{}, err
		}
		exact, ok := ExactDecimalText(value)
		if !ok {
			return ActionEvidence{}, ErrUnverifiedPrecision
		}
		texts[i] = exact
	}
	after, e, err := applyAction(a, b, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return ActionEvidence{}, err
	}
	e.Values = &ActionValues{IBOR: texts[0], Custodian: texts[1], Difference: texts[2]}
	var revision int64
	err = tx.QueryRow(ctx, `UPDATE custody_breaks SET status=$2, assignee=$3, explanation=$4,
        status_changed_at=CASE WHEN status<>$2 THEN $5 ELSE status_changed_at END
        WHERE break_id=$1 RETURNING revision`, a.BreakID, after.Status.String(), after.Assignee, after.Explanation, e.RecordedAt).Scan(&revision)
	if err != nil {
		return ActionEvidence{}, err
	}
	if revision != after.Revision {
		return ActionEvidence{}, ErrStaleRevision
	}
	blob, err = json.Marshal(e)
	if err != nil {
		return ActionEvidence{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO custody_actions (request_id,break_id,actor,request_digest,evidence,recorded_at) VALUES ($1,$2,$3,$4,$5,$6)`, a.RequestID, a.BreakID, a.Actor, a.digest(), blob, e.RecordedAt); err != nil {
		return ActionEvidence{}, err
	}
	fact, err := actionFact(ctx, tenant, e)
	if err != nil {
		return ActionEvidence{}, err
	}
	if err = outbox.Enqueue(ctx, tx, outbox.ServiceAccounting, fact); err != nil {
		return ActionEvidence{}, err
	}
	return e, tx.Commit(ctx)
}

func authorizeActionBreak(ctx context.Context, tx pgx.Tx, a Action, breakID string) error {
	var portfolio string
	err := tx.QueryRow(ctx, `SELECT portfolio_id FROM custody_breaks WHERE break_id=$1`, breakID).Scan(&portfolio)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !a.permits(portfolio)) {
		return ErrNoBreak
	}
	return err
}
