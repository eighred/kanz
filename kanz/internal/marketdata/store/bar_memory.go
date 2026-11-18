package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"
)

// barKey identifies one immutable knowledge-time revision of a venue candle.
type barKey struct {
	instrumentID  string
	venue         string
	resolution    Resolution
	bucketUnixNs  int64
	knowledgeUnix int64
}

// barRevisionKey uses the same instrument, venue, resolution, bucket and
// knowledge time as the durable candle's conflict target.
func barRevisionKey(instrumentID, venue string, resolution Resolution, bucketStart, knowledgeTime time.Time) barKey {
	return barKey{instrumentID: instrumentID, venue: venue, resolution: resolution, bucketUnixNs: bucketStart.UnixNano(), knowledgeUnix: knowledgeTime.UnixNano()}
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
	pending := make(map[barKey]Bar)
	for _, b := range bars {
		key := barRevisionKey(b.InstrumentID, b.Venue, b.Resolution, b.BucketStart, b.KnowledgeTime)
		existing, ok := pending[key]
		if !ok {
			existing, ok = m.bars[barRevisionKey(b.InstrumentID, b.Venue, b.Resolution, b.BucketStart, b.KnowledgeTime)]
		}
		if ok {
			if !SameCandle(existing, b) {
				return b.revisionConflict()
			}
			continue
		}
		pending[key] = cloneBar(b)
	}
	if m.bars == nil {
		m.bars = make(map[barKey]Bar, len(pending))
	}
	for _, b := range pending {
		m.bars[barRevisionKey(b.InstrumentID, b.Venue, b.Resolution, b.BucketStart, b.KnowledgeTime)] = b
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
		if !b.Resolution.alignedBucket(b.BucketStart) {
			return nil, b.gridError()
		}
		if cur, ok := latest[k.bucketUnixNs]; ok && !b.KnowledgeTime.After(cur.KnowledgeTime) {
			continue
		}
		latest[k.bucketUnixNs] = b
	}

	out := make([]Bar, 0, len(latest))
	for _, b := range latest {
		out = append(out, cloneBar(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketStart.Before(out[j].BucketStart) })
	return out, nil
}

// A caller may reuse its protobufs and trade count after a write or read.
func cloneBar(b Bar) Bar {
	b.Open = proto.CloneOf(b.Open)
	b.High = proto.CloneOf(b.High)
	b.Low = proto.CloneOf(b.Low)
	b.Close = proto.CloneOf(b.Close)
	b.Volume = proto.CloneOf(b.Volume)
	if b.TradeCount != nil {
		count := *b.TradeCount
		b.TradeCount = &count
	}
	return b
}

// barCount reports how many bar versions are held, for tests that need to prove
// a restatement COEXISTS with the original rather than replacing it.
func (m *Memory) barCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.bars)
}
