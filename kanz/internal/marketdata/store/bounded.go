package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// BoundedHistoryTimeout caps database and pool waits even without a caller deadline.
const BoundedHistoryTimeout = 5 * time.Second

// The descending index feeds Unique lazily. LIMIT is above revision collapse,
// never on raw rows: multiple corrections cannot consume the requested window.
const boundedHistorySQL = `SELECT observation_time, kind, price, currency_code, knowledge_time
 FROM (
 SELECT DISTINCT ON (observation_time, kind)
 observation_time, kind, price, currency_code, knowledge_time
 FROM price_observations
 WHERE instrument_id=$1 AND ($2=0 OR kind=$2)
 AND ($3::timestamptz IS NULL OR observation_time >= $3)
 AND ($4::timestamptz IS NULL OR observation_time <= $4)
 AND ($5::timestamptz IS NULL OR knowledge_time <= $5)
 ORDER BY observation_time DESC, kind DESC, knowledge_time DESC
 LIMIT $6
 ) AS recent ORDER BY observation_time, kind`

func (p *Postgres) boundedHistory(ctx context.Context, q Query) ([]Observation, error) {
	if q.InstrumentID == "" {
		return nil, errors.New("store: history with empty instrument_id")
	}
	ctx, cancel := context.WithTimeout(ctx, BoundedHistoryTimeout)
	defer cancel()
	rows, err := p.pool.Query(ctx, boundedHistorySQL, q.InstrumentID, int32(q.Kind), nullTime(q.Start), nullTime(q.End), nullTime(q.AsOf), q.Limit)
	if err != nil {
		return nil, fmt.Errorf("bounded history %s: %w", q.InstrumentID, err)
	}
	defer rows.Close()
	out := make([]Observation, 0, q.Limit)
	for rows.Next() {
		o, err := scanObservation(rows, q.InstrumentID)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func observationBefore(a, b Observation) bool {
	if !a.ObservationTime.Equal(b.ObservationTime) {
		return a.ObservationTime.Before(b.ObservationTime)
	}
	return a.Kind < b.Kind
}

// Memory is a reference store with an unordered backing map. Scan it without
// constructing a lifetime-sized revision map; retain only the newest N identities.
func (m *Memory) boundedHistory(ctx context.Context, q Query) ([]Observation, error) {
	if q.InstrumentID == "" {
		return nil, errors.New("store: history with empty instrument_id")
	}
	ctx, cancel := context.WithTimeout(ctx, BoundedHistoryTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Observation, 0, q.Limit)
	for _, o := range m.byInstrument[q.InstrumentID] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !visible(o, q) {
			continue
		}
		i := sort.Search(len(out), func(i int) bool { return !observationBefore(out[i], o) })
		if i < len(out) && !observationBefore(o, out[i]) {
			if o.KnowledgeTime.After(out[i].KnowledgeTime) {
				out[i] = o
			}
			continue
		}
		if len(out) == q.Limit {
			if i == 0 {
				continue
			}
			copy(out, out[1:])
			out = out[:len(out)-1]
			i--
		}
		out = append(out, Observation{})
		copy(out[i+1:], out[i:])
		out[i] = o
	}
	for i := range out {
		out[i] = cloneObs(out[i])
	}
	return out, nil
}
