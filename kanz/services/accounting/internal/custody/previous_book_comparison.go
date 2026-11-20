package custody

import (
	"context"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
)

// PreviousBookComparison opens the latest captured full comparison at the
// nearest earlier journal position of the same signed statement.
func (p *Postgres) PreviousBookComparison(ctx context.Context, subject Subject, account, runID string) (DatedResult, error) {
	unknown := DatedResult{State: "unknown"}
	tenant := bus.TenantIDFromContext(ctx)
	if tenant == "" || account == "" || !ValidReceiptID(runID) || subject.Validate() != nil {
		return unknown, nil
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DatedResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	anchor, err := historicalReceiptSnapshot(ctx, tx, tenant, subject, account, runID)
	if err != nil || anchor.State != "recorded" || !anchor.AccountAssignmentVerified ||
		!anchor.Run.Outcome.IsFullComparison() || anchor.Run.Basis.JournalPosition <= 0 {
		return unknown, err
	}
	position := anchor.Run.Basis.JournalPosition
	rows, err := tx.Query(ctx, `SELECT run_id,completed_at,(book_basis->>'journal_position')::bigint AS position
		FROM custody_runs
		WHERE tenant_id=app_current_tenant() AND tenant_id=$1 AND portfolio_id=$2 AND custodian_id=$3 AND business_date=$4
			AND statement_id=$5 AND signed_statement_sha256=decode($6,'hex')
			AND outcome IN ('clean','breaks','historical_clean','historical_breaks') AND values_verified AND failure_reason=''
			AND book_basis->>'mode'=$7 AND book_basis->'accounts'->$3::text=to_jsonb(ARRAY[$8::text])
			AND (book_basis->>'journal_position')::bigint >= 0 AND (book_basis->>'journal_position')::bigint < $9
			AND completed_at < $10
		ORDER BY position DESC,completed_at DESC,run_id LIMIT 2`, tenant, subject.PortfolioID, subject.CustodianID, BusinessDay(subject.BusinessDate),
		anchor.StatementID, anchor.SignedStatementSHA256, BasisDeclared, account, position, anchor.Run.CompletedAt.Truncate(time.Microsecond))
	if err != nil {
		return DatedResult{}, err
	}
	type candidate struct {
		runID    string
		at       time.Time
		position int64
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.runID, &c.at, &c.position)
		return c, err
	})
	// Run IDs identify receipts and supply no ordering evidence at a tied capture.
	if err != nil || len(runs) == 0 || (len(runs) == 2 && runs[0].position == runs[1].position && runs[0].at.Equal(runs[1].at)) {
		return unknown, err
	}
	previous, err := historicalReceiptSnapshot(ctx, tx, tenant, subject, account, runs[0].runID)
	if err != nil || previous.State != "recorded" || !previous.AccountAssignmentVerified || !previous.Run.Outcome.IsFullComparison() ||
		!anchor.Run.Basis.sameScope(previous.Run.Basis, subject.CustodianID) || !equalFigure(anchor.Run.Tolerance, previous.Run.Tolerance) {
		return unknown, err
	}
	previous.PreviousOfStatementID = anchor.StatementID
	previous.PreviousOfJournalPosition = &position
	return previous, nil
}
