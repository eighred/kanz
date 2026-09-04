// Package tape folds the published 1-minute candle series into the realised
// volume a finished decision's participation is measured against (#1007).
//
// # Why the OMS holds this at all
//
// EXECUTION_ALGO_POV enforces its participation cap against the volume profile's
// FORECAST, at admission, once. The realised half — what fraction of the tape the
// children turned out to be — needs the volume that ACTUALLY printed in each
// slice's own interval, and the OMS is the only process that also holds the
// decision, its schedule and its children's fills. Everything else in the estate
// that could see the tape cannot see the order.
//
// # market.crypto.bar IS THE ONLY REALISED-VOLUME RECORD, and that was checked
//
// The obvious cheaper option is the trade tape the OMS already subscribes to.
// It does not work, and the reason is worth writing down so nobody re-derives
// it: the venue adapters' market.crypto.trade prints come from a REST ticker
// poll and carry a PRICE AND NO SIZE (internal/execution/marktick.go), and the
// full-rate tape that does carry sizes is consumed in-process by market-ingest
// and never published — infra/nats/tenancy.yaml grants market-ingest publish on
// the candle, the coverage record, the profile and the quote, and not on the
// trade. So a volume fold over market.*.trade would fold every print at size
// zero and answer "nothing traded" for a live market, which is the one answer a
// participation denominator must never invent.
//
// # AN ABSENT CANDLE IS UNKNOWN, NEVER A ZERO
//
// internal/marketedge/bars emits no bar for a minute in which nothing traded and
// says why it cannot do better: "this process cannot tell 'nothing traded' from
// 'the feed was down' or 'we were rolling pods'". This inherits that limit
// exactly. Volume answers known=false for any window this fold cannot vouch for,
// and the one thing it will never do is return zero and true — a zero
// denominator is not a very large participation rate, it is no rate, and
// fabricating one would be loudest in precisely the thin market a cap exists for.
//
// # WHAT IT CAN VOUCH FOR, STATED NARROWLY
//
// "This series' earliest retained candle opened at T, so a window starting
// before T is one this fold was not watching." That is the whole attestation. It
// does NOT cover a hole in the middle: an hour in which market-ingest was down
// publishes no candles, and from here that is indistinguishable from an hour in
// which nothing traded. The estate has a first-hand record that WOULD settle it
// — internal/marketedge/coverage, published as market.crypto.ingestion_coverage
// — and joining it here is deliberately not done in this change: it is a second
// subscription and a second fold, and the measurement is worth more landed with
// a stated bound than delayed for a complete one. The bound is why every rate
// this feeds is documented as a LOWER bound rather than as the rate.
package tape

import (
	"context"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketedge/bars"
)

// Subject is the candle series this folds. It is bars.Subject rather than a
// string repeated here, so a producer that ever moves cannot leave a consumer
// subscribed to a subject nothing publishes on — which is a fold that reports an
// unmeasured market with exactly the confidence of a correct one.
const Subject = bars.Subject

// DefaultRetention is how far back candles are kept.
//
// 25 HOURS, AND THE NUMBER COMES FROM WHAT IT HAS TO OUTLIVE. This is read once
// per decision, at the moment a worked parent goes terminal, over that parent's
// whole working window — so it must hold every minute of the longest window the
// deployment admits, plus the lag between the last child finishing and the
// driver retiring the parent. The MARKET stream retains 24h
// (infra/nats/bootstrap-job.yaml), so a cold pod cannot rebuild more than that
// however long this is: an hour of margin over the stream's own bound is the
// most this can usefully hold, and holding less would discard minutes a pod
// still has.
//
// The cost is bounded and small: one *big.Rat per (instrument, venue, minute),
// so 1,500 entries per series per day — tens of thousands for an estate trading
// tens of instruments, against the tens of megabytes of order state the same
// process already holds.
const DefaultRetention = 25 * time.Hour

// Series is one candle stream: one instrument on one venue.
//
// PER VENUE, because participation is a claim about a BOOK. Being 8% of Binance
// and 40% of OKX are different facts about the same order, and a merged
// denominator would report the flattering one — which is the direction a
// participation control must never fail in.
type Series struct {
	InstrumentID string
	Venue        string
}

// candle is one folded minute: the window it covers and what printed in it.
type candle struct {
	open, close time.Time
	volume      *big.Rat
}

// Fold is the realised-volume view, folded off the bus.
//
// Safe for concurrent use: the bus handler folds on the delivery goroutine while
// the attribution path reads on whichever goroutine retired a parent.
type Fold struct {
	mu sync.Mutex
	// series is keyed by (instrument, venue) and holds that series' retained
	// candles, keyed by the candle's own open time.
	//
	// THE KEY SPACE COMES OFF THE WIRE, so it is bounded by eviction rather than
	// by construction — the shape test/arch/long_lived_maps_are_evicted_test.go
	// exists for. A producer that published a typo'd instrument would otherwise
	// leave an entry behind for the life of the pod. pruneLocked drops candles
	// past the horizon and drops a series once its last candle goes with them.
	series map[Series]map[int64]candle

	// newest is the newest candle open time this fold has seen on ANY series,
	// and it is what the horizon is measured back from.
	//
	// NOT A CLOCK, DELIBERATELY. A wall clock would evict a whole estate's
	// candles during a market-data outage — exactly when the last observations
	// before the outage are the only evidence anybody has — and it would make
	// two pods with different clock skew hold different history. Measuring the
	// horizon from the data is internal/pit's rule and this is the same rule
	// applied to a flat series.
	newest time.Time

	retention time.Duration

	// folded, refused and evicted separate a quiet feed from one this build is
	// rejecting. A fold that dropped every message silently would answer "that
	// window is unobservable" with exactly the confidence of one that folded them
	// all and found a genuinely unwatched window.
	folded, refused, evicted atomic.Int64
}

// NewFold returns an empty fold retaining candles for retention.
//
// A ZERO retention IS DefaultRetention, NOT "FOREVER". Reading a missing option
// as unbounded is the "nothing configured looks like checked, and fine" failure
// this estate refuses everywhere, and here it would grow a map keyed off the
// wire for the life of the pod.
func NewFold(retention time.Duration) *Fold {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Fold{series: make(map[Series]map[int64]candle), retention: retention}
}

// Handle folds one published candle. It is a bus.EventHandler.
//
// EVERY RETURN IS NIL — every delivery is ACKED, the stance costwatch takes for
// the same reason. This observes; it owns nothing. A message this build cannot
// decode does not become decodable by being redelivered, and nacking it would
// stall the subscription behind one bad message while every candle behind it
// went unread — leaving a fold three messages into a replay answering "I was not
// watching" with the confidence of one that had folded the lot.
//
// THERE IS NO TENANT CHECK, and it is the ruling internal/volprofilefeed already
// made for the profile spine: a candle is universal market data, every tenant's
// BTC-USDT prints on the same book, and it names no portfolio, account or order.
// What it must not do is reach a tenant-scoped decision unlabelled, and it does
// not — the participation figure is attached to the tenant's own order, and the
// instrument and venue it is looked up by come off that order.
func (f *Fold) Handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var ev marketpb.MarketDataEvent
	if proto.Unmarshal(payload, &ev) != nil {
		f.refused.Add(1)
		return nil
	}
	bar := ev.GetBar()
	if bar == nil || ev.GetInstrumentId() == "" || ev.GetMic() == "" {
		f.refused.Add(1)
		return nil
	}
	// OUT-OF-DOMAIN EXPONENTS ARE REFUSED BEFORE ANY NUMBER IS READ (#95).
	// Decimal.exponent is an unvalidated wire field and dec.FromProto
	// materialises 10^abs(exponent), so a candle carrying {1, 2000000000} would
	// not produce a wrong denominator — it would never return, and this handler
	// would stop acking while the service still reported healthy.
	if _, in := dec.InDomainDeep(&ev); !in {
		f.refused.Add(1)
		return nil
	}
	open, cl := bar.GetOpenTime().AsTime().UTC(), bar.GetCloseTime().AsTime().UTC()
	if bar.GetOpenTime() == nil || bar.GetCloseTime() == nil || !cl.After(open) {
		f.refused.Add(1)
		return nil
	}
	vol := bar.GetVolume()
	if vol == nil {
		f.refused.Add(1)
		return nil
	}
	f.put(Series{InstrumentID: ev.GetInstrumentId(), Venue: ev.GetMic()},
		candle{open: open, close: cl, volume: dec.FromProto(vol)})
	return nil
}

// put records one candle and prunes what has aged out.
//
// LAST WRITE WINS AT AN OPEN TIME. internal/marketedge/bars refuses to restate a
// published candle, so a second message for one minute is a redelivery of the
// same candle rather than a correction — replacing it rebuilds the identical
// fold, which is what makes a replaying pod converge on what the pod it replaced
// held. ADDING them would double a denominator on every redelivery and report a
// participation half what it was, which is the flattering direction.
func (f *Fold) put(s Series, c candle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.open.After(f.newest) {
		f.newest = c.open
	}
	// A CANDLE THAT IS ALREADY PAST THE HORIZON IS NOT ADMITTED, rather than
	// admitted and swept on the next fold. A replay delivers oldest-first, so
	// without this the whole retained history would be inserted and then evicted
	// message by message — the same end state, reached by allocating every
	// candle the stream holds.
	if c.open.Before(f.horizonLocked()) {
		f.evicted.Add(1)
		return
	}
	byOpen := f.series[s]
	if byOpen == nil {
		byOpen = make(map[int64]candle)
		f.series[s] = byOpen
	}
	byOpen[c.open.Unix()] = c
	f.folded.Add(1)
	f.pruneLocked()
}

// horizonLocked is the oldest candle open time still retained. The caller holds
// the lock.
func (f *Fold) horizonLocked() time.Time { return f.newest.Add(-f.retention) }

// pruneLocked drops every candle past the horizon, and every series left empty.
//
// CALLED ON EVERY FOLD rather than from a sweep goroutine, the shape
// volprofilefeed.Registry.gcLocked and bus.DedupWindow.gc already use here: a
// sweep nobody runs is an evictor that does not evict, and this way the bound
// holds without a second lifecycle to wire, forget, or lose on a pod that never
// reaches its first tick.
//
// THE SERIES DELETION IS THE HALF THAT MATTERS FOR THE BOUND. Dropping candles
// alone would leave a permanent, empty entry per instrument a producer ever
// named — including one it named by typo — which is the key-space-off-the-wire
// leak #805 was filed for.
func (f *Fold) pruneLocked() {
	cut := f.horizonLocked()
	for s, byOpen := range f.series {
		for k, c := range byOpen {
			if c.open.Before(cut) {
				// SPELLED AS AN INDEXED DELETE ON THE FIELD, not on the ranged
				// local. It is the same operation either way at runtime and it is
				// NOT the same statement to a reader or to
				// test/arch/long_lived_maps_are_evicted_test.go, which attributes a
				// shrink syntactically: a delete through a local is credited to
				// nothing, so the population at each key would report as bounded by
				// nothing while this loop bounded it.
				delete(f.series[s], k)
				f.evicted.Add(1)
			}
		}
		if len(f.series[s]) == 0 {
			delete(f.series, s)
		}
	}
}

// Volume is the quantity that printed in [from, to) on one venue's book. It
// satisfies tca.RealisedVolume.
//
// # THE ANSWER COVERS AT LEAST THE WINDOW ASKED FOR, AND USUALLY MORE
//
// The series is 1-minute and a slice interval is whatever the operator's window
// divided by their slice count happens to be, so a candle that OVERLAPS the
// window is counted whole. The volume returned is therefore over a window at
// least as long as the one asked about, so the participation rate computed from
// it is no larger than the true one. That asymmetry is deliberate and it is the
// estate's standing doctrine for interval evidence — internal/marketedge/
// coverage credits a floor of observed time and internal/marketdata/store/
// gaps.go rules that a missing bucket proves a window cannot support a claim
// while a whole window proves nothing. An exceedance measured against this is
// real; a rate under a cap is not proof the cap held.
//
// # known=false IS EVERY WINDOW THIS FOLD CANNOT VOUCH FOR
//
//	no series           nothing has ever published a candle for this book here.
//	window before the   this fold's earliest retained candle opens after the
//	  earliest candle   window starts, so it was not watching for part of it —
//	                    a cold pod, or a window older than the retention.
//	no overlapping      the window is inside the retained range and no candle
//	  candle            covers it: either nothing traded or nothing was
//	                    publishing, and from here those are the same absence.
//
// None of them is a zero. The caller counts the outcome as coverage rather than
// as a rate, which is what keeps "no participation" and "no observation"
// different claims all the way to the FACT.
//
// # THE READ COST, MEASURED RATHER THAN ASSUMED
//
// This walks the series' whole retained map per call, under the lock the bar
// handler also takes. The worst case the OMS can actually produce is bounded by
// admission: refuseUndrivableSchedule caps slice_count at the working window
// divided by the driver tick, so a 24h parent at a 10s tick is 8,640 intervals
// against ~1,440 retained candles — roughly 12M map probes, order of 100ms, ONCE
// when that parent goes terminal. That is a brief backlog of candle deliveries
// and nothing else: this read is off the order path entirely, and the fold it
// stalls is idempotent at the candle's own open time.
//
// The obvious optimisation is to probe minute-aligned keys instead of scanning.
// It is deliberately not taken: it would silently miss any candle whose open time
// is not on a minute boundary — answering UNKNOWN for a series the fold had
// accepted — and trading a stated 100ms for a blindness nothing would report is
// the wrong side of this estate's own rule about absences.
func (f *Fold) Volume(instrumentID, venue string, from, to time.Time) (*big.Rat, bool) {
	if instrumentID == "" || venue == "" || !to.After(from) {
		return nil, false
	}
	from, to = from.UTC(), to.UTC()

	f.mu.Lock()
	defer f.mu.Unlock()
	byOpen := f.series[Series{InstrumentID: instrumentID, Venue: venue}]
	if len(byOpen) == 0 {
		return nil, false
	}

	total := new(big.Rat)
	covered := false
	earliest := time.Time{}
	for _, c := range byOpen {
		if earliest.IsZero() || c.open.Before(earliest) {
			earliest = c.open
		}
		// OVERLAP, NOT CONTAINMENT. A candle is counted when any part of its
		// minute falls inside the window: requiring containment would answer zero
		// for every interval shorter than a minute, and answering zero is the one
		// thing this must never do.
		if c.open.Before(to) && c.close.After(from) {
			total.Add(total, c.volume)
			covered = true
		}
	}
	// THE COVERAGE CHECK IS AFTER THE SUM AND BEFORE THE RETURN, because it
	// governs whether the sum may be believed at all. A window that starts before
	// this fold's earliest candle is a window it was not watching for part of —
	// summing what it does hold would report a real number for a partly unwatched
	// interval, and the missing part is silent.
	if earliest.IsZero() || from.Before(earliest) {
		return nil, false
	}
	if !covered {
		return nil, false
	}
	return total, true
}

// Stats reports candles folded, messages refused, candles evicted, and the
// series currently held.
//
// THE REFUSAL COUNT IS THE ONE THAT MATTERS. A fold that has received nothing
// and one that has rejected everything both answer "unobservable" to every
// question, and only this separates a quiet feed from a producer this build
// cannot read.
func (f *Fold) Stats() (folded, refused, evicted, series int64) {
	f.mu.Lock()
	n := int64(len(f.series))
	f.mu.Unlock()
	return f.folded.Load(), f.refused.Load(), f.evicted.Load(), n
}
