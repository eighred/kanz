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

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/jackc/pgx/v5"
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
	var payload, binding []byte
	var statementID, outcome, tolerance string
	var completed time.Time
	var basis *RunBasis
	// Limit the bytea on the server before transmission and use the same answer
	// for an absent run, a different tenant/pair/date/account and a legacy row.
	err = tx.QueryRow(ctx, `SELECT manual_receipt,signed_statement_sha256,statement_id,outcome,tolerance,completed_at,book_basis
		FROM custody_runs WHERE tenant_id=app_current_tenant() AND tenant_id=$1 AND portfolio_id=$2 AND custodian_id=$3
		AND business_date=$4 AND run_id=$5 AND values_verified AND outcome IN ('clean','breaks') AND failure_reason=''
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
		!isBatchStatement(statementID) || len(run.Breaks) > 4096 || (run.Outcome == OutcomeClean) != (len(run.Breaks) == 0) {
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
	if !bytes.Equal(digest, binding) || (basis.Transactions != nil && !basis.Transactions.matches(source, run.Outcome, basis.JournalPosition)) || !receiptFiguresMatch(run, source) {
		return unknown, nil
	}
	run.Basis = basis
	return DatedResult{State: "recorded", StatementID: statementID, Run: run, SignedStatementSHA256: hex.EncodeToString(digest)}, nil
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
