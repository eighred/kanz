package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

// DefaultSnapshotInterval bounds how much of the durable log a restart has
// to replay: a snapshot every 30s means the bootstrap path (PERS-01d)
// resumes at most ~30s of events behind the crash. Tunable — shorten to
// shrink replay time at the cost of more write volume, lengthen for quiet
// books.
const DefaultSnapshotInterval = 30 * time.Second

// LogPositionSource reports the durable-log coordinate the engine has
// applied up to for a portfolio, at snapshot time — the value stamped
// onto the persisted record so the bootstrap path resumes the log at
// Offset+1 (the EVT-11 PortfolioSnapshot.log_position role).
//
// It is a seam, not yet a concrete type: ORCH-01 ingests from the NATS
// live spine, where the durable Kafka offset is not on the envelope, so
// the engine has no log coordinate to stamp on its own. PERS-01d wires a
// concrete source backed by the Kafka consumer's committed offsets. Until
// then the source is nil and the snapshotter falls back to the
// portfolio's own LogPosition (set only by an inbound PortfolioSnapshot
// apply, RISK-05) — which may be nil, in which case the record carries no
// resume hint and the bootstrap path replays from the log's start.
type LogPositionSource interface {
	// LogPositionFor returns the coordinate for the portfolio, or nil
	// when none is known.
	LogPositionFor(id v1.PortfolioID) *commonpb.LogPosition
}

// Snapshotter periodically materializes every known portfolio's committed
// state into the durable StateStore (PERS-01b) — the cold copy a restarted
// engine loads before replay. Each saved PortfolioRecord is the durable
// analog of an EVT-11 PortfolioSnapshot: full aggregate + position state,
// the LogPosition it includes up to, and the applied-key tail that lets
// replay stay idempotent at the boundary.
//
// # Why periodic, not per-apply
//
// Persisting on every apply would put the SQL write on the hot ingestion
// path. The engine's authoritative state is in memory (state.Store); the
// durable store only has to be recent enough to bound restart replay, so a
// coarse cadence (DefaultSnapshotInterval) is the right trade. A final
// Checkpoint on graceful shutdown (the lifecycle drain seam) captures the
// last state so a clean restart replays almost nothing.
type Snapshotter struct {
	store    *state.Store
	sink     persist.StateStore
	logPos   LogPositionSource // may be nil — see LogPositionSource
	interval time.Duration
	logger   *slog.Logger
}

// NewSnapshotter constructs a Snapshotter. logPos may be nil (fall back to
// each portfolio's own LogPosition); a non-positive interval falls back to
// DefaultSnapshotInterval; a nil logger uses slog.Default.
func NewSnapshotter(
	store *state.Store,
	sink persist.StateStore,
	logPos LogPositionSource,
	interval time.Duration,
	logger *slog.Logger,
) *Snapshotter {
	if interval <= 0 {
		interval = DefaultSnapshotInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Snapshotter{store: store, sink: sink, logPos: logPos, interval: interval, logger: logger}
}

// Run drives the periodic checkpoint until ctx is canceled, then returns
// nil. The caller runs it in a goroutine; graceful shutdown cancels ctx
// and is expected to call Checkpoint once more (the lifecycle drain) so
// the final state is durable. A per-tick checkpoint error is logged, not
// returned — a transient store blip must not kill the loop.
func (s *Snapshotter) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.Checkpoint(ctx); err != nil {
				s.logger.Error("periodic checkpoint failed", "err", err)
			}
		}
	}
}

// Checkpoint persists a durable snapshot of every known portfolio in one
// pass. Per-portfolio failures are collected and joined so one bad
// aggregate doesn't skip the rest — the caller (periodic loop or shutdown
// drain) decides what to do with the joined error. A portfolio retired
// between IDs() and the read is silently skipped.
func (s *Snapshotter) Checkpoint(ctx context.Context) error {
	var errs []error
	for _, id := range s.store.IDs() {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := s.checkpointOne(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Snapshotter) checkpointOne(ctx context.Context, id v1.PortfolioID) error {
	p, keys, ok := s.store.SnapshotWithKeys(id)
	if !ok {
		return nil // retired between IDs() and the read
	}
	rec := persist.FromPortfolio(p, keys)
	// Prefer the live durable-log coordinate when a source is wired; else
	// keep the portfolio's own LogPosition (already set by FromPortfolio).
	if s.logPos != nil {
		if lp := s.logPos.LogPositionFor(id); lp != nil {
			rec.LogPosition = lp
		}
	}
	if err := s.sink.Save(ctx, rec); err != nil {
		return fmt.Errorf("checkpoint portfolio %s: %w", id, err)
	}
	return nil
}
