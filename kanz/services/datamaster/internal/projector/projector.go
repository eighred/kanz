// Package projector is the consumer of the vendor feeds and the writer of the
// golden store (MASTER-01b/d). It is the piece that was missing: master.Resolve,
// pricing.Arbitrate, store.GoldenStore and store.ExceptionStore all existed and
// were all tested, but nothing ever drove them — the golden_records table had no
// writer, so the read API resolved the master from the live feeds on every single
// request.
//
// The projector closes that loop. On each cycle it pulls every vendor's reference
// records and price candidates, resolves one golden record per instrument
// (survivorship + crosswalk), writes it to the durable GoldenStore, and files
// every detected break — identifier conflicts, stale prices, tolerance breaches,
// missing prices — into the durable ExceptionStore. The read API then serves what
// was resolved rather than resolving as a side effect of being read.
//
// # A refresh is all-or-nothing across the feeds
//
// If any vendor errors, the whole cycle aborts and the store keeps its previous
// projection. This is deliberate: survivorship picks the winning field from the
// HIGHEST-PRIORITY vendor that has it, so resolving over a subset of the vendors
// silently produces a DIFFERENT golden record — an instrument whose primary vendor
// is down would have its identifiers and classification quietly overwritten from a
// fallback source, with the provenance stamped as if that were the intended
// answer. A stale-but-correct golden record beats a fresh degraded one. The next
// cycle retries.
package projector

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/kanz-eng/kanz/services/datamaster/internal/feed"
	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
	"github.com/kanz-eng/kanz/services/datamaster/internal/store"
)

// Projector folds the vendor feeds into the golden store and the exception queue.
type Projector struct {
	feeds      []feed.VendorFeed
	golden     store.GoldenStore
	exceptions store.ExceptionStore
	tolerance  float64
	staleness  time.Duration
	now        func() time.Time
	logger     *slog.Logger
}

// Option customizes the projector.
type Option func(*Projector)

// WithTolerance overrides the price-arbitration tolerance (≤ 0 ⇒ the default).
func WithTolerance(t float64) Option { return func(p *Projector) { p.tolerance = t } }

// WithStaleness overrides the price-staleness window (≤ 0 ⇒ the default).
func WithStaleness(d time.Duration) Option { return func(p *Projector) { p.staleness = d } }

// WithClock overrides the clock (tests pin a fixed now for staleness checks).
func WithClock(now func() time.Time) Option { return func(p *Projector) { p.now = now } }

// New builds a projector over the feeds and the two stores.
func New(feeds []feed.VendorFeed, golden store.GoldenStore, exceptions store.ExceptionStore, logger *slog.Logger, opts ...Option) *Projector {
	p := &Projector{
		feeds:      feeds,
		golden:     golden,
		exceptions: exceptions,
		now:        time.Now,
		logger:     logger,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	return p
}

// Run refreshes on an interval until ctx is cancelled. A failed cycle is logged
// and retried on the next tick — the store holds the last good projection in the
// meantime.
func (p *Projector) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.Refresh(ctx); err != nil && ctx.Err() == nil {
				p.logger.Error("golden refresh failed; serving the previous projection", "err", err)
			}
		}
	}
}

// Refresh runs one full cycle: collect → resolve → persist → file exceptions.
func (p *Projector) Refresh(ctx context.Context) error {
	records, candidates, err := p.collect(ctx)
	if err != nil {
		return err
	}
	now := p.now()

	for _, id := range sortedKeys(records) {
		golden, conflicts := master.Resolve(records[id])
		if err := p.golden.Put(ctx, golden); err != nil {
			return fmt.Errorf("persist golden record %s: %w", id, err)
		}
		for _, c := range conflicts {
			if err := p.exceptions.Add(ctx, pricing.Exception{
				ID:           pricing.ExceptionID(id, pricing.KindIdentifierConflict, string(c.Scheme)),
				Kind:         pricing.KindIdentifierConflict,
				InstrumentID: id,
				Detail:       c.Error(),
				Status:       pricing.StatusOpen,
				DetectedAt:   now,
			}); err != nil {
				return fmt.Errorf("file identifier conflict %s: %w", id, err)
			}
		}
	}

	// Arbitrate every instrument we know about, not just the ones with quotes: an
	// instrument in the master with NO price is exactly the MISSING_PRICE break the
	// oversight queue exists to surface, and it must not take a human reading the
	// price endpoint to notice. (So a deployment with reference feeds but no price
	// feed raises MISSING_PRICE across the book — which is true, and the remedy is
	// to wire a price feed.)
	for _, id := range sortedKeys(union(records, candidates)) {
		a := pricing.Arbitrate(id, candidates[id], p.tolerance, p.staleness, now)
		if err := p.exceptions.AddAll(ctx, a.Exceptions); err != nil {
			return fmt.Errorf("file price exceptions %s: %w", id, err)
		}
	}

	p.logger.Debug("golden projection refreshed", "instruments", len(records), "priced", len(candidates))
	return nil
}

// collect pulls every feed's records and price candidates, keyed by instrument.
// A single failing vendor aborts the cycle (see the package comment).
func (p *Projector) collect(ctx context.Context) (map[string][]master.VendorRecord, map[string][]pricing.Candidate, error) {
	records := map[string][]master.VendorRecord{}
	candidates := map[string][]pricing.Candidate{}
	for _, f := range p.feeds {
		recs, err := f.Records(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("vendor %s records: %w", f.Vendor(), err)
		}
		for _, r := range recs {
			if r.InstrumentID == "" {
				p.logger.Warn("vendor record has no instrument id; cannot be mastered", "vendor", f.Vendor())
				continue
			}
			records[r.InstrumentID] = append(records[r.InstrumentID], r)
		}
		prices, err := f.Prices(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("vendor %s prices: %w", f.Vendor(), err)
		}
		for _, c := range prices {
			if c.InstrumentID == "" {
				p.logger.Warn("price candidate has no instrument id; cannot be arbitrated", "vendor", f.Vendor(), "source", c.Source)
				continue
			}
			candidates[c.InstrumentID] = append(candidates[c.InstrumentID], c)
		}
	}
	return records, candidates, nil
}

// union returns the instrument ids present in either map.
func union(records map[string][]master.VendorRecord, candidates map[string][]pricing.Candidate) map[string]struct{} {
	ids := make(map[string]struct{}, len(records)+len(candidates))
	for id := range records {
		ids[id] = struct{}{}
	}
	for id := range candidates {
		ids[id] = struct{}{}
	}
	return ids
}

// sortedKeys makes a cycle deterministic: the same feed state writes the same
// rows in the same order, so a projection is reproducible and diffable.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
