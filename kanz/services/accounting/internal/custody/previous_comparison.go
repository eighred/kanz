package custody

import (
	"context"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
)

// PreviousComparison opens the first full comparison for the immediately
// preceding signed revision of this close. A manual rerun can have produced
// another receipt for that revision. Equal earliest times identify no first run.
func (p *Postgres) PreviousComparison(ctx context.Context, subject Subject, account, runID string) (DatedResult, error) {
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
	current, err := historicalReceiptSnapshot(ctx, tx, tenant, subject, account, runID)
	if err != nil || !current.AccountAssignmentVerified || current.Run.Outcome != OutcomeClean {
		return unknown, err
	}
	rows, err := tx.Query(ctx, `SELECT r.run_id,r.completed_at FROM custody_batch_receipts c
		JOIN custody_batch_receipts p ON (p.tenant_id,p.portfolio_id,p.custodian_id,p.business_date,p.account_id,p.source_id,p.public_key)=
			(c.tenant_id,c.portfolio_id,c.custodian_id,c.business_date,c.account_id,c.source_id,c.public_key)
			AND p.revision=c.revision-1 AND p.sha256=c.previous_sha256
		JOIN custody_runs r ON (r.tenant_id,r.portfolio_id,r.custodian_id,r.business_date,r.statement_id)=
			(p.tenant_id,p.portfolio_id,p.custodian_id,p.business_date,p.statement_id)
		WHERE c.tenant_id=app_current_tenant() AND c.portfolio_id=$1 AND c.custodian_id=$2 AND c.business_date=$3
			AND c.account_id=$4 AND c.statement_id=$5 AND c.sha256=$6
			AND r.signed_statement_sha256=decode(p.sha256,'hex')
			AND r.outcome IN ('clean','breaks') AND r.values_verified AND r.failure_reason=''
		ORDER BY r.completed_at,r.run_id LIMIT 2`, subject.PortfolioID, subject.CustodianID, subject.BusinessDate,
		account, current.StatementID, current.SignedStatementSHA256)
	if err != nil {
		return DatedResult{}, err
	}
	type candidate struct {
		runID string
		at    time.Time
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.runID, &c.at)
		return c, err
	})
	if err != nil || len(runs) == 0 || len(runs) == 2 && runs[0].at.Equal(runs[1].at) {
		return unknown, err
	}
	previous, err := historicalReceiptSnapshot(ctx, tx, tenant, subject, account, runs[0].runID)
	if err != nil || !previous.AccountAssignmentVerified || !current.Run.Basis.sameScope(previous.Run.Basis, subject.CustodianID) ||
		previous.Run.Basis.JournalPosition > current.Run.Basis.JournalPosition || previous.Run.CompletedAt.After(current.Run.CompletedAt) {
		return unknown, err
	}
	previous.PreviousOfStatementID = current.StatementID
	return previous, nil
}
