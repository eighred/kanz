package custody

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/jackc/pgx/v5"
)

// Working items are scoped by the actual signed source, its dated independent
// assignment, and the account on the booked trade. Older reconcilers can still
// close their own balance/membership items without touching these observations.
func quantityObservations(ctx context.Context, tx pgx.Tx, run Run, digest []byte, planHistorical bool) ([]*accountingpb.HistoricalQuantityResolution, error) {
	s := run.Subject
	if len(run.measured) == 0 || (!verifying(run) && run.Outcome != OutcomeHistoricalQuantityCorrection) || run.Basis == nil || run.Basis.Mode != BasisDeclared || len(digest) != sha256.Size {
		return nil, nil
	}
	accounts := run.Basis.Accounts[s.CustodianID]
	bindings := assignmentsForCustodian(run.Basis.Assignments, s.CustodianID)
	if len(accounts) != 1 || len(bindings) != 1 || accounts[0] != bindings[0].AccountID {
		return nil, nil
	}
	a := bindings[0]
	var source, account, key string
	var revision int64
	day := BusinessDay(s.BusinessDate)
	err := tx.QueryRow(ctx, `SELECT account_id,source_id,encode(public_key,'hex'),revision FROM custody_batch_receipts
 WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3 AND statement_id=$4 AND sha256=$5`, s.PortfolioID, s.CustodianID, day, run.StatementID, hex.EncodeToString(digest)).Scan(&account, &source, &key, &revision)
	if err != nil {
		return nil, fmt.Errorf("custody: quantity source: %w", err)
	}
	if account != a.AccountID {
		return nil, nil
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
			return nil, ErrUnverifiedPrecision
		}
		entries = append(entries, m.EntryID)
		instruments = append(instruments, m.InstrumentID)
		refs = append(refs, m.ExternalRef)
		books = append(books, book)
		statements = append(statements, statement)
		differences = append(differences, diff)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	// Absence is not equality. Only a later signed revision may close this
	// scope's working item. The pair lock acquired in CommitRun orders writers.
	const candidates = `WITH measured AS (
        SELECT *, custody_quantity_key(ARRAY[app_current_tenant(),$1,$2,to_char($3::date,'YYYY-MM-DD'),$4,$5,$6,$7,$8,ref,instrument]) id
        FROM unnest($9::text[],$10::text[],$11::text[],$12::text[],$13::text[],$14::text[])
        AS m(entry,instrument,ref,book,statement,diff)
    ), candidates AS (
        SELECT m.*,p.source_revision prior_revision,p.difference prior_difference,p.run_id prior_run,
               p.entry_id prior_entry,p.ibor prior_book,p.first_seen_at,
               $15::text new_run,$16::text new_statement,$17::bigint revision,$18::timestamptz measured_at
        FROM measured m LEFT JOIN LATERAL (
            SELECT source_revision,difference,first_seen_at,run_id,entry_id,ibor FROM custody_execution_quantity_observations
            WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3 AND break_id=m.id
            ORDER BY source_revision DESC,observation DESC LIMIT 1
        ) p ON true
    ) `
	const historicalEquality = `diff='0' AND prior_difference<>'0' AND revision>prior_revision AND entry=prior_entry AND book=prior_book`
	args := []any{s.PortfolioID, s.CustodianID, day, account, a.ProofDigest, a.AssignmentDigest, source, key,
		entries, instruments, refs, books, statements, differences, run.RunID, run.StatementID, revision, run.CompletedAt}
	if planHistorical {
		rows, err := tx.Query(ctx, candidates+`SELECT id,prior_run,entry,ref,instrument,book FROM candidates WHERE `+historicalEquality+` ORDER BY id COLLATE "C"`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var links []*accountingpb.HistoricalQuantityResolution
		for rows.Next() {
			var link accountingpb.HistoricalQuantityResolution
			var book string
			if err := rows.Scan(&link.BreakId, &link.DetectionRunId, &link.EntryId, &link.ExternalRef, &link.InstrumentId, &book); err != nil {
				return nil, err
			}
			amount, err := parseStored(book)
			if err != nil {
				return nil, err
			}
			var ok bool
			if link.AgreedQuantity, ok = exactProto(amount); !ok {
				return nil, ErrUnverifiedPrecision
			}
			links = append(links, &link)
		}
		return links, rows.Err()
	}
	filter := `(diff<>'0' AND (prior_revision IS NULL OR revision>=prior_revision)) OR (prior_difference<>'0' AND revision>prior_revision)`
	if run.Outcome == OutcomeHistoricalQuantityCorrection {
		filter = historicalEquality
	}
	n, err := tx.Exec(ctx, candidates+`INSERT INTO custody_execution_quantity_observations
        (portfolio_id,custodian_id,business_date,break_id,run_id,statement_id,entry_id,account_id,
         instrument_id,external_ref,source_revision,ibor,custodian,difference,first_seen_at,measured_at)
    SELECT $1,$2,$3,id,new_run,new_statement,entry,$4,instrument,ref,revision,book,statement,diff,
           CASE WHEN prior_difference<>'0' THEN first_seen_at ELSE measured_at END,measured_at FROM candidates WHERE `+filter, args...)
	if err != nil {
		return nil, fmt.Errorf("custody: record measured executions: %w", err)
	}
	if run.Outcome == OutcomeHistoricalQuantityCorrection && n.RowsAffected() != int64(len(run.HistoricalQuantityResolutions)) {
		return nil, ErrStatementChanged
	}
	return nil, nil
}

// MaxQueuePage is the largest complete customer queue or explicit page.
const MaxQueuePage = 4096

func validQueueFetch(limit int) error {
	// The extra row tells the HTTP reader whether a continuation exists.
	if limit < 1 || limit > MaxQueuePage+1 {
		return errors.New("custody: invalid break page size")
	}
	return nil
}

func sortQueuePage(items []Break, limit int) []Break {
	slices.SortFunc(items, func(a, b Break) int { return strings.Compare(a.BreakID, b.BreakID) })
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

// The aliases are SQL literals from the two internal queries. The scope and
// continuation stay bound parameters. C collation is the same byte order as Go.
func queuePredicates(alias string, portfolios []string, after string, limit int) (string, []any) {
	var q string
	var args []any
	if len(portfolios) > 0 {
		q += " AND " + alias + "portfolio_id=ANY($1)"
		args = append(args, portfolios)
	}
	if limit > 0 {
		args = append(args, after)
		q += " AND " + alias + "portfolio_id<>'' AND " + alias + "break_id COLLATE \"C\" > $" + strconv.Itoa(len(args))
	}
	return q, args
}

func queueOrder(alias string, limit int) string {
	q := " ORDER BY " + alias + "break_id COLLATE \"C\""
	if limit > 0 {
		q += " LIMIT " + strconv.Itoa(limit)
	}
	return q
}

func (p *Postgres) BreakPage(ctx context.Context, portfolios []string, after string, limit int) ([]Break, error) {
	if err := validQueueFetch(limit); err != nil {
		return nil, err
	}
	return p.outstandingBreaks(ctx, portfolios, after, limit)
}

func (p *Postgres) OutstandingBreaks(ctx context.Context, portfolios ...string) ([]Break, error) {
	return p.outstandingBreaks(ctx, portfolios, "", 0)
}

func (p *Postgres) outstandingBreaks(ctx context.Context, portfolios []string, after string, limit int) ([]Break, error) {
	if p.tx != nil {
		return p.outstandingInSnapshot(ctx, portfolios, after, limit)
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := (&Postgres{pool: p.pool, tx: tx}).outstandingInSnapshot(ctx, portfolios, after, limit)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

func (p *Postgres) outstandingInSnapshot(ctx context.Context, portfolios []string, after string, limit int) ([]Break, error) {
	out, err := p.balanceAndMembershipBreaks(ctx, portfolios, after, limit)
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
	scope, args := queuePredicates("", portfolios, after, limit)
	q += scope
	q += `
 ORDER BY portfolio_id,custodian_id,business_date,break_id,source_revision DESC,observation DESC) q
 JOIN custody_runs r ON r.tenant_id=q.tenant_id AND r.run_id=q.run_id WHERE q.difference<>'0'`
	q += queueOrder("q.", limit)
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
	return sortQueuePage(out, limit), rows.Err()
}
