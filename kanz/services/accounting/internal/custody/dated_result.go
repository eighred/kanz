package custody

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/jackc/pgx/v5"
)

// DatedResult describes what the recorded comparison establishes for one UTC close.
// A historical lookup also returns the digest of the specific verified signed revision.
type DatedResult struct {
	HistoricalQuantityCorrectionRunID string
	AccountAssignmentVerified         bool
	State                             string
	Reason                            string
	StatementID                       string
	SignedStatementSHA256             string
	PreviousOfStatementID             string
	PreviousOfJournalPosition         *int64
	reportedTransactions              []recon.Transaction
	Run                               Run
}

func (p TransactionComparison) matches(stmt Statement, outcome Outcome, prefix int64) bool {
	switch stmt.Grain {
	case recon.GrainBalancesOnly:
		return p.Pass == recon.LegBalancesOnly.String() && p.StatementReferences == nil && p.BookReferences == nil && p.UnreferencedBookTrades == nil
	case recon.GrainTransactions:
		return p.Pass == recon.LegRan.String() && p.StatementReferences != nil && *p.StatementReferences == len(stmt.Transactions) &&
			p.BookReferences != nil && *p.BookReferences >= 0 && p.UnreferencedBookTrades != nil && *p.UnreferencedBookTrades >= 0 &&
			int64(*p.UnreferencedBookTrades) <= prefix-int64(*p.BookReferences) &&
			((outcome != OutcomeClean && outcome != OutcomeHistoricalClean) || *p.BookReferences == *p.StatementReferences)
	}
	return false
}

func (p *Postgres) DatedResult(ctx context.Context, subject Subject) (DatedResult, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DatedResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenant string
	if err := tx.QueryRow(ctx, "SELECT app_current_tenant()").Scan(&tenant); err != nil {
		return DatedResult{}, err
	}
	if tenant == "" || tenant != bus.TenantIDFromContext(ctx) {
		return DatedResult{}, errors.New("custody: dated result tenant mismatch")
	}
	args := []any{subject.PortfolioID, subject.CustodianID, BusinessDay(subject.BusinessDate)}
	// Comparing the signed intake frontier and the materialized statement in the
	// same snapshot distinguishes pending delivery from a genuinely missing day.
	newest, err := latestSignedStatementID(ctx, tx, subject)
	if err != nil {
		return DatedResult{}, err
	}
	materialized, err := (&Postgres{pool: p.pool, tx: tx}).LatestStatement(ctx, subject)
	if errors.Is(err, ErrAmbiguousStatement) || errors.Is(err, ErrUnverifiedPrecision) {
		return DatedResult{State: "unknown"}, nil
	}
	if err != nil && !errors.Is(err, ErrNoStatement) {
		return DatedResult{}, err
	}
	id := materialized.StatementID
	if id != newest {
		if newest == "" || (id != "" && !isBatchStatement(id)) {
			return DatedResult{State: "unknown"}, nil
		}
		if id != "" {
			var currentApplied, priorSigned bool
			err := tx.QueryRow(ctx, `SELECT
                EXISTS(SELECT 1 FROM custody_statements WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3 AND statement_id=$4),
                EXISTS(SELECT 1 FROM custody_batch_receipts WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3 AND statement_id=$5)`, append(args, newest, id)...).Scan(&currentApplied, &priorSigned)
			if err != nil {
				return DatedResult{}, err
			}
			if currentApplied || !priorSigned {
				return DatedResult{State: "unknown"}, nil
			}
		}
		return DatedResult{State: "pending", StatementID: newest}, nil
	}

	// A later diagnostic cannot replace a successful comparison of the current
	// statement. A larger journal position takes precedence over completion time.
	// Conflicting outcomes/bases at the same position leave the result unknown.
	const result = `WITH candidates AS (
        SELECT run_id,outcome,completed_at,book_basis,tolerance,values_verified,signed_statement_sha256,
               outcome IN ('clean','breaks','historical_clean','historical_breaks') AS compared
        FROM custody_runs
        WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3 AND statement_id=$4
    ), best AS (
        SELECT * FROM candidates ORDER BY compared DESC,(outcome='historical_quantity_correction') DESC,
            CASE WHEN compared THEN (book_basis->>'journal_position')::bigint END DESC NULLS LAST,
            CASE WHEN compared THEN signed_statement_sha256 IS NOT NULL END DESC,
            CASE WHEN compared THEN book_basis->'assignments' IS NOT NULL END DESC,
            CASE WHEN compared AND book_basis->'assignments' IS NOT NULL THEN completed_at END DESC NULLS LAST,
            CASE WHEN NOT compared THEN completed_at END DESC NULLS LAST
        FETCH FIRST 1 ROW WITH TIES
    ) SELECT run_id,outcome,completed_at,book_basis,signed_statement_sha256,
        (SELECT bool_and(values_verified) AND count(DISTINCT (outcome,tolerance,signed_statement_sha256,book_basis->>'mode',book_basis->'accounts'->$2))=1
          AND count(DISTINCT book_basis->'transactions')<=1 AND count(DISTINCT book_basis->'assignments')<=1 FROM best)
        FROM best ORDER BY (book_basis->'transactions' IS NOT NULL) DESC,completed_at DESC,run_id LIMIT 1`
	var run Run
	var outcome string
	var binding []byte
	var consistent bool
	err = tx.QueryRow(ctx, result, append(args, id)...).Scan(&run.RunID, &outcome, &run.CompletedAt, &run.Basis, &binding, &consistent)
	if errors.Is(err, pgx.ErrNoRows) {
		var hasRun bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM custody_runs WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3)`, args...).Scan(&hasRun)
		if err != nil {
			return DatedResult{}, err
		}
		if !hasRun {
			return DatedResult{State: "not_compared", StatementID: id}, nil
		}
		if id != "" {
			return DatedResult{State: "pending", StatementID: id}, nil
		}
		return DatedResult{State: "unknown"}, nil
	}
	if err != nil {
		return DatedResult{}, err
	}
	run.Subject, run.StatementID, run.Outcome = subject, id, parseOutcome(outcome)
	if !consistent || run.Outcome == OutcomeUnspecified || (run.Outcome == OutcomeNoStatement && id != "") || run.RunID == "" || run.CompletedAt.IsZero() {
		return DatedResult{State: "unknown"}, nil
	}
	verifiedAssignment := false
	if run.Outcome.IsFullComparison() || run.Outcome == OutcomeHistoricalQuantityCorrection {
		if len(binding) != sha256.Size || run.Basis == nil || run.Basis.JournalPosition < 0 {
			return DatedResult{State: "unknown"}, nil
		}
		run.compared = &materialized
		digest, err := signedStatementDigest(ctx, tx, tenant, run)
		if errors.Is(err, errBatchReceipt) || (err == nil && !bytes.Equal(digest, binding)) {
			return DatedResult{State: "unknown"}, nil
		}
		if err != nil {
			return DatedResult{}, err
		}
		prefixPresent, changed, err := journalPrefixAtClose(ctx, tx, subject, run.Basis.JournalPosition)
		if err != nil {
			return DatedResult{}, err
		}
		if !prefixPresent {
			return DatedResult{State: "unknown"}, nil
		}
		if changed {
			return DatedResult{State: "pending", StatementID: id, Reason: "journal_changed"}, nil
		}
		if proof := run.Basis.Transactions; proof != nil && !proof.matches(materialized, run.Outcome, run.Basis.JournalPosition) {
			return DatedResult{State: "unknown"}, nil
		}
		if run.Basis.Assignments == nil {
			var enrolled bool
			if err := tx.QueryRow(ctx, "SELECT custody_assignment_reader_enrolled()").Scan(&enrolled); err != nil {
				return DatedResult{}, err
			}
			if enrolled {
				return DatedResult{State: "pending", StatementID: id, Reason: "assignment_unproven"}, nil
			}
		}
		verifiedAssignment, err = confirmRunAssignments(ctx, tx, subject, run.Basis, false)
		if errors.Is(err, ErrUnprovenAssignment) {
			return DatedResult{State: "pending", Reason: "assignment_changed", StatementID: id}, nil
		}
		if err != nil {
			return DatedResult{}, err
		}
		run.compared = nil
	}
	if run.Outcome == OutcomeHistoricalQuantityCorrection {
		pending := DatedResult{State: "pending", StatementID: id}
		accounts := run.Basis.Accounts[subject.CustodianID]
		if verifiedAssignment && len(accounts) == 1 {
			saved, err := historicalReceiptSnapshot(ctx, tx, tenant, subject, accounts[0], run.RunID)
			if err != nil {
				return DatedResult{}, err
			}
			if saved.State == "recorded" {
				pending.HistoricalQuantityCorrectionRunID = run.RunID
				pending.Run.Basis = run.Basis
			}
		}
		return pending, nil
	}
	if run.Outcome.IsHistoricalComparison() {
		// Even the latest dated view uses the retained evidence for a historical
		// comparison. Its differences have no corresponding live queue rows.
		accounts := run.Basis.Accounts[subject.CustodianID]
		if !verifiedAssignment || len(accounts) != 1 {
			return DatedResult{State: "unknown"}, nil
		}
		return historicalReceiptSnapshot(ctx, tx, tenant, subject, accounts[0], run.RunID)
	}
	// The current balance result and retained execution evidence must describe
	// the same signed run, account and database snapshot.
	if verifiedAssignment && len(binding) == sha256.Size && run.Basis != nil &&
		run.Basis.Transactions != nil && run.Basis.Transactions.Pass == recon.LegRan.String() {
		if accounts := run.Basis.Accounts[subject.CustodianID]; len(accounts) == 1 {
			saved, err := historicalReceiptSnapshot(ctx, tx, tenant, subject, accounts[0], run.RunID)
			if err != nil {
				return DatedResult{}, err
			}
			if saved.State == "recorded" && saved.AccountAssignmentVerified {
				run.ExecutionEconomics = saved.Run.ExecutionEconomics
			}
		}
	}
	return DatedResult{State: "recorded", StatementID: id, Run: run, AccountAssignmentVerified: verifiedAssignment}, nil
}

// Revision selection precedes the effective cutoff in the ledger fold. A
// later-dated corporate-action revision can displace an earlier leg.
func journalPrefixAtClose(ctx context.Context, tx pgx.Tx, subject Subject, position int64) (prefixPresent, changed bool, err error) {
	err = tx.QueryRow(ctx, `SELECT COALESCE(
                    (SELECT position FROM ledger_heads WHERE tenant_id=app_current_tenant() AND portfolio_id=$1),
                    (SELECT count(*) FROM ledger_entries WHERE tenant_id=app_current_tenant() AND portfolio_id=$1)) >= $2,
            EXISTS(
            SELECT 1 FROM ledger_append_positions p JOIN ledger_entries e USING (tenant_id,portfolio_id,entry_id)
            WHERE p.tenant_id=app_current_tenant() AND p.portfolio_id=$1 AND p.position>$2 AND
                (e.effective_time<$3 OR (e.action->>'action_id' <> '' AND EXISTS(
                    SELECT 1 FROM ledger_entries earlier WHERE earlier.tenant_id=p.tenant_id AND earlier.portfolio_id=p.portfolio_id
                      AND earlier.effective_time<$3 AND earlier.action->>'action_id'=e.action->>'action_id'))))`,
		subject.PortfolioID, position, BusinessDay(subject.BusinessDate).AddDate(0, 0, 1)).Scan(&prefixPresent, &changed)
	return
}
