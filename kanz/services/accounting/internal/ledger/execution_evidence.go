package ledger

import (
	"context"
	"errors"
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var ErrLegacyExecutionUnknown = errors.New("ledger: legacy execution attribution requires verified evidence")

func samePostingScope(original, incoming *orderpb.Fill, oldCurrency, currency string) bool {
	if oldCurrency != currency {
		return false
	}
	a, b := original.GetSpotVenueSymbol(), incoming.GetSpotVenueSymbol()
	if a == b {
		return true
	}
	// Ordinary redelivery can add spot metadata to an older cash-denominated
	// trade. An approval must retain the exact basis of the original.
	return (a == "" || b == "") && incoming.GetRecovery().GetFeeApproval() == nil &&
		(original.GetFee().GetCurrencyCode() == currency || original.GetFee().GetAmount().GetCoefficient() == 0)
}

func exactRat(a, b *big.Rat) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(b) == 0
}

func executionFromEntry(e *Event) (*orderpb.Fill, error) {
	if len(e.ExecutionEvidence) == 0 {
		return nil, nil
	}
	if len(e.ExecutionEvidence) > 1<<20 {
		return nil, fillfact.ErrExecutionIdentityConflict
	}
	var fill orderpb.Fill
	if err := proto.Unmarshal(e.ExecutionEvidence, &fill); err != nil {
		return nil, err
	}
	if _, ok := dec.InDomainDeep(&fill); !ok {
		return nil, fillfact.ErrExecutionIdentityConflict
	}
	expected, err := FromFill(e.PortfolioID, &fill, e.CashCurrency, e.Knowledge)
	if err != nil {
		return nil, err
	}
	if e.Type != EntryTrade || e.EntryID != expected.EntryID || e.SourceRef != expected.SourceRef || e.VenueAccountID != expected.VenueAccountID || e.InstrumentID != expected.InstrumentID ||
		e.CashCurrency != expected.CashCurrency || !exactRat(e.Quantity, expected.Quantity) || !exactRat(e.Price, expected.Price) || !exactRat(e.Cash, expected.Cash) ||
		!e.Effective.Equal(expected.Effective) || e.SettlementBasis != expected.SettlementBasis || !e.SettlementDate.Equal(expected.SettlementDate) {
		return nil, fillfact.ErrExecutionIdentityConflict
	}
	return &fill, nil
}

// The alias lock also covers older writers through the migration's trigger.
// A rolling deployment cannot pass a check and then insert the same execution
// under a different key while the newer writer commits.
func prepareExecutionEntry(ctx context.Context, tx pgx.Tx, e *Event) (*Event, error) {
	fill, err := executionFromEntry(e)
	if err != nil || fill == nil {
		return e, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(app_current_tenant()),hashtext('ledger-execution-alias:' || $1))`, fill.GetFillId()); err != nil {
		return nil, err
	}
	legacy := "fill:" + fill.GetFillId()
	if legacy != e.EntryID {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ledger_entries WHERE entry_id=$1)`, legacy).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return nil, ErrLegacyExecutionUnknown
		}
	}
	var evidence []byte
	var portfolio, bookedCurrency string
	err = tx.QueryRow(ctx, `SELECT portfolio_id,cash_currency,execution_evidence FROM ledger_entries WHERE entry_id=$1`, e.EntryID).Scan(&portfolio, &bookedCurrency, &evidence)
	if errors.Is(err, pgx.ErrNoRows) {
		if fill.GetRecovery().GetFeeApproval() != nil {
			previous, err := fillfact.FeeRevisionTerms(fill)
			if err != nil {
				return nil, err
			}
			// This book has never posted the execution: book its approved current
			// economics once, while retaining the revision identity for redelivery.
			if _, _, err := fillfact.RecordFeeRevision(ctx, tx, "ledger", previous, fill); err != nil {
				return nil, err
			}
		}
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	if len(evidence) == 0 {
		return nil, ErrLegacyExecutionUnknown
	}
	var original orderpb.Fill
	if err := proto.Unmarshal(evidence, &original); err != nil {
		return nil, err
	}
	if portfolio != e.PortfolioID || !samePostingScope(&original, fill, bookedCurrency, e.CashCurrency) {
		return nil, fillfact.ErrExecutionIdentityConflict
	}
	if fill.GetRecovery().GetFeeApproval() != nil {
		delta, fresh, err := fillfact.RecordFeeRevision(ctx, tx, "ledger", &original, fill)
		if err != nil {
			return nil, err
		}
		if !fresh {
			return nil, nil
		}
		return feeAdjustment(e, fill, delta), nil
	}
	head, err := fillfact.FeeHead(ctx, tx, "ledger", &original)
	if err != nil {
		return nil, err
	}
	if !fillfact.SameExecution(head, fill) && !(fill.Recovery == nil && fillfact.SameExecution(&original, fill)) {
		return nil, fillfact.ErrExecutionIdentityConflict
	}
	return e, nil
}

func feeAdjustment(trade *Event, fill *orderpb.Fill, delta *big.Rat) *Event {
	adjustment := &Event{EntryID: "fee-correction:" + fill.GetRecovery().GetCaseId() + ":" + fillfact.ExecutionKey(fill), PortfolioID: trade.PortfolioID, VenueAccountID: trade.VenueAccountID, Type: EntryFee, InstrumentID: trade.InstrumentID, Cash: delta, CashCurrency: trade.CashCurrency, Effective: trade.Effective, Knowledge: trade.Knowledge, SettlementBasis: trade.SettlementBasis, SettlementDate: trade.SettlementDate, SourceRef: trade.EntryID, ExecutionEvidence: append([]byte(nil), trade.ExecutionEvidence...)}
	if fill.GetSpotVenueSymbol() != "" && fill.GetFee().GetCurrencyCode() != trade.CashCurrency {
		// SettlementLegs has already established that the only other supported unit is base.
		adjustment.Quantity, adjustment.Price, adjustment.Cash = delta, trade.Price, nil
	}
	return adjustment
}

// A revised fee changes the inventory received at the original execution.
// Apply all visible revisions at that trade's place in the cost-basis fold.
// Posting a later offset at the same price after another trade can change
// realized P&L even when the ending quantity agrees.
func restateBaseFees(ordered []*Event) []*Event {
	type key struct{ portfolio, id string }
	var trades map[key]int
	var seen map[key]bool
	for i, e := range ordered {
		if e.Type != EntryFee || e.Quantity == nil || len(e.ExecutionEvidence) == 0 {
			continue
		}
		if trades == nil {
			trades = make(map[key]int)
			seen = make(map[key]bool)
			for at, entry := range ordered {
				id := key{entry.PortfolioID, entry.EntryID}
				if _, exists := trades[id]; entry.Type == EntryTrade && !exists {
					trades[id] = at
				}
			}
		}
		at, found := trades[key{e.PortfolioID, e.SourceRef}]
		if !found {
			continue
		} // A journal slice may contain only the adjustment.
		id := key{e.PortfolioID, e.EntryID}
		if !seen[id] {
			trade := *ordered[at]
			trade.Quantity = new(big.Rat).Add(trade.Quantity, e.Quantity)
			ordered[at] = &trade
			seen[id] = true
		}
		adjustment := *e
		adjustment.Quantity = nil
		ordered[i] = &adjustment
	}
	return ordered
}
