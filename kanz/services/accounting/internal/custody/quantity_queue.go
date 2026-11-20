package custody

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/jackc/pgx/v5"
)

// Working items are scoped by the actual signed source, its dated independent
// assignment, and the account on the booked trade. Older reconcilers can still
// close their own balance/membership items without touching these observations.
func recordQuantityObservations(ctx context.Context, tx pgx.Tx, run Run, digest []byte) error {
	s := run.Subject
	if !verifying(run) || run.ExecutionQuantities == nil || run.Basis == nil || run.Basis.Mode != BasisDeclared || len(digest) != sha256.Size {
		return nil
	}
	accounts := run.Basis.Accounts[s.CustodianID]
	bindings := assignmentsForCustodian(run.Basis.Assignments, s.CustodianID)
	if len(accounts) != 1 || len(bindings) != 1 || accounts[0] != bindings[0].AccountID {
		return nil
	}
	a := bindings[0]
	var source, account, key string
	var revision int64
	day := BusinessDay(s.BusinessDate)
	err := tx.QueryRow(ctx, `SELECT account_id,source_id,encode(public_key,'hex'),revision FROM custody_batch_receipts
 WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3 AND statement_id=$4 AND sha256=$5`, s.PortfolioID, s.CustodianID, day, run.StatementID, hex.EncodeToString(digest)).Scan(&account, &source, &key, &revision)
	if err != nil {
		return fmt.Errorf("custody: quantity source: %w", err)
	}
	if account != a.AccountID {
		return nil
	}
	var entries, instruments, refs, books, statements, differences []string
	for _, m := range run.measured {
		if m.AccountID != account || m.EntryID == "" {
			continue
		}
		book, bok := ExactDecimalText(m.Book)
		statement, sok := ExactDecimalText(m.Statement)
		diff, dok := ExactDecimalText(m.Difference)
		if !bok || !sok || !dok {
			return ErrUnverifiedPrecision
		}
		entries = append(entries, m.EntryID)
		instruments = append(instruments, m.InstrumentID)
		refs = append(refs, m.ExternalRef)
		books = append(books, book)
		statements = append(statements, statement)
		differences = append(differences, diff)
	}
	if len(entries) == 0 {
		return nil
	}
	// Absence is not equality. Only a later signed revision may close this
	// scope's working item. The pair lock acquired in CommitRun orders writers.
	_, err = tx.Exec(ctx, `WITH measured AS (
        SELECT *, custody_quantity_key(ARRAY[app_current_tenant(),$1,$2,to_char($3::date,'YYYY-MM-DD'),$4,$5,$6,$7,$8,ref,instrument]) id
        FROM unnest($9::text[],$10::text[],$11::text[],$12::text[],$13::text[],$14::text[])
        AS m(entry,instrument,ref,book,statement,diff)
    )
    INSERT INTO custody_execution_quantity_observations
        (portfolio_id,custodian_id,business_date,break_id,run_id,statement_id,entry_id,account_id,
         instrument_id,external_ref,source_revision,ibor,custodian,difference,first_seen_at,measured_at)
    SELECT $1,$2,$3,m.id,$15,$16,m.entry,$4,m.instrument,m.ref,$17,m.book,m.statement,m.diff,
        CASE WHEN p.difference<>'0' THEN p.first_seen_at ELSE $18 END,$18
    FROM measured m LEFT JOIN LATERAL (
        SELECT source_revision,difference,first_seen_at FROM custody_execution_quantity_observations
        WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3 AND break_id=m.id
        ORDER BY source_revision DESC,observation DESC LIMIT 1
    ) p ON true
    WHERE (m.diff<>'0' AND (p.source_revision IS NULL OR $17>=p.source_revision))
       OR (p.difference<>'0' AND $17>p.source_revision)`,
		s.PortfolioID, s.CustodianID, day, account, a.ProofDigest, a.AssignmentDigest, source, key,
		entries, instruments, refs, books, statements, differences, run.RunID, run.StatementID, revision, run.CompletedAt)
	if err != nil {
		return fmt.Errorf("custody: record measured executions: %w", err)
	}
	return nil
}

func (p *Postgres) OutstandingBreaks(ctx context.Context, portfolios ...string) ([]Break, error) {
	if p.tx != nil {
		return p.outstandingInSnapshot(ctx, portfolios...)
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	out, err := (&Postgres{pool: p.pool, tx: tx}).outstandingInSnapshot(ctx, portfolios...)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

func (p *Postgres) outstandingInSnapshot(ctx context.Context, portfolios ...string) ([]Break, error) {
	out, err := p.balanceAndMembershipBreaks(ctx, portfolios...)
	if err != nil {
		return nil, err
	}
	q := `SELECT q.break_id,q.portfolio_id,q.custodian_id,q.instrument_id,q.external_ref,
 q.ibor,q.custodian,q.difference,q.first_seen_at,q.measured_at,q.run_id,q.statement_id,
 to_char(q.business_date,'YYYY-MM-DD'),r.book_basis->>'mode',r.book_basis->>'journal_position',
 CASE WHEN EXISTS(SELECT FROM custody_runs newer WHERE newer.tenant_id=q.tenant_id AND newer.portfolio_id=q.portfolio_id
    AND newer.custodian_id=q.custodian_id AND newer.business_date>q.business_date AND newer.outcome IN ('clean','breaks'))
 THEN coalesce((SELECT candidate.statement_id FROM custody_batch_receipts original JOIN custody_batch_receipts candidate
    USING (tenant_id,portfolio_id,custodian_id,business_date,account_id,source_id,public_key)
    WHERE original.tenant_id=q.tenant_id AND original.statement_id=q.statement_id AND candidate.revision>q.source_revision
    ORDER BY candidate.revision DESC LIMIT 1),'') ELSE '' END
 FROM (SELECT DISTINCT ON (portfolio_id,custodian_id,business_date,break_id) *
 FROM custody_execution_quantity_observations WHERE tenant_id=app_current_tenant()`
	var args []any
	if len(portfolios) > 0 {
		q += " AND portfolio_id=ANY($1)"
		args = append(args, portfolios)
	}
	q += `
 ORDER BY portfolio_id,custodian_id,business_date,break_id,source_revision DESC,observation DESC) q
 JOIN custody_runs r ON r.tenant_id=q.tenant_id AND r.run_id=q.run_id WHERE q.difference<>'0'`
	rows, err := p.tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		b := Break{Kind: recon.BreakExecutionQuantity, Status: BreakOpen, ValuesVerified: true}
		var ibor, custodian, diff string
		if err := rows.Scan(&b.BreakID, &b.PortfolioID, &b.CustodianID, &b.InstrumentID, &b.Key, &ibor, &custodian, &diff, &b.FirstSeenAt, &b.LastSeenAt, &b.Detection.RunID, &b.Detection.StatementID, &b.Detection.BusinessDate, &b.Detection.ScopeMode, &b.Detection.JournalPosition, &b.LateCorrectionStatementID); err != nil {
			return nil, err
		}
		if b.IBOR, err = parseStored(ibor); err != nil {
			return nil, err
		}
		if b.Custodian, err = parseStored(custodian); err != nil {
			return nil, err
		}
		if b.Diff, err = parseStored(diff); err != nil {
			return nil, err
		}
		b.StatusChangedAt = b.FirstSeenAt
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b Break) int { return strings.Compare(a.BreakID, b.BreakID) })
	return out, rows.Err()
}
