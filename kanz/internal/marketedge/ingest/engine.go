// Package ingest wires a depth source to the in-memory book and publishes
// bounded, periodic OrderBookSnapshots on the bus. This is the market-ingest
// composition of the off-bus hot path: the fold loop consumes the full-rate
// feed into memory (never republished at full rate), and a separate,
// time-driven snapshot loop emits a depth-capped view for durable replay and
// audit. The native-alpha engines (order-book imbalance, cross-venue arbitrage)
// will read the same in-memory book and emit signal.v1.StrategySignals with
// source = NATIVE_ENGINE; this milestone lays the book + snapshot foundation
// they build on.
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/marketedge/book"
	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/pkg/bus"
)

// SubjectBookSnapshot is the subject bounded L2 snapshots publish on. Partition
// key is instrument_id, so per-instrument ordering holds.
const SubjectBookSnapshot = "market.book.snapshot"

// Publisher is the bus publish surface — satisfied by *bus.Producer.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Engine folds one instrument's depth feed and publishes its snapshots.
type Engine struct {
	book             *book.Book
	source           depth.DepthSource
	pub              Publisher
	logger           *slog.Logger
	snapshotInterval time.Duration
	snapshotDepth    int
	now              func() time.Time
	tenant           string
}

// Config configures an Engine.
type Config struct {
	Book             *book.Book
	Source           depth.DepthSource
	Publisher        Publisher
	Logger           *slog.Logger
	SnapshotInterval time.Duration // <=0 ⇒ 1s
	SnapshotDepth    int           // <=0 ⇒ 20
	Now              func() time.Time
	// Tenant is stamped on every book snapshot this engine publishes.
	//
	// SET IT. Snapshots are published off a ticker, outside any bus delivery, so
	// there is no inbound envelope to inherit a tenant from. Empty is tolerated —
	// the publish then falls through to the producer's own ProducerConfig.Tenant,
	// which is what this engine relied on before the field existed — but that
	// makes this library correct only under callers who set that fallback. A
	// caller that leaves ProducerConfig.Tenant empty and supplies tenants
	// per-event (the multi-tenant arrangement pkg/alpha documents) will have
	// EVERY snapshot rejected as "tenant_id required" unless it sets this.
	//
	// Book snapshots are shared reference data — every fund sees the same
	// BTC-USD book — so this is normally the platform's own tenant, not a
	// customer's. That is a different question from the fund-scoped signal path,
	// which resolves its tenant per event via alpha.Config.Authority.
	Tenant string
}

// New builds an Engine.
func New(cfg Config) *Engine {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SnapshotInterval <= 0 {
		cfg.SnapshotInterval = time.Second
	}
	if cfg.SnapshotDepth <= 0 {
		cfg.SnapshotDepth = 20
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Engine{
		book: cfg.Book, source: cfg.Source, pub: cfg.Publisher, logger: cfg.Logger,
		snapshotInterval: cfg.SnapshotInterval, snapshotDepth: cfg.SnapshotDepth, now: cfg.Now,
		tenant: cfg.Tenant,
	}
}

// Run drives the fold loop and the snapshot loop until ctx is cancelled. It
// returns the first fatal error (a source error that is not ctx cancellation).
func (e *Engine) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- e.foldLoop(ctx) }()
	go e.snapshotLoop(ctx)
	return <-errc
}

// foldLoop consumes the depth feed into the in-memory book. A snapshot resets
// the book; a delta folds incrementally. A sequence gap discards the delta and
// waits for the source's next snapshot — the ingester never folds out of order.
func (e *Engine) foldLoop(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		u, err := e.source.Recv(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		switch {
		case u.Snapshot != nil:
			e.book.ApplySnapshot(u.Snapshot)
		case u.Delta != nil:
			if err := e.book.ApplyDelta(u.Delta); errors.Is(err, book.ErrSequenceGap) {
				e.logger.Warn("depth sequence gap — awaiting re-snapshot",
					"instrument", e.book.InstrumentID(), "prev_seq", u.Delta.GetPrevUpdateSequence())
			}
		}
	}
}

// snapshotLoop publishes a bounded book snapshot every interval.
func (e *Engine) snapshotLoop(ctx context.Context) {
	t := time.NewTicker(e.snapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.publishSnapshot(ctx)
		}
	}
}

func (e *Engine) publishSnapshot(ctx context.Context) {
	snap := e.book.Snapshot(e.snapshotDepth)
	if len(snap.GetBids()) == 0 && len(snap.GetAsks()) == 0 {
		return // nothing folded yet — don't emit an empty book
	}
	if err := e.pub.Publish(ctx, bus.Event{
		Subject:       SubjectBookSnapshot,
		EventType:     SubjectBookSnapshot,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        "market",
		EventTime:     e.now().UTC(),
		PartitionKey:  snap.GetInstrumentId(),
		// Stamped explicitly, because nothing else on this path can supply it.
		// This publish runs off a ticker, not a bus delivery, so there is no
		// inbound envelope for bus.Consumer to have stashed a tenant from —
		// ctx carries none. Until this field existed the tenant came solely
		// from ProducerConfig.Tenant, which made a library's correctness depend
		// on how each of its callers happened to wire its producer.
		TenantID: e.tenant,
		Payload:  snap,
	}); err != nil {
		e.logger.Warn("book snapshot publish failed", "instrument", snap.GetInstrumentId(), "err", err)
	}
}
