// Package book is the in-memory L2 order book at the heart of the market-ingest
// edge. It is the off-bus hot path: the raw, full-rate depth feed from an
// exchange is folded here in memory and NEVER republished on the bus at full
// rate. Only bounded, periodic Snapshots leave the process, and the bound is
// their whole purpose — capping what the spine carries, not preserving the book.
//
// BOOK HISTORY IS NOT RETAINED (#1005), and that is a decision rather than an
// oversight. This doc used to say the snapshots left the process "for durable
// replay, cross-node bootstrap, and audit", and named the native-alpha engines
// as readers of the live book. None of the four was ever built, and the estate
// is arranged so that the first three cannot happen: the MARKET stream ages
// market.> out, the archiver's DefaultSubjects names no market subject, and
// lake-sink's topic list excludes market.book — all three deliberately, because
// L2 depth is the highest-volume stream here and the CURRENT book is
// re-fetchable from the venue (DATA-M1). The snapshot IS delivered — market-data
// and, when calibration is enabled, the risk-engine both subscribe the market.>
// wildcard — but nothing turns it into state that outlives the pod's retention
// window. book_history_is_not_retained_test.go binds this paragraph to the three
// artifacts that decide it, so the claim and the estate cannot drift apart.
//
// THE CONSEQUENCE, stated here so nobody discovers it during a best-execution
// review: this estate cannot say what the book looked like at a past instant.
// That is stronger than "unretained" — it is unreachable. Neither venue serves
// historical L2 depth on the public endpoints internal/marketedge/depth dials,
// and Snapshots leave on a per-second cadence, so "the book at 14:32:05.123"
// would stay unanswerable under this shape even with unlimited retention. The
// artifact that WOULD answer what a reviewer asks is a book capture taken on the
// execution path at fill time and attached to the fill — bounded by fill count
// rather than by tick rate, and landing in an already-archived domain. It is not
// this, and it is its own piece of work. Do not close the gap by subscribing
// this subject into a store nobody queries: this repository has already ruled
// that a consumer written to satisfy a guard moves no capability.
//
// The one thing that reads a Snapshot today is in-process and is not the
// subject at all: the ingest engine derives the top-of-book market.v1 Quote it
// publishes from the SAME Snapshot call that feeds the published FACT, so the
// two FACTs on one tick cannot disagree about where the touch was.
//
// The book folds OrderBookDeltas with strict sequence-chain checking: a delta
// whose prev_update_sequence does not chain onto the current book sequence is a
// gap, and the book refuses it rather than folding out of order — the ingester
// re-snapshots instead of guessing a missing update. Prices and sizes are exact
// (big.Rat); no float ever touches depth a pricing decision reads.
package book

import (
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

// ErrSequenceGap is returned by ApplyDelta when a delta does not chain onto the
// current book sequence. The caller must re-snapshot before folding further —
// applying out of order would silently corrupt the book.
//
// THIS IS DEFENCE IN DEPTH, NOT THE PRIMARY PATH, AND #190 SETTLED WHY THAT IS
// ENOUGH. Both real depth sources detect the gap themselves and re-anchor, so a
// gapped delta does not normally reach this function at all:
//
//   - depth/binance.go refetches REST depth and surfaces the result as a
//     SNAPSHOT rather than a delta (TestBinanceSource_SequenceGapReAnchors).
//   - depth/okx.go resubscribes, which makes OKX re-push a snapshot
//     (TestOKXSource_SequenceGapReAnchors).
//
// WHAT IS DELIBERATELY NOT DONE: across a re-anchor the book keeps serving its
// last-known levels through BestBid/BestAsk/Top, and pkg/alpha's MarketView has
// no way to ask whether it is mid-re-anchor. A strategy pricing off the touch in
// that window uses depth one REST round trip stale and cannot know it.
//
// That was weighed and left alone (#190, decision recorded 2026-08-08). The
// window is BOUNDED — a REST fetch, not "until someone notices" — and a
// staleness flag is not free: it touches MarketView, every engine that reads it,
// and needs a defined re-seed semantic. Adding a seam no strategy reads is its
// own cost, and no strategy reads one today.
//
// THE CONDITION THAT WOULD REOPEN IT: a strategy that actually wants to decline
// on staleness. The cheap answer then is Book.EventTime() surfaced as an AGE on
// MarketView — each strategy sets its own tolerance — rather than a Stale() bool,
// which would bake one tolerance into the seam for everybody. Do not re-raise
// this as a fail-open bug; it was traced to the sources and it is not one.
var ErrSequenceGap = errors.New("book: sequence gap — re-snapshot required")

// ErrNoVenueTime is returned when an update carries no event_time and the book
// has no earlier venue time to keep (#957).
//
// IT IS A SOURCE DEFECT, NOT A FEED CONDITION — unlike ErrSequenceGap, which a
// healthy venue produces routinely and which re-snapshotting repairs. A depth
// source that omits event_time will omit it on every message, so this refuses
// the same way every time until the source is fixed. The alternative was folding
// it and stamping the book with the epoch (the pre-#957 behaviour, invisible
// because a nil timestamp is not a zero time) or with time.Now() (the branch
// written for this case, which converts "we do not know when the venue said
// this" into "we know it now").
var ErrNoVenueTime = errors.New("book: update carries no venue event_time and the book has none to keep")

// Book is one instrument's L2 depth on one venue. Safe for concurrent use: the
// fold path and the snapshot path run on separate goroutines.
type Book struct {
	instrumentID string
	symbol       string
	mic          string

	mu        sync.RWMutex
	bids      map[string]*big.Rat // canonical price string -> aggregate size
	asks      map[string]*big.Rat
	lastSeq   uint64
	eventTime time.Time
	seeded    bool // a snapshot or first delta has been folded
}

// New returns an empty book for instrumentID/symbol on venue mic.
func New(instrumentID, symbol, mic string) *Book {
	return &Book{
		instrumentID: instrumentID, symbol: symbol, mic: mic,
		bids: map[string]*big.Rat{}, asks: map[string]*big.Rat{},
	}
}

// ApplySnapshot resets the book to a venue snapshot — the bootstrap and
// gap-recovery entry point. After it, deltas chain off snapshot.last_update_sequence.
//
// IT REFUSES A SNAPSHOT THAT CARRIES NO VENUE TIME (#957), and the check is on
// the PROTO rather than on the converted value. A nil google.protobuf.Timestamp
// reads back through AsTime() as time.Unix(0, 0) — the UNIX EPOCH, not a zero
// time.Time — so the guard that was written for this case (`if et.IsZero()` in
// Snapshot) could never fire, and a source that omitted event_time stamped every
// book 1970-01-01.
//
// WHAT THAT COST IS THE DIAGNOSIS, not the number. The epoch propagates to
// market.crypto.quote and internal/marketdata/mark refuses a 56-year-old
// observation, so the fold holds no width rather than a fabricated fresh one —
// that part is safe. What is not safe is that market-ingest folds perfectly,
// publishes perfectly and reports ready while every consumer silently discards
// its output, with no error anywhere naming the cause.
//
// REFUSING BEATS SUBSTITUTING A CLOCK. The dead branch substituted time.Now(),
// which converts "we do not know when the venue said this" into "we know it
// now" — the same quiet confidence the staleness bound exists to prevent, and it
// would have made the epoch case invisible in the other direction.
func (b *Book) ApplySnapshot(s *marketpb.OrderBookSnapshot) error {
	if s.GetEventTime() == nil {
		return ErrNoVenueTime
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bids = levelsToMap(s.GetBids())
	b.asks = levelsToMap(s.GetAsks())
	b.lastSeq = s.GetLastUpdateSequence()
	b.eventTime = s.GetEventTime().AsTime()
	b.seeded = true
	return nil
}

// ApplyDelta folds one incremental update. It returns ErrSequenceGap if the
// delta does not chain onto the current sequence (venue-sequenced feeds only);
// an unsequenced feed (all-zero sequences, e.g. the sim) skips the check.
func (b *Book) ApplyDelta(d *marketpb.OrderBookDelta) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.seeded && b.lastSeq != 0 && d.GetPrevUpdateSequence() != 0 &&
		d.GetPrevUpdateSequence() != b.lastSeq {
		return ErrSequenceGap
	}
	// A DELTA MAY OMIT event_time ONLY ONCE THE BOOK IS SEEDED (#957). On a
	// seeded book the previous venue time still describes the state being
	// amended, which is why this path has always tolerated nil. On an UNSEEDED
	// book there is no previous time to keep, so folding it would leave
	// eventTime at Go's zero value and Snapshot would substitute a clock — the
	// same "we know it now" the snapshot path refuses.
	if !b.seeded && d.GetEventTime() == nil {
		return ErrNoVenueTime
	}
	applyLevels(b.bids, d.GetBids())
	applyLevels(b.asks, d.GetAsks())
	if s := d.GetLastUpdateSequence(); s != 0 {
		b.lastSeq = s
	}
	if t := d.GetEventTime(); t != nil {
		b.eventTime = t.AsTime()
	}
	b.seeded = true
	return nil
}

// Snapshot returns a bounded top-`depth` view (depth <= 0 ⇒ full book). Bids are
// ordered by strictly decreasing price, asks by strictly increasing price.
func (b *Book) Snapshot(depth int) *marketpb.OrderBookSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	bids := sortedLevels(b.bids, true, depth)
	asks := sortedLevels(b.asks, false, depth)
	// NO CLOCK SUBSTITUTION (#957). Both fold paths now refuse an update with no
	// venue time, so a SEEDED book always carries one and there is nothing to
	// substitute for. An unseeded book has no levels either, and the snapshot
	// loop already declines to publish an empty one.
	//
	// The branch this replaces read `if et.IsZero() { et = time.Now().UTC() }`
	// and could not fire for the case it was written for: a nil timestamp
	// arrives as the epoch, not as a zero time.
	return &marketpb.OrderBookSnapshot{
		InstrumentId:       b.instrumentID,
		Symbol:             b.symbol,
		Mic:                b.mic,
		EventTime:          timestamppb.New(b.eventTime.UTC()),
		LastUpdateSequence: b.lastSeq,
		Bids:               bids,
		Asks:               asks,
	}
}

// BestBid / BestAsk return the top-of-book price, or nil when that side is empty
// — the read seam the native-alpha engines build on.
func (b *Book) BestBid() *big.Rat { b.mu.RLock(); defer b.mu.RUnlock(); return best(b.bids, true) }
func (b *Book) BestAsk() *big.Rat { b.mu.RLock(); defer b.mu.RUnlock(); return best(b.asks, false) }

// Level is one aggregated L2 level — the exact, proto-free read type the alpha
// read-seam is built from. The engines run on the hot path, so they read the book
// directly as rationals rather than paying a proto marshal per tick.
type Level struct {
	Price *big.Rat
	Size  *big.Rat
}

// Top returns the best n levels per side (n <= 0 ⇒ the full book): bids by
// decreasing price, asks by increasing price. The returned rationals are copies,
// so a caller can hold or mutate them while the fold goroutine keeps writing.
func (b *Book) Top(n int) (bids, asks []Level) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return topLevels(b.bids, true, n), topLevels(b.asks, false, n)
}

// MIC reports the venue this book is of.
func (b *Book) MIC() string { return b.mic }

// --- helpers ---

func levelsToMap(levels []*marketpb.PriceLevel) map[string]*big.Rat {
	m := make(map[string]*big.Rat, len(levels))
	for _, l := range levels {
		size := dec.FromProto(l.GetSize())
		if size.Sign() <= 0 {
			continue
		}
		m[dec.FromProto(l.GetPrice()).RatString()] = size
	}
	return m
}

func applyLevels(side map[string]*big.Rat, levels []*marketpb.PriceLevel) {
	for _, l := range levels {
		key := dec.FromProto(l.GetPrice()).RatString()
		size := dec.FromProto(l.GetSize())
		if size.Sign() <= 0 {
			delete(side, key) // size 0 removes the level
			continue
		}
		side[key] = size
	}
}

func sortedLevels(side map[string]*big.Rat, descending bool, depth int) []*marketpb.PriceLevel {
	type lvl struct {
		price *big.Rat
		size  *big.Rat
	}
	all := make([]lvl, 0, len(side))
	for k, size := range side {
		p, _ := new(big.Rat).SetString(k)
		all = append(all, lvl{price: p, size: size})
	}
	sort.Slice(all, func(i, j int) bool {
		if descending {
			return all[i].price.Cmp(all[j].price) > 0
		}
		return all[i].price.Cmp(all[j].price) < 0
	})
	if depth > 0 && len(all) > depth {
		all = all[:depth]
	}
	// Same rule as the venue ingress that feeds this book (#94/#189): scaled, and
	// a level that still will not convert is dropped rather than carried wrong.
	//
	// This path is narrower than that one — it builds the published
	// OrderBookSnapshot FACT, not the exact read seam pkg/alpha sizes against — so
	// a wrapped level here misinforms whoever consumes the snapshot rather than
	// pricing an order. It takes the same action anyway: two conversions of the
	// same book that disagreed about which levels exist would be worse than either
	// rule alone, because the published book would stop matching the traded one.
	out := make([]*marketpb.PriceLevel, 0, len(all))
	for _, l := range all {
		price, okP := dec.ToProtoScaled(l.price)
		size, okS := dec.ToProtoScaled(l.size)
		if !okP || !okS {
			continue
		}
		out = append(out, &marketpb.PriceLevel{Price: price, Size: size})
	}
	return out
}

// topLevels sorts one side and truncates to n. Callers hold the read lock.
func topLevels(side map[string]*big.Rat, descending bool, n int) []Level {
	out := make([]Level, 0, len(side))
	for k, size := range side {
		p, ok := new(big.Rat).SetString(k)
		if !ok {
			continue
		}
		out = append(out, Level{Price: p, Size: new(big.Rat).Set(size)})
	}
	sort.Slice(out, func(i, j int) bool {
		if descending {
			return out[i].Price.Cmp(out[j].Price) > 0
		}
		return out[i].Price.Cmp(out[j].Price) < 0
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func best(side map[string]*big.Rat, descending bool) *big.Rat {
	var top *big.Rat
	for k := range side {
		p, _ := new(big.Rat).SetString(k)
		if top == nil || (descending && p.Cmp(top) > 0) || (!descending && p.Cmp(top) < 0) {
			top = p
		}
	}
	return top
}

// InstrumentID reports the book's instrument.
func (b *Book) InstrumentID() string { return b.instrumentID }
