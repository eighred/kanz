// Package book is the in-memory L2 order book at the heart of the market-ingest
// edge. It is the off-bus hot path: the raw, full-rate depth feed from an
// exchange is folded here in memory and NEVER republished on the bus at full
// rate. The native-alpha engines (order-book imbalance, cross-venue arbitrage)
// read the live book directly; only bounded, periodic Snapshots leave the
// process (for durable replay, cross-node bootstrap, and audit).
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

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/market-ingest/internal/dec"
)

// ErrSequenceGap is returned by ApplyDelta when a delta does not chain onto the
// current book sequence. The caller must re-snapshot before folding further —
// applying out of order would silently corrupt the book.
var ErrSequenceGap = errors.New("book: sequence gap — re-snapshot required")

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
func (b *Book) ApplySnapshot(s *marketpb.OrderBookSnapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bids = levelsToMap(s.GetBids())
	b.asks = levelsToMap(s.GetAsks())
	b.lastSeq = s.GetLastUpdateSequence()
	b.eventTime = s.GetEventTime().AsTime()
	b.seeded = true
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
	et := b.eventTime
	if et.IsZero() {
		et = time.Now().UTC()
	}
	return &marketpb.OrderBookSnapshot{
		InstrumentId:       b.instrumentID,
		Symbol:             b.symbol,
		Mic:                b.mic,
		EventTime:          timestamppb.New(et.UTC()),
		LastUpdateSequence: b.lastSeq,
		Bids:               bids,
		Asks:               asks,
	}
}

// BestBid / BestAsk return the top-of-book price, or nil when that side is empty
// — the read seam the native-alpha engines build on.
func (b *Book) BestBid() *big.Rat { b.mu.RLock(); defer b.mu.RUnlock(); return best(b.bids, true) }
func (b *Book) BestAsk() *big.Rat { b.mu.RLock(); defer b.mu.RUnlock(); return best(b.asks, false) }

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
	out := make([]*marketpb.PriceLevel, len(all))
	for i, l := range all {
		out[i] = &marketpb.PriceLevel{Price: dec.ToProto(l.price), Size: dec.ToProto(l.size)}
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
