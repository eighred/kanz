package custody

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// ValidReceiptID bounds a saved opaque ID before it reaches PostgreSQL.
func ValidReceiptID(id string) bool {
	return id != "" && len(id) <= 512 && utf8.ValidString(id) && strings.TrimSpace(id) == id && strings.IndexFunc(id, unicode.IsControl) < 0
}

// HistoricalReceipt reads the figures committed at one comparison. Its account
// comes from the server's present entitlement, independently of the saved ID.
func (p *Postgres) HistoricalReceipt(ctx context.Context, subject Subject, account, id string) (DatedResult, error) {
	unknown := DatedResult{State: "unknown"}
	tenant := bus.TenantIDFromContext(ctx)
	if tenant == "" || !ValidReceiptID(id) || account == "" || subject.Validate() != nil {
		return unknown, nil
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DatedResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	saved, err := historicalReceiptSnapshot(ctx, tx, tenant, subject, account, id)
	if err != nil || saved.State == "recorded" {
		return saved, err
	}
	return historicalAbsenceSnapshot(ctx, tx, tenant, subject, id)
}

// Absence belongs to the scheduled portfolio/custodian close. The retained
// diagnostic attests what the scheduler observed, independently of the
// current statement. It contains no signed source or account-level figures.
func historicalAbsenceSnapshot(ctx context.Context, tx pgx.Tx, tenant string, subject Subject, id string) (DatedResult, error) {
	unknown := DatedResult{State: "unknown"}
	var payload []byte
	var completed time.Time
	day := BusinessDay(subject.BusinessDate)
	err := tx.QueryRow(ctx, `SELECT manual_receipt, completed_at FROM custody_runs
		WHERE tenant_id=app_current_tenant() AND tenant_id=$1 AND portfolio_id=$2 AND custodian_id=$3
		AND business_date=$4 AND run_id=$5 AND values_verified AND outcome='no_statement'
		AND failure_reason='' AND statement_id='' AND signed_statement_sha256 IS NULL AND book_basis IS NULL
		AND octet_length(manual_receipt) BETWEEN 1 AND $6`,
		tenant, subject.PortfolioID, subject.CustodianID, day, id, BatchLimit).Scan(&payload, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return unknown, nil
	}
	if err != nil {
		return DatedResult{}, err
	}
	run, err := runFromRecord(payload)
	if err != nil || run.RunID != id || run.RunID != runID(subject, run.CompletedAt) ||
		run.Subject.PortfolioID != subject.PortfolioID || run.Subject.CustodianID != subject.CustodianID || !run.Subject.BusinessDate.Equal(day) ||
		!run.CompletedAt.Truncate(time.Microsecond).Equal(completed) || run.Outcome != OutcomeNoStatement ||
		run.RequestID != "" || run.Actor != "" || run.StatementID != "" || run.FailureReason != "" || len(run.Breaks) != 0 ||
		run.ExecutionEconomics != nil || run.ExecutionQuantities != nil || len(run.HistoricalQuantityResolutions) != 0 {
		return unknown, nil
	}
	// Use PostgreSQL's timestamp precision, as in DatedResult's live lookup.
	run.CompletedAt = completed
	return DatedResult{State: "recorded", Run: run}, nil
}

func historicalReceiptSnapshot(ctx context.Context, tx pgx.Tx, tenant string, subject Subject, account, id string) (DatedResult, error) {
	unknown := DatedResult{State: "unknown"}
	var payload, binding []byte
	var statementID, outcome, tolerance string
	var completed time.Time
	var basis *RunBasis
	// Limit the bytea on the server before transmission and use the same answer
	// for an absent run, a different tenant/pair/date/account and a legacy row.
	err := tx.QueryRow(ctx, `SELECT manual_receipt,signed_statement_sha256,statement_id,outcome,tolerance,completed_at,book_basis
		FROM custody_runs WHERE tenant_id=app_current_tenant() AND tenant_id=$1 AND portfolio_id=$2 AND custodian_id=$3
		AND business_date=$4 AND run_id=$5 AND values_verified AND outcome IN ('clean','breaks','historical_quantity_correction') AND failure_reason=''
		AND book_basis->>'mode'=$6 AND book_basis->'accounts'->$3::text=to_jsonb(ARRAY[$7::text])
		AND octet_length(signed_statement_sha256)=$8 AND octet_length(manual_receipt) BETWEEN 1 AND $9`,
		tenant, subject.PortfolioID, subject.CustodianID, BusinessDay(subject.BusinessDate), id, BasisDeclared, account, sha256.Size, BatchLimit).
		Scan(&payload, &binding, &statementID, &outcome, &tolerance, &completed, &basis)
	if errors.Is(err, pgx.ErrNoRows) {
		return unknown, nil
	}
	if err != nil {
		return DatedResult{}, err
	}
	run, err := runFromRecord(payload)
	if err != nil || basis == nil || basis.JournalPosition < 0 || run.Subject.PortfolioID != subject.PortfolioID ||
		run.Subject.CustodianID != subject.CustodianID || !run.Subject.BusinessDate.Equal(subject.BusinessDate) ||
		run.RunID != id || run.Outcome.String() != outcome || run.StatementID != statementID ||
		run.FailureReason != "" || !run.CompletedAt.Truncate(time.Microsecond).Equal(completed) ||
		!isBatchStatement(statementID) || len(run.Breaks) > historicalBreakLimit || ((run.Outcome == OutcomeClean || run.Outcome == OutcomeHistoricalQuantityCorrection) != (len(run.Breaks) == 0)) {
		return unknown, nil
	}
	storedTolerance, err := parseRatText(tolerance)
	if err != nil || !equalFigure(storedTolerance, run.Tolerance) || run.Tolerance.Sign() < 0 {
		return unknown, nil
	}
	if run.RequestID == "" {
		if run.Actor != "" || run.RunID != runID(subject, run.CompletedAt) {
			return unknown, nil
		}
	} else if run.Actor == "" || run.RunID != "manual:"+run.RequestID {
		return unknown, nil
	}
	source, digest, err := retainedSignedStatement(ctx, tx, tenant, subject, statementID, []string{account})
	if errors.Is(err, errBatchReceipt) {
		return unknown, nil
	}
	if err != nil {
		return DatedResult{}, err
	}
	if !bytes.Equal(digest, binding) || (basis.Transactions != nil && !basis.Transactions.matches(source, run.Outcome, basis.JournalPosition)) || !receiptFiguresMatch(run, source) || !quantityEvidenceMatches(run, source, basis.Transactions) {
		return unknown, nil
	}
	verifiedAssignment, err := confirmRunAssignments(ctx, tx, subject, basis, true)
	if errors.Is(err, ErrUnprovenAssignment) {
		return unknown, nil
	}
	if err != nil {
		return DatedResult{}, err
	}
	run.Basis = basis
	// Earlier readers could not ingest a signed source declaring this contract.
	// A verified ordinary receipt against such a source must retain the
	// measurements it committed, including an explicitly unassessed row.
	if verifiedAssignment && verifying(run) && run.ExecutionEconomics == nil {
		for _, line := range source.Transactions {
			if line.EconomicsConvention != "" || line.Commission != nil {
				return unknown, nil
			}
		}
	}
	if run.ExecutionEconomics != nil {
		if !verifiedAssignment {
			return unknown, nil
		}
		expected, err := compareExecutionEconomics(ctx, tx, run, source)
		if err != nil {
			return DatedResult{}, err
		}
		if !proto.Equal(expected, run.ExecutionEconomics) {
			return unknown, nil
		}
	}
	if run.Outcome == OutcomeHistoricalQuantityCorrection {
		if !verifiedAssignment || run.ExecutionQuantities != nil || len(run.HistoricalQuantityResolutions) == 0 || len(run.HistoricalQuantityResolutions) > historicalBreakLimit {
			return unknown, nil
		}
		valid, err := historicalResolutionsMatch(ctx, tx, run, source, account)
		if err != nil {
			return DatedResult{}, err
		}
		if !valid {
			return unknown, nil
		}
	} else if len(run.HistoricalQuantityResolutions) != 0 {
		return unknown, nil
	}
	result := DatedResult{State: "recorded", StatementID: statementID, Run: run, SignedStatementSHA256: hex.EncodeToString(digest), AccountAssignmentVerified: verifiedAssignment}
	if verifiedAssignment && run.Outcome != OutcomeHistoricalQuantityCorrection && source.Grain == recon.GrainTransactions && basis.Transactions != nil && basis.Transactions.Pass == recon.LegRan.String() {
		// This optional read projection comes from the retained independent signature.
		// The original FACT and its comparison scope remain exactly as committed.
		result.reportedTransactions = source.Transactions
		if result.reportedTransactions == nil {
			result.reportedTransactions = []recon.Transaction{}
		}
	}
	return result, nil
}

func receiptFiguresMatch(run Run, source Statement) bool {
	references := make(map[string]*big.Rat, len(source.Transactions))
	for _, row := range source.Transactions {
		references[row.ExternalRef] = row.Quantity
	}
	seen := make(map[string]bool, len(run.Breaks))
	for _, b := range run.Breaks {
		if b.Key == "" || b.BreakID != BreakID(run.Subject.PortfolioID, run.Subject.CustodianID, b.Kind, b.Key) || seen[b.BreakID] || b.Status == BreakResolved {
			return false
		}
		seen[b.BreakID] = true
		var signed *big.Rat
		switch b.Kind {
		case recon.BreakMissingAtCustodian, recon.BreakMissingInIBOR, recon.BreakQuantity:
			signed = orZeroRat(source.Positions[b.Key])
		case recon.BreakCash:
			signed = orZeroRat(source.Cash[b.Key])
		case recon.BreakExecutionMissingInIBOR:
			q, present := references[b.Key]
			if !present || b.IBOR == nil || b.IBOR.Sign() != 0 {
				return false
			}
			signed = q
		case recon.BreakExecutionMissingAtCustodian:
			if _, present := references[b.Key]; present {
				return false
			}
			signed = new(big.Rat)
		default:
			return false
		}
		if !equalFigure(signed, b.Custodian) {
			return false
		}
		if b.IBOR == nil || b.Custodian == nil {
			if b.Diff != nil || (b.Kind != recon.BreakExecutionMissingAtCustodian && b.Kind != recon.BreakExecutionMissingInIBOR) {
				return false
			}
		} else if !equalFigure(b.Diff, new(big.Rat).Sub(b.IBOR, b.Custodian)) {
			return false
		}
	}
	return true
}

func historicalResolutionsMatch(ctx context.Context, tx pgx.Tx, run Run, source Statement, account string) (bool, error) {
	if len(run.Basis.HistoricalDetections) != len(run.HistoricalQuantityResolutions) {
		return false, nil
	}
	signed := make(map[string]recon.Transaction, len(source.Transactions))
	for _, line := range source.Transactions {
		signed[line.ExternalRef] = line
	}
	rows, err := tx.Query(ctx, `SELECT curr.break_id,prior.run_id,curr.entry_id,curr.external_ref,curr.instrument_id,curr.ibor
    FROM custody_execution_quantity_observations curr
    JOIN custody_runs r USING(tenant_id,run_id,portfolio_id,custodian_id,business_date,statement_id)
    JOIN ledger_entries e USING(tenant_id,portfolio_id,entry_id)
    JOIN ledger_append_positions ep USING(tenant_id,portfolio_id,entry_id)
    JOIN LATERAL (
        SELECT p.* FROM custody_execution_quantity_observations p
        WHERE (p.tenant_id,p.portfolio_id,p.custodian_id,p.business_date,p.break_id)=
              (curr.tenant_id,curr.portfolio_id,curr.custodian_id,curr.business_date,curr.break_id) AND p.observation<curr.observation
        ORDER BY p.source_revision DESC,p.observation DESC LIMIT 1
    ) prior ON true
    JOIN custody_runs old ON old.tenant_id=prior.tenant_id AND old.run_id=prior.run_id AND old.statement_id=prior.statement_id
    JOIN custody_batch_receipts source ON source.tenant_id=curr.tenant_id AND source.statement_id=curr.statement_id
    JOIN custody_batch_receipts original ON original.tenant_id=prior.tenant_id AND original.statement_id=prior.statement_id
    CROSS JOIN LATERAL jsonb_array_elements(r.book_basis->'assignments') bound
    WHERE curr.tenant_id=app_current_tenant() AND curr.run_id=$1 AND curr.portfolio_id=$2 AND curr.custodian_id=$3 AND curr.business_date=$4
      AND curr.statement_id=$5 AND curr.account_id=$6 AND r.outcome='historical_quantity_correction'
      AND old.outcome IN ('clean','breaks') AND old.signed_statement_sha256=decode(original.sha256,'hex')
      AND (old.portfolio_id,old.custodian_id,old.business_date)=(curr.portfolio_id,curr.custodian_id,curr.business_date)
      AND (source.source_id,source.public_key,source.account_id)=(original.source_id,original.public_key,original.account_id)
      AND original.account_id=curr.account_id AND original.revision=prior.source_revision AND source.revision=curr.source_revision
      AND bound->>'account_id'=curr.account_id AND bound->>'custodian_id'=curr.custodian_id
      AND old.book_basis->'assignments' @> jsonb_build_array(bound)
      AND curr.break_id=public.custody_quantity_key(ARRAY[curr.tenant_id,curr.portfolio_id,curr.custodian_id,to_char(curr.business_date,'YYYY-MM-DD'),
          curr.account_id,bound->>'proof_sha256',bound->>'assignment_sha256',source.source_id,encode(source.public_key,'hex'),curr.external_ref,curr.instrument_id])
      AND prior.difference<>'0' AND prior.source_revision<curr.source_revision
      AND (prior.entry_id,prior.account_id,prior.instrument_id,prior.external_ref,prior.ibor,prior.first_seen_at)=
          (curr.entry_id,curr.account_id,curr.instrument_id,curr.external_ref,curr.ibor,curr.first_seen_at)
      AND e.venue_account_id=curr.account_id AND e.instrument_id=curr.instrument_id AND e.source_ref=curr.external_ref AND e.entry_type=1
      AND ep.position<=(r.book_basis->>'journal_position')::bigint AND ep.position<=(old.book_basis->>'journal_position')::bigint
      AND curr.difference='0' AND curr.custodian=curr.ibor
      AND split_part(e.quantity,'/',1)::numeric=curr.ibor::numeric*coalesce(nullif(split_part(e.quantity,'/',2),''),'1')::numeric
    ORDER BY curr.break_id COLLATE "C"`, run.RunID, run.Subject.PortfolioID, run.Subject.CustodianID, run.Subject.BusinessDate, run.StatementID, account)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		if i >= len(run.HistoricalQuantityResolutions) {
			return false, nil
		}
		link := run.HistoricalQuantityResolutions[i]
		i++
		var id, prior, entry, ref, instrument, book string
		if err := rows.Scan(&id, &prior, &entry, &ref, &instrument, &book); err != nil {
			return false, err
		}
		if link == nil || id != link.BreakId || prior != link.DetectionRunId || entry != link.EntryId || ref != link.ExternalRef || instrument != link.InstrumentId || run.Basis.MeasuredEntries[entry] != ref || run.Basis.HistoricalDetections[id] != prior {
			return false, nil
		}
		agreed, ok := dec.FromProtoChecked(link.AgreedQuantity)
		amount, err := parseStored(book)
		line := signed[ref]
		if !ok || err != nil || !equalFigure(agreed, amount) || !line.QuantityInInstrumentUnits || line.InstrumentID != instrument || !equalFigure(agreed, line.Quantity) {
			return false, nil
		}
	}
	return i == len(run.HistoricalQuantityResolutions), rows.Err()
}
