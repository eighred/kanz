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

// ExternalFlowProof identifies one signed off-venue movement and its signed amount.
type ExternalFlowProof struct {
	MovementID, CommandDigest string
	Effective                 time.Time
	Amount                    *big.Rat
}

// RecordedExternalFlows binds the ordered signed movements to the entire cash
// journal for (start,end] known at horizon. An extra currency, venue account
// or legacy entry must remain visible to the comparison.
func RecordedExternalFlows(ctx context.Context, tx pgx.Tx, tenant, portfolio, ccy string,
	start, end, horizon time.Time, proofs []ExternalFlowProof,
) (bool, error) {
	if len(proofs) == 0 || tenant == "" || portfolio == "" || ccy == "" || !start.Before(end) || horizon.Before(end) {
		return false, nil
	}
	rows, err := tx.Query(ctx, `SELECT e.entry_id,e.venue_account_id,e.cash_currency,e.cash,e.effective_time,e.knowledge_time,e.source_ref,
		c.digest,c.payload FROM ledger_entries e
		LEFT JOIN cash_commands c ON c.tenant_id=e.tenant_id AND c.portfolio_id=e.portfolio_id
			AND e.entry_id LIKE 'cash:%' AND c.movement_id=substring(e.entry_id from 6)
		WHERE e.tenant_id=public.app_current_tenant() AND e.tenant_id=$1 AND e.portfolio_id=$2
			AND e.entry_type=$3 AND e.effective_time>$4 AND e.effective_time<=$5
			AND e.knowledge_time<=$6 AND e.created_at<=$6
		ORDER BY e.effective_time LIMIT $7`,
		tenant, portfolio, ledger.EntryCash, start, end, horizon, int64(len(proofs))+1)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for i, proof := range proofs {
		if proof.Amount == nil || (i > 0 && !proof.Effective.After(proofs[i-1].Effective)) {
			return false, nil
		}
		if !rows.Next() {
			return false, rows.Err()
		}
		matches, err := recordedCashMatches(rows, portfolio, ccy, proof)
		if err != nil || !matches {
			return false, err
		}
	}
	if rows.Next() {
		return false, nil
	}
	err = rows.Err()
	return err == nil, err
}

func recordedCashMatches(row pgx.Row, portfolio, ccy string, proof ExternalFlowProof) (bool, error) {
	var id, account, currency, source string
	var cash, acceptedDigest *string
	var at, known time.Time
	var payload []byte
	if err := row.Scan(&id, &account, &currency, &cash, &at, &known, &source, &acceptedDigest, &payload); err != nil {
		return false, err
	}
	if id != "cash:"+proof.MovementID || account != "" || currency != ccy || cash == nil || acceptedDigest == nil ||
		*acceptedDigest != proof.CommandDigest || !at.Equal(proof.Effective) {
		return false, nil
	}
	booked, err := dec.Exact(*cash).Rat()
	if err != nil || booked.Cmp(proof.Amount) != 0 {
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
