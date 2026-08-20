package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PutCoverage stores attestations idempotently. See CoverageStore.
//
// ONE TRANSACTION FOR THE BATCH, every record validated BEFORE any of it is
// sent — the same shape as PutBars, for a sharper reason. A half-written batch
// of coverage leaves intervals that read as UNKNOWN when they were in fact
// observed, and the reader cannot tell that from a feed that was down: the
// record's whole value is that those two states are distinguishable.
//
// # NOT BITEMPORAL, AND THAT IS A DECISION
//
// ohlcv_bars is append-only by knowledge_time because a venue RESTATES a candle
// and a backtest as of a moment before the restatement must still read the old
// one. Coverage has no analogue: an attestation is not revised, it is only ever
// re-proven. Two claims about one bucket are two subscriptions speaking, and the
// merge below takes the larger — which is a sound lower bound on what the
// platform actually observed, where a version history would only record which
// pod published second.
func (p *Postgres) PutCoverage(ctx context.Context, cov []Coverage) error {
	if len(cov) == 0 {
		return nil
	}
	for i := range cov {
		if err := cov[i].Validate(); err != nil {
			return err
		}
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, c := range cov {
		// The row that PROVED MORE wins wholesale: observed_ns, breaks and
		// recorded_at all come from it, so the stored row is one attestor's
		// coherent claim rather than the best-looking field from each.
		batch.Queue(`
			INSERT INTO ingestion_coverage
				(instrument_id, venue, resolution, bucket_start,
				 observed_ns, breaks, attestor, recorded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (instrument_id, venue, resolution, bucket_start, attestor)
			DO UPDATE SET
				observed_ns = GREATEST(EXCLUDED.observed_ns, ingestion_coverage.observed_ns),
				breaks = CASE WHEN EXCLUDED.observed_ns > ingestion_coverage.observed_ns
				              THEN EXCLUDED.breaks ELSE ingestion_coverage.breaks END,
				recorded_at = CASE WHEN EXCLUDED.observed_ns > ingestion_coverage.observed_ns
				                   THEN EXCLUDED.recorded_at ELSE ingestion_coverage.recorded_at END
		`, c.InstrumentID, c.Venue, string(c.Resolution), c.BucketStart.UTC(),
			int64(c.Observed), c.Breaks, c.Attestor, c.RecordedAt.UTC())
	}
	br := tx.SendBatch(ctx, batch)
	for range cov {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("insert coverage: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("close batch: %w", err)
	}
	return tx.Commit(ctx)
}

// Coverage returns the attestations for one series, ascending. See CoverageStore.
//
// EVERY ATTESTOR'S ROW IS RETURNED, not a per-bucket collapse. It takes only one
// subscription to have watched a whole bucket for that bucket's absence to be
// explained, so the OR across attestors belongs to the reader
// (AttestedWindowOf) — collapsing here would have to pick a winner, and picking
// the wrong one turns a covered interval into an unknown one.
func (p *Postgres) Coverage(ctx context.Context, q CoverageQuery) ([]Coverage, error) {
	if err := validateCoverageQuery(q); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `
		SELECT bucket_start, observed_ns, breaks, attestor, recorded_at
		FROM ingestion_coverage
		WHERE instrument_id = $1
		  AND venue = $2
		  AND resolution = $3
		  AND ($4::timestamptz IS NULL OR bucket_start >= $4)
		  AND ($5::timestamptz IS NULL OR bucket_start < $5)
		ORDER BY bucket_start, attestor
	`, q.InstrumentID, q.Venue, string(q.Resolution), nullTime(q.From), nullTime(q.To))
	if err != nil {
		return nil, fmt.Errorf("query coverage %s@%s: %w", q.InstrumentID, q.Venue, err)
	}
	defer rows.Close()

	var out []Coverage
	for rows.Next() {
		c := Coverage{InstrumentID: q.InstrumentID, Venue: q.Venue, Resolution: q.Resolution}
		var observedNs int64
		if err := rows.Scan(&c.BucketStart, &observedNs, &c.Breaks, &c.Attestor, &c.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan coverage: %w", err)
		}
		c.Observed = time.Duration(observedNs)
		out = append(out, c)
	}
	return out, rows.Err()
}

var _ CoverageStore = (*Postgres)(nil)
