package coverage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// Subject is where completed attestations publish.
//
// market.crypto.* ALREADY EXISTS and flattens to the provisioned Kafka topic
// market.crypto, which market.crypto.trade and market.crypto.bar already use.
// Riding it means no new topic and no new archival decision — and a coverage
// record inherits that topic's "not archived" stance honestly, because unlike a
// fill it is re-observable: the next minute produces the next one, and a lost
// record reads as UNKNOWN, which is the safe direction.
const Subject = "market.crypto.ingestion_coverage"

const domain = "market"

// SweepInterval is how often elapsed buckets are closed out.
//
// Well under Resolution, and it is NOT optional the way the bar flush is a
// latency nicety. A series whose feed has DIED produces no further observation,
// so nothing but this sweep will ever close its buckets — and an attestation
// that is never emitted is indistinguishable from a platform that was never
// looking, which is the one conflation this package exists to end.
const SweepInterval = 5 * time.Second

// Publisher is the bus publish surface — satisfied by *bus.Producer, the same
// seam bars.Collector takes.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Config configures a Recorder.
type Config struct {
	// Publisher puts completed attestations on the bus. Required.
	Publisher Publisher
	// Tenant is stamped on every record. Coverage is shared reference data —
	// every fund's BTC-USDT is fed by the same subscription — so there is no fund
	// to ask, exactly as for book snapshots and candles.
	Tenant string
	// MaxSilence is the longest gap between liveness observations that still
	// counts as continuous coverage. REQUIRED, and there is no default: it is a
	// property of the attesting feed's heartbeat cadence, and a value invented
	// here would either credit a dead socket or refuse a healthy one.
	MaxSilence time.Duration

	Logger *slog.Logger
	Now    func() time.Time
}

// Recorder accumulates liveness observations from live feed subscriptions and
// publishes one attestation per (series, bucket).
//
// It is the concurrent face of the accounting in coverage.go: every subscription
// observes on its own goroutine while the sweep runs on another.
type Recorder struct {
	mu    sync.Mutex
	state map[Series]*state

	pub        Publisher
	tenant     string
	maxSilence time.Duration
	logger     *slog.Logger
	now        func() time.Time
}

// NewRecorder validates the configuration and builds the Recorder.
func NewRecorder(cfg Config) (*Recorder, error) {
	if cfg.Publisher == nil {
		return nil, errors.New("coverage: publisher is required")
	}
	if err := validateSilence(cfg.MaxSilence); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Recorder{
		state:      make(map[Series]*state),
		pub:        cfg.Publisher,
		tenant:     cfg.Tenant,
		maxSilence: cfg.MaxSilence,
		logger:     cfg.Logger,
		now:        cfg.Now,
	}, nil
}

// Observe binds one subscription to the record.
//
// REGISTERING IS NOT ATTESTING. A series registered here and never heard from
// produces no record at all — the absence is the honest answer, and it is exactly
// what a downstream reader must see as UNKNOWN rather than as "covered, and quiet".
//
// attestor names the subscription (e.g. "binance:trades:BTCUSDT") so a claim can
// be traced back to the thing that made it. Calling it twice for one series keeps
// the first attestor: two subscriptions folding into one series would produce a
// record neither of them can be held to.
func (r *Recorder) Observe(s Series, attestor string) *Observer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.state[s]; !ok {
		r.state[s] = &state{series: s, attestor: attestor}
	}
	return &Observer{rec: r, series: s}
}

// Observer is one subscription's report seam. It satisfies trades.Liveness.
type Observer struct {
	rec    *Recorder
	series Series
}

// Live records that the subscription round-tripped a frame at `at`.
//
// WHAT COUNTS AS A FRAME IS THE DESIGN DECISION. A heartbeat — a websocket
// ping/pong, a venue keepalive — is the evidence that matters, because it proves
// the socket is alive whether or not anything traded. Reporting only on TRADES
// would make coverage a function of trading activity, so a quiet minute would
// report as uncovered and the record would be re-deriving the very ambiguity it
// exists to resolve.
func (o *Observer) Live(at time.Time) {
	if o == nil {
		return
	}
	o.rec.observe(o.series, at.UTC(), true, nil)
}

// Down records that the subscription FAILED at `at` — a read error, a dial
// failure, a rejected subscribe.
//
// The bucket it lands in stops accruing coverage from here, and the outage is not
// credited when the feed comes back: the next observation resumes crediting from
// ITS OWN instant, so the hole between them belongs to nobody.
func (o *Observer) Down(at time.Time, err error) {
	if o == nil {
		return
	}
	o.rec.observe(o.series, at.UTC(), false, err)
}

// observe folds one liveness event and publishes whatever buckets it completed.
func (r *Recorder) observe(s Series, at time.Time, live bool, cause error) {
	r.mu.Lock()
	st, ok := r.state[s]
	if !ok {
		// A subscription that never registered. Recording it under an unnamed
		// attestor would put a claim on the bus that nothing can be traced to.
		r.mu.Unlock()
		r.logger.Error("COVERAGE OBSERVATION DROPPED — no subscription registered for this series, "+
			"so the interval will read as UNKNOWN rather than observed",
			"series", s.String(), "at", at)
		return
	}
	var done []Record
	emit := func(rec Record) { done = append(done, rec) }

	switch {
	case !st.open:
		// FIRST CONTACT. Coverage starts HERE, not at the top of the bucket: the
		// seconds before the first observation are seconds nobody was watching,
		// and crediting them would be the first lie in the record.
		st.open = true
		st.bucket = at.Truncate(Resolution)
		st.cursor = at
	default:
		// The traversed interval is credited only if the subscription was believed
		// live throughout AND the silence was within tolerance. A longer silence is
		// not evidence of an outage — it is the absence of evidence either way, and
		// this record only ever states what was proven.
		credit := st.live && at.Sub(st.last) <= r.maxSilence
		st.advance(at, credit, emit)
	}
	st.last = at
	st.live = live
	if !live {
		st.breaks++
	}
	r.mu.Unlock()

	if !live {
		r.logger.Warn("feed subscription broke — this interval's ingestion coverage is now short, "+
			"and the bars inside it can no longer be read as a complete window",
			"series", s.String(), "at", at, "err", cause)
	}
	r.publish(context.Background(), done)
}

// Flush closes out every bucket that has fully elapsed as of now.
//
// IT IS WHAT MAKES A DEAD FEED VISIBLE. Observations alone close a bucket only
// when the NEXT one arrives, so a subscription that stopped talking would leave
// its last bucket open forever and publish nothing — and nothing published is
// exactly what a platform that was never looking also produces.
//
// Nothing after the newest observation is credited: the in-progress bucket is not
// emitted at all, because an interval that has not elapsed cannot be attested,
// and its absence honestly reads as UNKNOWN until it can.
func (r *Recorder) Flush(ctx context.Context, now time.Time) {
	now = now.UTC()
	// The start of the bucket in progress. Everything before it has elapsed.
	limit := now.Truncate(Resolution)

	r.mu.Lock()
	var done []Record
	emit := func(rec Record) { done = append(done, rec) }
	for _, st := range r.state {
		if !st.open || !st.bucket.Before(limit) {
			continue
		}
		// credit=false: the sweep observed nothing. Only a real liveness event
		// credits time, so a bucket closed by the sweep carries exactly what was
		// proven before the feed went quiet.
		st.advance(limit, false, emit)
	}
	r.mu.Unlock()

	r.publish(ctx, done)
}

// Run sweeps until ctx is cancelled, then sweeps once more on the way out.
func (r *Recorder) Run(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// A LAST SWEEP. Without it every elapsed-but-unclosed bucket is lost on
			// a rollout, and a deploy would silently erase the attestation for the
			// minutes around it — which reads downstream as "we were not looking"
			// for intervals we demonstrably were.
			r.Flush(context.WithoutCancel(ctx), r.now())
			return
		case <-t.C:
			r.Flush(ctx, r.now())
		}
	}
}

// publish puts completed attestations on the bus.
func (r *Recorder) publish(ctx context.Context, done []Record) {
	for _, rec := range done {
		end := rec.BucketStart.Add(Resolution)
		ev := &marketpb.IngestionCoverage{
			InstrumentId: rec.Series.InstrumentID,
			Mic:          rec.Series.Venue,
			BucketStart:  timestamppb.New(rec.BucketStart),
			BucketEnd:    timestamppb.New(end),
			Observed:     durationpb.New(rec.Observed),
			Breaks:       rec.Breaks,
			Attestor:     rec.Attestor,
		}
		if err := r.pub.Publish(ctx, bus.Event{
			Subject:       Subject,
			EventType:     Subject,
			EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion: 1,
			Domain:        domain,
			// The attestation is complete only once the interval has elapsed, so
			// the bucket's CLOSE is when this fact became true.
			EventTime: end,
			// PARTITIONED BY INSTRUMENT, as every other market.v1 event is, so a
			// consumer folding one instrument reads its coverage in order.
			PartitionKey: rec.Series.InstrumentID,
			TenantID:     r.tenant,
			Payload:      ev,
		}); err != nil && ctx.Err() == nil {
			// NOT FATAL, AND NOT SILENT. A dropped attestation degrades to UNKNOWN,
			// which is the safe direction — but a reader cannot tell a lost record
			// from a feed that was never live, so the only place that distinction
			// survives is here.
			r.logger.Error("ingestion-coverage publish failed — this interval will read downstream "+
				"as UNKNOWN, and the bars inside it can no longer be distinguished from a dead feed",
				"series", rec.Series.String(), "bucket", rec.BucketStart,
				"observed", rec.Observed, "err", err)
		}
	}
}

// String reports how many subscriptions are registered and the silence tolerance
// in force — the two facts that decide whether anything is being attested at all.
func (r *Recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("coverage.Recorder{series:%d, maxSilence:%s}", len(r.state), r.maxSilence)
}
