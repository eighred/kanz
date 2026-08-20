package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
	"github.com/eighred/kanz/tools/replay"
)

// Bootstrap is the restart-recovery path (PERS-01d): load the latest durable
// snapshot of every portfolio into the in-memory state.Store, replay the
// Kafka log of record forward from each snapshot's LogPosition to catch up
// the gap, then return so the caller switches to the live NATS spine.
//
// # Why three phases, in this order
//
//  1. Restore (durable store → memory). Rehydrates aggregate state AND
//     pre-seeds each portfolio's dedup window from the persisted applied-key
//     tail (state.Store.Restore). The pre-seed is what makes phase 2
//     idempotent.
//  2. Replay (Kafka → memory). Re-applies events committed to the durable
//     log after the snapshot. Applied through the SAME RISK-04 Ingestor as
//     live traffic, against the BARE state.Store (not the TriggeringApplier)
//     so replay does not emit a storm of intermediate, immediately-superseded
//     risk FACTs — the query path computes from state on demand, and live
//     ingestion re-arms the recompute reaction. Events already folded into
//     the snapshot are recognized by the seeded dedup window and skipped, so
//     the replay can safely start at-or-before the true resume point (RPO=0,
//     no double-count).
//  3. Switch to live (caller). After Run returns the caller starts the live
//     Ingest; the NATS durable consumer resumes from its own position and the
//     same per-aggregate dedup absorbs any overlap with the tail of the
//     replay.
//
// # Resume coordinate
//
// A snapshot's LogPosition is a single (topic, partition, offset). Replay
// starts each (topic, partition) at the MINIMUM offset+1 seen across all
// loaded snapshots for it — conservative (a portfolio sharing the partition
// whose snapshot was older pulls the start back), which idempotent apply
// makes free. A (topic, partition) with no recorded position is NOT replayed
// from genesis; it is left to the live spine, with a logged warning. Complete
// per-stream replay needs the snapshotter's LogPositionSource populated
// (PERS-01c seam, nil today) so positions actually land on records.
type Bootstrap struct {
	store   *state.Store
	sink    persist.StateStore
	applier ingest.Applier // bare state.Store — see phase 2
	brokers []string
	logger  *slog.Logger

	now       func() time.Time
	newSource sourceFactory

	// foreign counts the durable records the last restore declined because the
	// shard ring assigns them elsewhere (#110). Written by restore and read by
	// the composition root after Run; both are single-threaded with respect to
	// each other (boot completes before live ingestion starts).
	foreign int
}

// eventSource is the slice of replay.Reader the Bootstrap drains; an
// interface so PERS-01e tests inject an in-memory log without Kafka.
type eventSource interface {
	Next(ctx context.Context) (replay.Event, error)
	Close() error
}

type sourceFactory func(replay.Config) (eventSource, error)

func defaultSourceFactory(cfg replay.Config) (eventSource, error) { return replay.NewReader(cfg) }

// NewBootstrap builds the recovery path. store and sink are required;
// applier is normally the bare store (so replay is side-effect-free);
// brokers may be empty to skip the replay phase. A nil logger uses
// slog.Default.
func NewBootstrap(store *state.Store, sink persist.StateStore, applier ingest.Applier, brokers []string, logger *slog.Logger) (*Bootstrap, error) {
	if store == nil || sink == nil {
		return nil, errors.New("app: bootstrap requires store and sink")
	}
	if applier == nil {
		return nil, errors.New("app: bootstrap requires applier")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Bootstrap{
		store:     store,
		sink:      sink,
		applier:   applier,
		brokers:   brokers,
		logger:    logger,
		now:       time.Now,
		newSource: defaultSourceFactory,
	}, nil
}

// resumeKey identifies one durable-log partition to resume from.
type resumeKey struct {
	topic     string
	partition int
}

// Run executes the restore then replay phases. It returns before the caller
// starts live ingestion. A restore failure aborts (we cannot run on partial
// state); a replay source failure aborts too, but malformed individual frames
// are logged and skipped.
func (b *Bootstrap) Run(ctx context.Context) error {
	resume, err := b.restore(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap restore: %w", err)
	}
	if err := b.replay(ctx, resume); err != nil {
		return fmt.Errorf("bootstrap replay: %w", err)
	}
	return nil
}

// restore loads every durable record into the store and returns the per
// (topic, partition) resume offsets derived from the snapshots' LogPositions.
func (b *Bootstrap) restore(ctx context.Context) (map[resumeKey]int64, error) {
	records, err := b.sink.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	resume := make(map[resumeKey]int64)
	restored, foreign := 0, 0
	for _, rec := range records {
		// A FOREIGN RECORD IS SKIPPED; ANY OTHER REFUSAL ABORTS BOOT (#110).
		//
		// LoadAll returns every portfolio in the tenant's database, which on a
		// SHARDED replica is mostly other replicas' portfolios. Restoring those
		// used to be silent and was the origin of the corruption chain (see
		// state.WithShardOwnership): the copy freezes, because ShardFilter drops
		// its live events, and the Snapshotter then writes the frozen copy back
		// over the owner's fresher record. So ErrNotOwned here is the gate doing
		// its job, not a failure — skip it, count it, and do NOT take its resume
		// position, because this replica will not be replaying its partition on
		// its behalf.
		//
		// Every other error still aborts: booting past one would leave memory
		// missing a portfolio this replica DOES own and must answer for, and it
		// would answer from nothing.
		if err := b.store.Restore(rec.ToPortfolio(), rec.AppliedKeys); err != nil {
			if errors.Is(err, state.ErrNotOwned) {
				foreign++
				continue
			}
			return nil, fmt.Errorf("restore %q: %w", rec.ID, err)
		}
		restored++
		if lp := rec.LogPosition; lp != nil && lp.Topic != "" {
			k := resumeKey{topic: lp.Topic, partition: int(lp.Partition)}
			start := int64(lp.Offset) + 1 // resume at offset+1 (log_position semantics)
			if cur, ok := resume[k]; !ok || start < cur {
				resume[k] = start
			}
		}
	}
	b.foreign = foreign
	// The two counts are reported separately and always, because "this replica
	// restored 4 of 12 portfolios" is the only boot-time evidence that the ring
	// is actually splitting the book. One number cannot say it.
	b.logger.Info("bootstrap restore complete",
		"portfolios", restored, "not_owned_skipped", foreign, "resume_partitions", len(resume))
	return resume, nil
}

// ForeignRecordsSkipped reports how many durable records the last Run declined
// to restore because the shard ring assigns them to another replica. Zero on an
// unsharded replica, always — it owns everything, so nothing is foreign.
func (b *Bootstrap) ForeignRecordsSkipped() int { return b.foreign }

// replay drains each resume partition from the durable log up to the bootstrap
// start time and re-applies events through the Ingestor (idempotent via the
// seeded dedup windows).
func (b *Bootstrap) replay(ctx context.Context, resume map[resumeKey]int64) error {
	if len(b.brokers) == 0 {
		b.logger.Warn("no kafka brokers configured — skipping replay, relying on live spine")
		return nil
	}
	if len(resume) == 0 {
		b.logger.Warn("no durable resume positions — skipping replay, relying on live spine")
		return nil
	}
	ingestor, err := ingest.NewIngestor(b.applier)
	if err != nil {
		return err
	}
	end := b.now()
	for k, start := range resume {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.replayPartition(ctx, ingestor, k, start, end); err != nil {
			return err
		}
	}
	b.logger.Info("bootstrap replay complete")
	return nil
}

func (b *Bootstrap) replayPartition(ctx context.Context, ingestor *ingest.Ingestor, k resumeKey, start int64, end time.Time) error {
	src, err := b.newSource(replay.Config{
		Brokers:    b.brokers,
		Topic:      k.topic,
		Partitions: []int{k.partition},
		Range:      replay.Range{StartOffset: &start, EndTime: &end},
	})
	if err != nil {
		return fmt.Errorf("open source %s/%d: %w", k.topic, k.partition, err)
	}
	defer func() { _ = src.Close() }()

	var applied, skipped int
	for {
		ev, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var mfe *replay.MalformedFrameError
			if errors.As(err, &mfe) {
				skipped++
				b.logger.Warn("skip malformed frame in replay",
					"topic", mfe.Topic, "partition", mfe.Partition, "offset", mfe.Offset, "err", mfe.Err)
				continue
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return fmt.Errorf("replay %s/%d: %w", k.topic, k.partition, err)
		}
		if ev.Envelope == nil {
			continue
		}
		if herr := ingestor.Handler(ctx, ev.Envelope, ev.Payload); herr != nil {
			skipped++
			b.logger.Warn("skip event in replay",
				"topic", ev.Topic, "partition", ev.Partition, "offset", ev.Offset, "err", herr)
			continue
		}
		applied++
	}
	b.logger.Info("replayed partition", "topic", k.topic, "partition", k.partition,
		"start_offset", start, "applied", applied, "skipped", skipped)
	return nil
}
