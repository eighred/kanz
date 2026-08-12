package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// barKey is a bar's full bitemporal identity within one series, so a restatement
// (same bucket, newer knowledge_time) is a distinct entry and an exact re-put
// overwrites in place — idempotent, exactly as the Postgres ON CONFLICT DO
// NOTHING is.
type barKey struct {
	instrumentID  string
	venue         string
	resolution    Resolution
	bucketUnixNs  int64
	knowledgeUnix int64
}

// PutBars stores bars idempotently. See BarStore.
//
// EVERY BAR IS VALIDATED BEFORE ANY IS STORED, matching Postgres: a batch that
// half-applies leaves a hole in a series, and a hole in a bar series is invisible
// downstream — an indicator simply computes a different number and nothing says
// why. The memory store is what most tests run against, so a laxer contract here
// would mean the suite proves a store the database does not implement.
func (m *Memory) PutBars(_ context.Context, bars []Bar) error {
	for i := range bars {
		if err := bars[i].Validate(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bars == nil {
		m.bars = map[barKey]Bar{}
	}
	for _, b := range bars {
		m.bars[barKey{
			instrumentID:  b.InstrumentID,
			venue:         b.Venue,
			resolution:    b.Resolution,
			bucketUnixNs:  b.BucketStart.UTC().UnixNano(),
			knowledgeUnix: b.KnowledgeTime.UTC().UnixNano(),
		}] = b
	}
	return nil
}

// Bars returns the point-in-time-correct series. See BarStore.
func (m *Memory) Bars(_ context.Context, q BarQuery) ([]Bar, error) {
	if q.InstrumentID == "" {
		return nil, errors.New("store: bars with empty instrument_id")
	}
	if q.Venue == "" {
		return nil, errors.New("store: bars with empty venue — a series is per venue, and " +
			"collapsing venues would silently blend two different markets into one candle")
	}
	if !q.Resolution.Valid() {
		return nil, fmt.Errorf("store: bars with resolution %q, which is not one this platform stores",
			q.Resolution)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	// LATEST KNOWN BY AsOf, PER BUCKET — the same collapse the SQL DISTINCT ON
	// performs. Written out rather than "last write wins" because the two differ
	// exactly when it matters: a correction that arrived after AsOf must not be
	// visible, and a store that returned it would make every backtest read the
	// future.
	latest := map[int64]Bar{}
	for k, b := range m.bars {
		if k.instrumentID != q.InstrumentID || k.venue != q.Venue || k.resolution != q.Resolution {
			continue
		}
		if !q.From.IsZero() && b.BucketStart.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && !b.BucketStart.Before(q.To) {
			continue
		}
		if !q.AsOf.IsZero() && b.KnowledgeTime.After(q.AsOf) {
			continue
		}
		if cur, ok := latest[k.bucketUnixNs]; ok && !b.KnowledgeTime.After(cur.KnowledgeTime) {
			continue
		}
		latest[k.bucketUnixNs] = b
	}

	out := make([]Bar, 0, len(latest))
	for _, b := range latest {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketStart.Before(out[j].BucketStart) })
	return out, nil
}

// barCount reports how many bar versions are held, for tests that need to prove
// a restatement COEXISTS with the original rather than replacing it.
func (m *Memory) barCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.bars)
}
