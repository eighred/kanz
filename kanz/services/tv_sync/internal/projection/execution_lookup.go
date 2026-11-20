package projection

import (
	"context"
	"crypto/sha256"

	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Each lookup bounds both retained rows and protobuf input. Older writers leave
// the hints NULL. Their original payloads can establish an identity, but an
// incomplete search cannot establish absence.
const maxPriorExecutionCandidates = 128

func lookupFill(f Fact) (string, *orderpb.Fill, error) {
	var state *orderpb.OrderState
	var fill *orderpb.Fill
	var id string
	switch f.EventType {
	case evtFilled:
		var ev orderpb.OrderFilled
		if !decodeFact(f.Payload, &ev) {
			return "", nil, ErrUnresolvedFillAlias
		}
		id, state, fill = ev.OrderId, ev.State, ev.Fill
	case evtPartiallyFilled:
		var ev orderpb.OrderPartiallyFilled
		if !decodeFact(f.Payload, &ev) {
			return "", nil, ErrUnresolvedFillAlias
		}
		id, state, fill = ev.OrderId, ev.State, ev.Fill
	default:
		return "", nil, nil
	}
	if fill.GetFillId() == "" || state.GetPortfolioId() == "" {
		return "", nil, ErrUnresolvedFillAlias
	}
	fill, err := normalizedOrderFill(id, state, fill)
	return state.GetPortfolioId(), fill, err
}

func fillLookupKeys(f *orderpb.Fill) ([]byte, []byte) {
	alias := sha256.Sum256([]byte(f.GetFillId()))
	key := fillfact.ExecutionKey(f)
	if key == f.GetFillId() {
		return alias[:], nil
	}
	scoped := sha256.Sum256([]byte(key))
	return alias[:], scoped[:]
}

func factLookupKeys(f Fact) ([]byte, []byte) {
	_, fill, err := lookupFill(f)
	if err != nil || fill == nil {
		return nil, nil
	}
	return fillLookupKeys(fill)
}

func (l *PostgresLog) priorExecution(ctx context.Context, before int64, portfolio string, fill *orderpb.Fill) (bool, error) {
	alias, key := fillLookupKeys(fill)
	// The first prior indexed claim is sufficient: a contradictory delivery
	// could never have passed its fold watermark. Unindexed raw facts need
	// individual decoding to decide whether they concern this execution.
	rows, err := l.db.Query(ctx, `
		SELECT event_type,CASE WHEN octet_length(payload)<=$5 THEN payload END FROM (
			(SELECT event_type,payload FROM tv_facts WHERE seq<$1 AND execution_key_digest=$3 ORDER BY seq LIMIT 1)
			UNION ALL
			(SELECT event_type,payload FROM tv_facts WHERE seq<$1 AND fill_alias_digest=$2 AND (execution_key_digest IS NULL OR $3::bytea IS NULL) ORDER BY seq LIMIT 1)
			UNION ALL
			(SELECT event_type,payload FROM tv_facts WHERE seq<$1 AND fill_alias_digest IS NULL AND event_type IN ('order.order.filled','order.order.partially_filled') LIMIT $4)
		) AS candidates LIMIT $4`, before, alias, key, maxPriorExecutionCandidates+1, ReplayMaxFactBytes)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	duplicate := false
	for count := 0; rows.Next(); count++ {
		var previous Fact
		if err := rows.Scan(&previous.EventType, &previous.Payload); err != nil {
			return false, err
		}
		if count == maxPriorExecutionCandidates || previous.Payload == nil {
			return false, ErrUnresolvedFillAlias
		}
		owner, prior, err := lookupFill(previous)
		if err != nil || prior == nil {
			return false, ErrUnresolvedFillAlias
		}
		match, err := priorExecutionMatch(portfolio, fill, owner, prior)
		if err != nil {
			return false, err
		}
		duplicate = duplicate || match
	}
	return duplicate, rows.Err()
}

func priorExecutionMatch(portfolio string, fill *orderpb.Fill, owner string, prior *orderpb.Fill) (bool, error) {
	priorKey, key := fillfact.ExecutionKey(prior), fillfact.ExecutionKey(fill)
	if prior.FillId == fill.FillId && (priorKey == prior.FillId || key == fill.FillId) {
		return false, ErrUnresolvedFillAlias
	}
	if priorKey != key {
		return false, nil
	}
	if owner != portfolio || !fillfact.SameExecution(prior, fill) {
		return false, fillfact.ErrExecutionIdentityConflict
	}
	return true, nil
}
