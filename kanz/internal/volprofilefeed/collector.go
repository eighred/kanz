package volprofilefeed

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/marketedge/trades"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
	"github.com/eighred/kanz/pkg/bus"
)

// THE PRODUCER HALF: the fold that runs where the sockets are, and the sweep
// that publishes it (#897).

// Publisher is the bus publish surface — satisfied by *bus.Producer, the same
// seam internal/marketedge/bars takes for candles.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Config configures a Collector.
type Config struct {
	// Publisher is where profiles are announced. Required.
	Publisher Publisher

	// Tenant is stamped on every profile.
	//
	// REQUIRED, because this publishes off a SWEEP rather than off an inbound
	// delivery: there is no envelope to inherit a tenant from, and bus.Validate
	// rejects an envelope without one. A Collector built without it would fold
	// perfectly and land nothing, which is the shape #634 records for the OMS
	// producer and #591 for the coverage record.
	Tenant string

	// MinSessions is how many completed sessions must be retained before a shape
	// may be asserted.
	//
	// REQUIRED, AND THERE IS NO DEFAULT — volprofile.Config's rule, carried out
	// rather than papered over. How much history a desk demands before it will
	// schedule against a curve is an execution-policy decision, and a number
	// invented here would either refuse a healthy profile or let a two-day sample
	// set a month's schedule.
	MinSessions int

	// Bucket and Horizon are volprofile.Config's, and zero means its documented
	// default rather than "unbounded".
	Bucket  time.Duration
	Horizon time.Duration

	Logger *slog.Logger
}

// Collector folds live trades into intraday profiles and publishes them.
//
// # It is the fold AND the announcement, like internal/marketedge/bars
//
// The alternative — a fold here and a publisher elsewhere — would put the shape
// on a seam with no reader, which is the state #897 exists to end. Keeping them
// together also keeps the horizon honest: the value published on the wire is the
// store's own, read from the store rather than restated beside it, so a consumer
// bounding its integration by that number is bounding it by the history that
// actually stands behind the curve.
type Collector struct {
	store *volprofile.Store
	cfg   Config
	pub   Publisher
	log   *slog.Logger

	// mu guards published, which records the version last put on the wire for
	// each series so an unchanged curve is not republished on every sweep.
	//
	// IT IS AN OPTIMISATION AND NOT A CORRECTNESS BOUNDARY. Losing it on a
	// restart republishes every series once, and the consumer's fold is
	// idempotent at the as-of, so the only cost is one message per series.
	//
	// The key space is the SERIES SET THIS PROCESS SUBSCRIBES TO — one entry per
	// feed the composition root configured, not one per message — which is the
	// same bound volprofile.Store.series is under.
	mu        sync.Mutex
	published map[Series]string

	// publishes, unchanged, refused and failed are what an operator reads to tell
	// a quiet edge from a broken one. A collector that folds perfectly and
	// publishes nothing looks exactly like one nobody configured.
	publishes, unchanged, refused, failed atomic.Int64
}

// NewCollector builds a Collector over its own fold.
//
// EVERY REFUSAL IS AT CONSTRUCTION. A collector with no publisher, no tenant or
// no session floor would fold correctly and announce nothing, or announce
// something the broker rejects — and both look from outside exactly like an
// instrument nobody trades.
func NewCollector(cfg Config) (*Collector, error) {
	if cfg.Publisher == nil {
		return nil, fmt.Errorf("%w: no publisher — the fold would be perfect and nothing would "+
			"leave this process, which reads downstream as a market nobody measures", ErrProfile)
	}
	if cfg.Tenant == "" {
		return nil, fmt.Errorf("%w: no tenant — this publishes off a sweep with no inbound "+
			"envelope to inherit one from, so every announcement would be rejected by "+
			"bus.Validate while the fold kept running", ErrProfile)
	}
	store, err := volprofile.New(volprofile.Config{
		Bucket:      cfg.Bucket,
		Horizon:     cfg.Horizon,
		MinSessions: cfg.MinSessions,
	})
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Collector{
		store:     store,
		cfg:       cfg,
		pub:       cfg.Publisher,
		log:       cfg.Logger,
		published: make(map[Series]string),
	}, nil
}

// Store is the fold behind this collector, for a caller that wants to read it
// in-process. The market-data edge has no such reader today; a test does.
func (c *Collector) Store() *volprofile.Store { return c.store }

// Observe folds one trade. Safe to call from every feed goroutine at once —
// volprofile.Store owns its own lock.
func (c *Collector) Observe(s Series, tr trades.Trade) {
	c.store.Observe(volprofile.Series{InstrumentID: s.InstrumentID, Venue: s.Venue}, tr)
}

// Sweep closes every elapsed session and announces the series whose shape has
// changed since the last sweep.
//
// # Advance FIRST, and that ordering is the whole point of the sweep existing
//
// volprofile.Observe discovers a session boundary only when a print from a LATER
// session arrives, so on a series whose feed has died — or on a quiet instrument
// over a weekend — the last session sits open forever and never enters the shape.
// A profile silently short the day a pod rolled through is a curve nobody can see
// is wrong. Advance is what closes it.
//
// # An UNCHANGED shape is not republished, and an unchanged shape is the norm
//
// A profile moves once per session per series. Publishing on every sweep would
// put the same curve on the bus hundreds of times a day under a new as-of each
// time — and because the version is derived from the content INCLUDING the as-of,
// each of those would be a distinct version, so a consumer's retained version list
// would fill with copies of one curve and evict the pins that are still in use.
//
// # EVERY SERIES IS ANNOUNCED, INCLUDING THE ONES WITH NO SHAPE
//
// A series that is ABSENT or TOO_FEW_SESSIONS is published with its counts and no
// curve. It costs one small message per series per change and it buys the
// distinction this estate refuses to lose: an operator can tell "nobody is folding
// this instrument" from "folded, and there is not enough of it yet", and a
// consumer can say which of the two refused an order.
func (c *Collector) Sweep(ctx context.Context, now time.Time, series []Series) {
	c.store.Advance(now)

	for _, s := range series {
		ans, err := c.store.Profile(volprofile.Series{InstrumentID: s.InstrumentID, Venue: s.Venue}, now)
		if err != nil {
			// ErrLookahead: the newest completed session ends after `now`, which
			// means a print carried an event time in the future. Counted rather
			// than published — announcing a curve built from a session that has
			// not happened is the look-ahead volprofile refuses at its own door.
			c.refused.Add(1)
			c.log.Warn("volume profile not published — the fold refused the read",
				"instrument", s.InstrumentID, "venue", s.Venue, "err", err)
			continue
		}
		pb, err := Encode(ans, c.store.Horizon())
		if err != nil {
			c.refused.Add(1)
			c.log.Error("VOLUME PROFILE DROPPED — it cannot be represented on the wire, and a "+
				"partial curve would read downstream as buckets nothing trades in",
				"instrument", s.InstrumentID, "venue", s.Venue, "err", err)
			continue
		}

		c.mu.Lock()
		same := c.published[s] == pb.GetVersion()
		c.mu.Unlock()
		if same {
			c.unchanged.Add(1)
			continue
		}

		if err := c.pub.Publish(ctx, bus.Event{
			Subject:       Subject,
			EventType:     Subject,
			EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion: 1,
			Domain:        domain,
			EventTime:     ans.AsOf,
			// PARTITIONED BY INSTRUMENT, as every other market.v1 event is, so a
			// consumer folding one instrument's series reads its versions in the
			// order they were published.
			PartitionKey: s.InstrumentID,
			TenantID:     c.cfg.Tenant,
			Payload:      pb,
		}); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.failed.Add(1)
			// NOT FATAL, AND NOT SILENT. A profile that does not land is a VWAP
			// order refused at admission on every pod — which reads to a desk as
			// "this platform cannot work a VWAP" rather than as a publish failure.
			c.log.Error("volume profile publish failed — until one lands, every volume-driven "+
				"order is refused under NO_VOLUME_PROFILE and the refusal names the market "+
				"rather than this",
				"instrument", s.InstrumentID, "venue", s.Venue, "version", pb.GetVersion(), "err", err)
			continue
		}

		c.mu.Lock()
		c.published[s] = pb.GetVersion()
		c.mu.Unlock()
		c.publishes.Add(1)
		c.log.Info("volume profile published",
			"instrument", s.InstrumentID, "venue", s.Venue, "version", pb.GetVersion(),
			"verdict", ans.Verdict.String(), "sessions", ans.Sessions,
			"unknown_buckets", ans.Window.Unknown())
	}
}

// Stats reports what this collector has done: profiles announced, sweeps that
// found nothing new, reads or encodes it refused, and publishes that failed.
func (c *Collector) Stats() (publishes, unchanged, refused, failed int64) {
	return c.publishes.Load(), c.unchanged.Load(), c.refused.Load(), c.failed.Load()
}

// Tee wraps a TradeSource so every print reaches the fold on its way to the tape.
//
// A TEE RATHER THAN A SECOND CONSUMER, for internal/marketedge/bars.Tee's reason:
// a TradeSource has exactly one reader, and two Recv loops on one feed would each
// see half the prints — and each would build a curve that looked plausible and was
// wrong.
func Tee(src trades.TradeSource, s Series, c *Collector) trades.TradeSource {
	return &tee{src: src, series: s, c: c}
}

type tee struct {
	src    trades.TradeSource
	series Series
	c      *Collector
}

func (t *tee) Recv(ctx context.Context) (trades.Trade, error) {
	tr, err := t.src.Recv(ctx)
	if err != nil {
		return tr, err
	}
	t.c.Observe(t.series, tr)
	return tr, nil
}

func tsOf(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}

func durOf(d time.Duration) *durationpb.Duration { return durationpb.New(d) }
