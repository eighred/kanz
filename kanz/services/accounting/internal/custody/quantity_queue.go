package custody

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

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
	var writes pgx.Batch
	for _, m := range run.measured {
		if m.AccountID != account || m.EntryID == "" {
			continue
		}
		framed, err := json.Marshal([]string{a.TenantID, s.PortfolioID, s.CustodianID, day.Format(time.DateOnly), account, a.ProofDigest, a.AssignmentDigest, source, key, m.ExternalRef, m.InstrumentID})
		if err != nil {
			return err
		}
		id := fmt.Sprintf("q1:%x", sha256.Sum256(framed))
		ibor, ok1 := ExactDecimalText(m.Book)
		custodian, ok2 := ExactDecimalText(m.Statement)
		diff, ok3 := ExactDecimalText(m.Difference)
		if !ok1 || !ok2 || !ok3 {
			return ErrUnverifiedPrecision
		}
		// A missing, unassessed or differently attributed reference provides no equality.
		// Only a later signed source revision may clear an outstanding item.
		writes.Queue(`WITH previous AS (
    SELECT source_revision,difference,first_seen_at FROM custody_execution_quantity_observations
    WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3 AND break_id=$4
    ORDER BY observation DESC LIMIT 1)
  INSERT INTO custody_execution_quantity_observations
    (portfolio_id,custodian_id,business_date,break_id,run_id,statement_id,entry_id,account_id,instrument_id,external_ref,source_revision,ibor,custodian,difference,first_seen_at,measured_at)
  SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,
    CASE WHEN p.difference<>'0' THEN p.first_seen_at ELSE $15 END,$15
    FROM (VALUES (1)) seed(n) LEFT JOIN previous p ON true
    WHERE ($14::text<>'0' AND (p.source_revision IS NULL OR $11>=p.source_revision))
       OR (p.difference<>'0' AND $11>p.source_revision)`, s.PortfolioID, s.CustodianID, day, id, run.RunID, run.StatementID, m.EntryID, account, m.InstrumentID, m.ExternalRef, revision, ibor, custodian, diff, run.CompletedAt)
	}
	if writes.Len() == 0 {
		return nil
	}
	if err := tx.SendBatch(ctx, &writes).Close(); err != nil {
		return fmt.Errorf("custody: record measured execution: %w", err)
	}
	return nil
}

func (p *Postgres) OutstandingBreaks(ctx context.Context) ([]Break, error) {
	if p.tx != nil {
		return p.outstandingInSnapshot(ctx)
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	out, err := (&Postgres{pool: p.pool, tx: tx}).outstandingInSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

func (p *Postgres) outstandingInSnapshot(ctx context.Context) ([]Break, error) {
	out, err := p.balanceAndMembershipBreaks(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := p.tx.Query(ctx, `SELECT q.break_id,q.portfolio_id,q.custodian_id,q.instrument_id,q.external_ref,
 q.ibor,q.custodian,q.difference,q.first_seen_at,q.measured_at,q.run_id,q.statement_id,
 to_char(q.business_date,'YYYY-MM-DD'),r.book_basis->>'mode',r.book_basis->>'journal_position'
 FROM (SELECT DISTINCT ON (portfolio_id,custodian_id,business_date,break_id) *
 FROM custody_execution_quantity_observations WHERE tenant_id=app_current_tenant()
 ORDER BY portfolio_id,custodian_id,business_date,break_id,observation DESC) q
 JOIN custody_runs r ON r.tenant_id=q.tenant_id AND r.run_id=q.run_id WHERE q.difference<>'0'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		b := Break{Kind: recon.BreakExecutionQuantity, Status: BreakOpen, ValuesVerified: true}
		var ibor, custodian, diff string
		if err := rows.Scan(&b.BreakID, &b.PortfolioID, &b.CustodianID, &b.InstrumentID, &b.Key, &ibor, &custodian, &diff, &b.FirstSeenAt, &b.LastSeenAt, &b.Detection.RunID, &b.Detection.StatementID, &b.Detection.BusinessDate, &b.Detection.ScopeMode, &b.Detection.JournalPosition); err != nil {
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
