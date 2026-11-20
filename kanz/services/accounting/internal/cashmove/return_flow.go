package cashmove

import (
	"context"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// RecordedExternalFlow binds a signed assessment to the single booked external
// off-venue cash movement in its window. The count covers all currencies and
// accounts so a second booked cash leg cannot disappear from the proof.
func RecordedExternalFlow(ctx context.Context, tx pgx.Tx, tenant, portfolio, ccy, movement, digest string,
	start, end, horizon, effective time.Time, amount *big.Rat,
) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT e.entry_id,e.venue_account_id,e.cash_currency,e.cash,e.effective_time,e.knowledge_time,e.source_ref,
		c.digest,c.payload FROM ledger_entries e
		LEFT JOIN cash_commands c ON c.tenant_id=e.tenant_id AND c.portfolio_id=e.portfolio_id
			AND e.entry_id LIKE 'cash:%' AND c.movement_id=substring(e.entry_id from 6)
		WHERE e.tenant_id=public.app_current_tenant() AND e.tenant_id=$1 AND e.portfolio_id=$2
			AND e.entry_type=$3 AND e.effective_time>$4 AND e.effective_time<=$5
			AND e.knowledge_time<=$6 AND e.created_at<=$6 LIMIT 2`,
		tenant, portfolio, ledger.EntryCash, start, end, horizon)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var id, account, currency, source string
	var cash, acceptedDigest *string
	var at, known time.Time
	var payload []byte
	if err := rows.Scan(&id, &account, &currency, &cash, &at, &known, &source, &acceptedDigest, &payload); err != nil {
		return false, err
	}
	if rows.Next() {
		return false, nil
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if id != "cash:"+movement || account != "" || currency != ccy || cash == nil || acceptedDigest == nil ||
		*acceptedDigest != digest || !at.Equal(effective) {
		return false, nil
	}
	booked, err := dec.Exact(*cash).Rat()
	if err != nil || booked.Cmp(amount) != 0 {
		return false, nil
	}
	var entry accountingpb.LedgerEntry
	if proto.Unmarshal(payload, &entry) != nil || entry.EntryId != id || entry.PortfolioId != portfolio || entry.VenueAccountId != account ||
		entry.EntryType != accountingpb.EntryType_ENTRY_TYPE_CASH || entry.CashCurrency != ccy || entry.SourceRef != source ||
		entry.GetEffectiveTime().CheckValid() != nil || !entry.EffectiveTime.AsTime().Equal(at) ||
		entry.GetKnowledgeTime().CheckValid() != nil || !entry.KnowledgeTime.AsTime().Equal(known) {
		return false, nil
	}
	accepted, ok := dec.FromProtoChecked(entry.Cash)
	return ok && accepted.Cmp(booked) == 0, nil
}
