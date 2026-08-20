package store

import (
	"context"
	"sort"
)

// coverageKey is one attestation's identity: a series' bucket, per ATTESTOR.
//
// The attestor is in the key because two subscriptions may cover one series and
// each can only speak for itself. Collapsing them would make the last writer's
// claim the platform's claim, and the last writer is whichever pod happened to
// publish second.
type coverageKey struct {
	instrumentID string
	venue        string
	resolution   Resolution
	bucketUnixNs int64
	attestor     string
}

// PutCoverage stores attestations idempotently. See CoverageStore.
//
// EVERY RECORD IS VALIDATED BEFORE ANY IS STORED, matching Postgres. The memory
// store is what most tests run against, so a laxer contract here would mean the
// suite proves a store the database does not implement — and the contract that
// matters most is the one Validate enforces: an over-claim is refused, never
// clamped.
func (m *Memory) PutCoverage(_ context.Context, cov []Coverage) error {
	for i := range cov {
		if err := cov[i].Validate(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.coverage == nil {
		m.coverage = map[coverageKey]Coverage{}
	}
	for _, c := range cov {
		k := coverageKey{
			instrumentID: c.InstrumentID,
			venue:        c.Venue,
			resolution:   c.Resolution,
			bucketUnixNs: c.BucketStart.UTC().UnixNano(),
			attestor:     c.Attestor,
		}
		// max(Observed), NOT last-write-wins — see CoverageStore.PutCoverage. A
		// redelivery carrying a shorter claim must not retract coverage that was
		// genuinely proven; the row that proved more wins WHOLESALE, so Breaks and
		// RecordedAt travel with it rather than being mixed across two claims.
		if cur, ok := m.coverage[k]; ok && cur.Observed >= c.Observed {
			continue
		}
		m.coverage[k] = c
	}
	return nil
}

// Coverage returns the attestations for one series, ascending by BucketStart.
// See CoverageStore.
//
// AN INTERVAL WITH NO ROW IS SIMPLY ABSENT FROM THE RESULT. It is not returned
// as a zero-coverage record, because "nobody attested this" and "an attestor
// said it saw nothing" are different facts, and only the second is a
// measurement. Callers distinguish them with AttestedWindowOf.
func (m *Memory) Coverage(_ context.Context, q CoverageQuery) ([]Coverage, error) {
	if err := validateCoverageQuery(q); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []Coverage
	for k, c := range m.coverage {
		if k.instrumentID != q.InstrumentID || k.venue != q.Venue || k.resolution != q.Resolution {
			continue
		}
		if !q.From.IsZero() && c.BucketStart.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && !c.BucketStart.Before(q.To) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].BucketStart.Equal(out[j].BucketStart) {
			return out[i].BucketStart.Before(out[j].BucketStart)
		}
		return out[i].Attestor < out[j].Attestor
	})
	return out, nil
}

// coverageCount reports how many attestations are held, for tests that need to
// prove two attestors coexist rather than one replacing the other.
func (m *Memory) coverageCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.coverage)
}
