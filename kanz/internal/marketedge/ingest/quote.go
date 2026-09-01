package ingest

import (
	"context"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// SubjectMarketCryptoQuote is the subject this engine publishes the TOP OF THE
// BOOK on, as a canonical market.v1 Quote (#876).
//
// # Why this producer exists at all
//
// #866 decomposes a decision's implementation shortfall into spread + impact +
// timing. The spread leg is the half-width of the market that was QUOTED when a
// slice was released, and internal/marketdata/mark folds a width on its Quote
// arm ONLY — a trade print has no width, at any later time, for anybody. #875
// measured how often that mattered and established the answer from the source:
// always, on every instrument, structurally. Both venue adapters poll a
// last-price REST ticker and publish market.crypto.trade; market-data's feed
// loop is not started in the shipped manifest; datamaster emits
// Quote{BidPrice: p, AskPrice: p}, a mid dressed as a quote, which
// recordTouchLocked refuses correctly because a zero width would score every
// instrument as the cheapest thing the fund trades.
//
// So the platform had a headline execution-cost number and no attribution. What
// was missing was a PRODUCER, and this is it.
//
// # Why HERE, and not a bookTicker subscription on the two venue adapters
//
// The issue's own recommendation was a best-bid/offer websocket subscription in
// each of services/venue-{binance,okx}, published through the shared
// MarkTickPublisher. That was priced first and this is the cheaper and safer
// answer, for four reasons that are all properties of the estate as it stands:
//
//  1. THE DATA IS ALREADY IN THIS PROCESS, AT FULL RATE. market-ingest already
//     dials both venues' public L2 depth streams and folds them into
//     internal/marketedge/book, for exactly the instruments the desk trades
//     (MARKET_INGEST_INSTRUMENTS and the two adapters' symbol maps carry the
//     same instrument ids, BTC-USDT / ETH-USDT). The top of that book IS the
//     quoted market. Nothing needs to be fetched.
//
//  2. IT ADDS NO UNPROVEN WIRE ASSUMPTION. The venue route requires asserting
//     the field semantics of two payloads — Binance <symbol>@bookTicker, OKX
//     tickers — that cannot be verified from this repository: there are no
//     credentials here and probing a live exchange is #72's territory. This
//     repository has already paid for that class of guess (OKX silently refuses
//     a clOrdId outside 1-32 alphanumerics). The depth feeds under
//     internal/marketedge/depth are already implemented, already tested against
//     httptest fakes, and already load-bearing for the book pkg/alpha sizes
//     against, so this producer inherits their wire assumptions rather than
//     introducing new ones.
//
//  3. IT IS FRESHER. The adapters' ticker feeds are a 5s REST poll. This book is
//     folded off a websocket at full rate, so the width published here is the one
//     that was actually crossable, which is the whole point of measuring it
//     against a slice release.
//
//  4. IT KEEPS PUBLIC MARKET DATA OUT OF THE CREDENTIALED PROCESSES. This
//     service's own manifest states the reason it exists: "It holds NO
//     credentials — it reads public quotes — which is precisely why it did not
//     need the process isolation the venue adapters got." Adding two new
//     market-data websockets to the two processes that place orders inverts that
//     decision to obtain data one process already has.
//
// # The door this deliberately does NOT open
//
// market.book.snapshot already carries a two-sided book, and mark.Handle refuses
// it BY EVENT TYPE on purpose: market.v1.OrderBookSnapshot is wire-compatible
// with market.v1.MarketDataEvent, so a bids-only snapshot unmarshals cleanly
// into a "Trade" at the deepest resting bid and poisons the mark below mid.
// Teaching the fold to read that subject would reopen exactly that hole.
//
// This producer does not touch it. The touch is derived HERE, at the source,
// and published as a NEW, separately constructed market.v1.MarketDataEvent
// carrying a Quote — the shape mark.Handle has always accepted. No
// OrderBookSnapshot is ever published on a mark-bearing subject, so the hazard
// is not guarded against, it is absent: see TestAQuotePayloadIsNeverABookSnapshot.
//
// # One book read, two FACTs
//
// The bid and the ask come from ONE Book.Snapshot call, which takes the read
// lock once across both sides. Book.BestBid and Book.BestAsk take it twice and
// could therefore straddle a fold, returning a bid from one book state and an
// ask from another — a market that never existed, and precisely the blended-legs
// case recordTouchLocked's crossed-quote refusal exists to catch. Deriving both
// legs from the snapshot that is being published anyway also means the quote and
// the snapshot describe the same book state and cannot disagree.
const SubjectMarketCryptoQuote = "market.crypto.quote"

// publishQuote publishes the top of one book snapshot as a market.v1 Quote.
//
// UNKNOWN IS A THIRD VALUE HERE, AND IT FAILS CLOSED. A book with an empty side,
// a non-positive touch, or a crossed touch publishes NOTHING. It does not
// publish a zero width, and it does not publish a mid on both legs — that is the
// datamaster shape the fold already refuses, and reproducing it would be worse
// than publishing nothing, because a zero spread is a real and different claim:
// it says crossing was free. A missing quote degrades the decomposition to
// TOTAL_ONLY, which is the current behaviour of the whole estate and is safe.
//
// A CROSSED PAIR IS DROPPED AT THE PRODUCER as well as at the fold. That is not
// redundancy for its own sake: this is the only place that knows the pair came
// from one consistent book state, and a producer that emits a shape its own
// consumer must refuse is a producer that has decided the consumer's guard is
// the specification. recordTouchLocked's refusal stays exactly as strict.
//
// It returns nothing, for publishSnapshot's reason: this runs on a ticker with
// no caller that has a better answer to a dropped quote than the next tick.
// A persistently refused publish is not silent — market-ingest wraps its
// producer in bus.HealthPublisher, so /readyz goes 503 and says so.
func (e *Engine) publishQuote(ctx context.Context, snap *marketpb.OrderBookSnapshot) {
	bid, ask, ok := topOfBook(snap)
	if !ok {
		return
	}
	ev := &marketpb.MarketDataEvent{
		InstrumentId: snap.GetInstrumentId(),
		Symbol:       snap.GetSymbol(),
		Mic:          snap.GetMic(),
		// THE VENUE'S OWN BOOK TIME, not ours. mark.Source ages a width from the
		// event's timestamp because that is what is true about the quote rather
		// than about our plumbing, and Book.Snapshot carries the venue time of
		// the last update folded into it. Stamping e.now() here would make every
		// width look as fresh as our ticker regardless of when the venue last
		// spoke, which is the one thing the staleness bound exists to detect.
		EventTime:      snap.GetEventTime(),
		SourceSequence: snap.GetLastUpdateSequence(),
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
			BidPrice: bid.GetPrice(), BidSize: bid.GetSize(),
			AskPrice: ask.GetPrice(), AskSize: ask.GetSize(),
		}},
	}
	if err := e.pub.Publish(ctx, bus.Event{
		Subject:       SubjectMarketCryptoQuote,
		EventType:     SubjectMarketCryptoQuote,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        "market",
		EventTime:     e.now().UTC(),
		PartitionKey:  snap.GetInstrumentId(),
		// Stamped for publishSnapshot's reason: this publish rides a ticker, not
		// a bus delivery, so there is no inbound envelope to inherit a tenant
		// from and bus.Validate refuses an envelope without one.
		TenantID: e.tenant,
		Payload:  ev,
	}); err != nil {
		e.logger.Warn("top-of-book quote publish failed — the market's width is not reaching the price "+
			"spine, so every execution-cost attribution for this instrument degrades to TOTAL_ONLY",
			"instrument", snap.GetInstrumentId(), "subject", SubjectMarketCryptoQuote, "err", err)
	}
}

// topOfBook returns the best bid and best offer levels of a snapshot, and
// whether the pair is a market that can be quoted at all.
//
// ok == false is the UNKNOWN answer and it has three causes, none of which is a
// zero width:
//
//   - one side is empty. A book with bids and no asks is not a tight market, it
//     is a market whose offer we cannot see.
//   - either leg is non-positive. Book.Snapshot already drops levels that will
//     not convert (#94/#189), so this is defence in depth against a zero or
//     negative price reaching a cost calculation as if it were observed.
//   - ask <= bid. A crossed or locked pair is a book this process cannot have
//     seen in one consistent state, and a non-positive half-spread arrives in
//     the attribution as the fund being PAID to take liquidity.
//
// Comparison is exact and stays in common.v1.Decimal — dec.Cmp and
// dec.IsPositive are total on any in-range input and read no float.
func topOfBook(snap *marketpb.OrderBookSnapshot) (bid, ask *marketpb.PriceLevel, ok bool) {
	bids, asks := snap.GetBids(), snap.GetAsks()
	if len(bids) == 0 || len(asks) == 0 {
		return nil, nil, false
	}
	bid, ask = bids[0], asks[0]
	if !dec.IsPositive(bid.GetPrice()) || !dec.IsPositive(ask.GetPrice()) {
		return nil, nil, false
	}
	if dec.Cmp(ask.GetPrice(), bid.GetPrice()) <= 0 {
		return nil, nil, false
	}
	return bid, ask, true
}
