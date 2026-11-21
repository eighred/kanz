package ledger

import (
	"context"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// ExecutionEconomics is the booked position and signed cash of one settled
// execution. GrossCash excludes commission. NetCash and SettledCash come from
// the traded and settled ledger folds, including approved fee adjustments.
type ExecutionEconomics struct {
	Quantity, Price, GrossCash, NetCash, Commission, SettledCash *big.Rat
	CashCurrency, CommissionCurrency, Venue, VenueExecutionID    string
	TradeDate, SettlementDate                                    time.Time
	SettlementBasis                                              SettlementBasis
}

// ReadCustodyExecutionEconomics uses the receipt reader's RLS transaction and
// immutable commit prefix. Missing or ambiguous book evidence returns nil.
func ReadCustodyExecutionEconomics(ctx context.Context, tx pgx.Tx, portfolio, account, entryID, externalRef, instrument string, businessDate time.Time, journalPosition int64) (*ExecutionEconomics, error) {
	if portfolio == "" || account == "" || entryID == "" || externalRef == "" || instrument == "" || businessDate.IsZero() || journalPosition <= 0 {
		return nil, nil
	}
	day := CustodyBusinessDate(businessDate, time.Time{})
	// Include competing executions under the raw alias, as well as corrections
	// linked to the measured entry. Legacy rows without a position cannot prove
	// their membership in the original book. Check them before returning evidence.
	rows, err := tx.Query(ctx, `SELECT p.position,e.entry_id,e.entry_type,e.venue_account_id,e.instrument_id,e.source_ref,
			e.quantity,e.price,e.cash,e.cash_currency,e.effective_time,e.knowledge_time,e.settlement_status,e.settlement_date,
			CASE WHEN octet_length(e.execution_evidence)<=1048576 AND e.action IS NULL THEN e.execution_evidence END
		FROM ledger_entries e
		JOIN ledger_heads h USING (tenant_id,portfolio_id)
		LEFT JOIN ledger_append_positions p USING (tenant_id,portfolio_id,entry_id)
		WHERE e.tenant_id=app_current_tenant() AND e.portfolio_id=$1 AND h.position>=$2
		  AND (p.position IS NULL OR p.position<=$2)
		  AND (e.entry_id=$3 OR (e.entry_type=3 AND e.source_ref=$3)
		       OR (e.entry_type=1 AND e.source_ref=$4 AND e.source_ref<>'' AND e.venue_account_id=$5
		           AND COALESCE(e.settlement_date,e.effective_time)>=$6 AND COALESCE(e.settlement_date,e.effective_time)<$7))
		ORDER BY p.position NULLS FIRST`, portfolio, journalPosition, entryID, externalRef, account, day, day.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var original *ExecutionEconomics
	var head *orderpb.Fill
	book := NewBook(portfolio)
	for rows.Next() {
		var pos *int64
		var qty, price, cash *string
		var settlement *time.Time
		e := Event{PortfolioID: portfolio}
		if err := rows.Scan(&pos, &e.EntryID, &e.Type, &e.VenueAccountID, &e.InstrumentID, &e.SourceRef, &qty, &price, &cash, &e.CashCurrency, &e.Effective, &e.Knowledge, &e.SettlementBasis, &settlement, &e.ExecutionEvidence); err != nil {
			return nil, err
		}
		if pos == nil || e.VenueAccountID != account || e.InstrumentID != instrument || e.SettlementBasis != SettlementSettled || settlement == nil || e.CashCurrency == "" || !CustodyBusinessDate(e.Effective, time.Time{}).Equal(day) || !CustodyBusinessDate(e.Effective, *settlement).Equal(day) {
			return nil, nil
		}
		var fill orderpb.Fill
		if proto.Unmarshal(e.ExecutionEvidence, &fill) != nil || fill.ExecutedAt == nil || fill.ExecutedAt.CheckValid() != nil {
			return nil, nil
		}
		// TIMESTAMPTZ is microsecond precision; restore the match instant only
		// when both booked timestamps agree with the retained spot Fill.
		instant := fill.ExecutedAt.AsTime()
		if !e.Effective.Equal(instant.Truncate(time.Microsecond)) || !settlement.Equal(instant.Truncate(time.Microsecond)) {
			return nil, nil
		}
		e.Effective, e.SettlementDate = instant, instant
		for _, amount := range []struct {
			raw  *string
			dest **big.Rat
		}{{qty, &e.Quantity}, {price, &e.Price}, {cash, &e.Cash}} {
			if amount.raw != nil {
				v, err := dec.Exact(*amount.raw).Rat()
				if err != nil {
					return nil, nil
				}
				*amount.dest = v
			}
		}
		verified := coveredExecution(&e)
		if verified == nil || verified.GetFee().GetCurrencyCode() != e.CashCurrency {
			return nil, nil
		}
		if _, err := fillfact.EconomicDigest(verified); err != nil || dec.FromProto(verified.Fee.Amount).Sign() < 0 {
			return nil, nil
		}
		switch e.Type {
		case EntryTrade:
			if original != nil || e.EntryID != entryID || e.SourceRef != externalRef {
				return nil, nil
			}
			if verified.GetRecovery().GetFeeApproval() != nil {
				if _, err := fillfact.FeeRevisionTerms(verified); err != nil {
					return nil, nil
				}
			}
			original = &ExecutionEconomics{
				Quantity: e.Quantity, Price: e.Price, CashCurrency: e.CashCurrency,
				CommissionCurrency: verified.Fee.CurrencyCode, Venue: verified.Venue, VenueExecutionID: verified.VenueExecutionId,
				TradeDate: e.Effective, SettlementDate: e.SettlementDate, SettlementBasis: e.SettlementBasis,
			}
		case EntryFee:
			if head == nil || e.SourceRef != entryID {
				return nil, nil
			}
			if _, err := fillfact.ApprovedFeeDelta(head, verified); err != nil {
				return nil, nil
			}
		default:
			return nil, nil
		}
		book.Apply(&e)
		head = verified
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if original == nil {
		return nil, nil
	}
	original.Commission = dec.FromProto(head.Fee.Amount)
	original.GrossCash = new(big.Rat).Neg(new(big.Rat).Mul(original.Quantity, original.Price))
	original.NetCash = book.CashBalance(original.CashCurrency)
	original.SettledCash = new(big.Rat).Set(orZero(book.SettledCash[original.CashCurrency]))
	if original.GrossCash.Cmp(new(big.Rat).Add(original.NetCash, original.Commission)) != 0 || original.SettledCash.Cmp(original.NetCash) != 0 {
		return nil, nil
	}
	return original, nil
}
