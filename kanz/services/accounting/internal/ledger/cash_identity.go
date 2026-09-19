package ledger

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	pb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var ErrCashConflict = errors.New("ledger: cash entry identity conflicts with accepted or recorded terms")

func isCashMovement(e *Event) bool { return strings.HasPrefix(e.EntryID, "cash:") }
func validCashEntry(e *Event) bool {
	return (e.Type == EntryCash || e.Type == EntryFee) && e.Cash != nil && e.InstrumentID == "" && e.Quantity == nil && e.Price == nil && e.Action == nil
}
func sameCash(a, b *Event) bool {
	return a.PortfolioID == b.PortfolioID && a.VenueAccountID == b.VenueAccountID && a.Type == b.Type && a.CashCurrency == b.CashCurrency &&
		a.SourceRef == b.SourceRef && a.Effective.UTC().Truncate(time.Microsecond).Equal(b.Effective.UTC().Truncate(time.Microsecond)) &&
		a.SettlementBasis == b.SettlementBasis && a.SettlementDate.Equal(b.SettlementDate) && a.Cash != nil && b.Cash != nil && a.Cash.Cmp(b.Cash) == 0
}

// checkCashEntry shares the acceptance lock, verifies the immutable command if
// one exists, and refuses changed retries rather than silently discarding them.
// Legacy redelivery preserves the original knowledge time: delivery time alone
// is not a new economic event. New commands carry a fixed accepted timestamp.
func checkCashEntry(ctx context.Context, tx pgx.Tx, e *Event) (bool, error) {
	if !validCashEntry(e) {
		return false, ErrCashConflict
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(app_current_tenant()||'/cash-entry/'||$1,0))`, e.EntryID); err != nil {
		return false, err
	}
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM cash_commands WHERE movement_id=$1`, strings.TrimPrefix(e.EntryID, "cash:")).Scan(&payload)
	if err == nil {
		var expected pb.LedgerEntry
		if err = proto.Unmarshal(payload, &expected); err != nil {
			return false, err
		}
		kind := EntryCash
		if expected.EntryType == pb.EntryType_ENTRY_TYPE_FEE {
			kind = EntryFee
		}
		cash, ok := dec.FromProtoChecked(expected.Cash)
		if !ok {
			return false, ErrCashConflict
		}
		want := &Event{PortfolioID: expected.PortfolioId, VenueAccountID: expected.VenueAccountId, Type: kind, Cash: cash, CashCurrency: expected.CashCurrency, Effective: expected.EffectiveTime.AsTime(), SourceRef: expected.SourceRef}
		if !sameCash(e, want) || !e.Knowledge.Equal(expected.KnowledgeTime.AsTime()) {
			return false, ErrCashConflict
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var old Event
	var amount *string
	var settled *time.Time
	err = tx.QueryRow(ctx, `SELECT portfolio_id,venue_account_id,entry_type,cash,cash_currency,effective_time,settlement_status,settlement_date,source_ref FROM ledger_entries WHERE entry_id=$1`, e.EntryID).Scan(&old.PortfolioID, &old.VenueAccountID, &old.Type, &amount, &old.CashCurrency, &old.Effective, &old.SettlementBasis, &settled, &old.SourceRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if settled != nil {
		old.SettlementDate = *settled
	}
	old.Cash, err = parseRat(amount)
	if err != nil {
		return false, err
	}
	if !sameCash(e, &old) {
		return false, ErrCashConflict
	}
	return true, nil
}
