// THE TOP-OF-BOOK QUOTE, AND THE FOLD IT HAS TO REACH (#876).
//
// These tests are not about the engine publishing a message. They are about the
// message being the one thing that closes #876: a market.v1 Quote that
// internal/marketdata/mark folds into a LIVE WIDTH, so the spread leg of #866's
// shortfall decomposition stops coming back UNSET on every order the platform
// has ever worked.
//
// So the round-trip is asserted against the REAL fold rather than against a
// restatement of what it wants. A test that checked "we published a Quote with
// bid and ask set" would have passed for the datamaster shape too
// (Quote{BidPrice: p, AskPrice: p}), which mark refuses — correctly — and which
// is one of the four publishers #876 names as the reason the estate has no
// attribution.
package ingest

import (
	"context"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/marketedge/book"
	"github.com/eighred/kanz/internal/marketedge/depth"
	"github.com/eighred/kanz/pkg/bus"
)

// runOneBook folds ONE scripted snapshot through a real bus.Producer and returns
// everything that reached the transport.
//
// It waits for the tick to have happened rather than for a message to appear,
// because the interesting cases here PUBLISH NOTHING on the quote subject and a
// wait-for-a-message helper cannot tell that apart from being early.
func runOneBook(t *testing.T, bids, asks []*marketpb.PriceLevel) *captureClient {
	t.Helper()
	cc := newCaptureClient()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "market-edge", ProducerVersion: "test"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	src := &scriptedSource{updates: []depth.Update{
		{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: "BTC-USD", LastUpdateSequence: 7, Bids: bids, Asks: asks,
			// THE VENUE TIME IS NOT OPTIONAL IN A FIXTURE, because it is not
			// optional on the wire: depth/binance.go and depth/okx.go both stamp
			// one on every snapshot and every delta. Omitting it here would test
			// a book state neither real source produces — and would fail, loudly
			// and in the SAFE direction, for the reason
			// TestABookWithNoVenueTimeYieldsNoUsableWidth records.
			EventTime: tsOf(time.Now().UTC()),
		}},
	}}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: prod,
		SnapshotInterval: 10 * time.Millisecond, SnapshotDepth: 10, Tenant: "test-tenant",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	<-ctx.Done()
	<-done
	return cc
}

// framed returns the envelope and payload bytes of every captured message whose
// event type matches, in order.
func framed(t *testing.T, cc *captureClient, eventType string) []struct {
	env     *envelopepb.Envelope
	payload []byte
} {
	t.Helper()
	<-cc.mu
	defer func() { cc.mu <- struct{}{} }()
	var out []struct {
		env     *envelopepb.Envelope
		payload []byte
	}
	for _, m := range cc.sent {
		env, payload, err := bus.Unframe(m.Body)
		if err != nil {
			t.Fatalf("Unframe: %v", err)
		}
		if env.GetEventType() == eventType {
			out = append(out, struct {
				env     *envelopepb.Envelope
				payload []byte
			}{env, payload})
		}
	}
	return out
}

// THE ENVELOPE. The quote is published off a ticker with no inbound delivery, so
// every field the broker checks has to be stamped here — and publishQuote only
// LOGS a refusal, exactly as publishSnapshot does, so an illegal envelope would
// present as a healthy engine whose width nobody ever receives.
func TestATopOfBookQuoteIsAValidEnvelope(t *testing.T) {
	cc := runOneBook(t,
		[]*marketpb.PriceLevel{lvl("50000", "3"), lvl("49999", "9")},
		[]*marketpb.PriceLevel{lvl("50001", "2"), lvl("50002", "8")})

	got := framed(t, cc, SubjectMarketCryptoQuote)
	if len(got) == 0 {
		t.Fatal("no quote reached the transport. The width is the whole point of #876: without it " +
			"every execution attribution stays TOTAL_ONLY, which is indistinguishable from today")
	}
	env, payload := got[0].env, got[0].payload
	if err := bus.Validate(env); err != nil {
		t.Fatalf("the quote envelope fails Validate: %v", err)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT — an observed market is not a command", got)
	}
	if got := env.GetTenantId(); got != "test-tenant" {
		t.Errorf("tenant_id = %q, want test-tenant", got)
	}
	if got := env.GetDomain(); got != "market" {
		t.Errorf("domain = %q, want market", got)
	}
	// DERIVED by the producer from the payload type. It is the cheapest proof
	// that the payload is a MarketDataEvent and NOT the OrderBookSnapshot the
	// quote was derived from.
	if got := env.GetPayloadSchemaRef(); got != "market.v1.MarketDataEvent:1" {
		t.Errorf("payload_schema_ref = %q, want market.v1.MarketDataEvent:1", got)
	}

	var ev marketpb.MarketDataEvent
	if err := proto.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	q := ev.GetQuote()
	if q == nil {
		t.Fatal("the payload carries no Quote — mark.Handle folds a width on the Quote arm ONLY, " +
			"so anything else here leaves the spread leg UNSET")
	}
	if got := q.GetBidPrice().GetCoefficient(); got == 0 {
		t.Error("bid_price is zero")
	}
	if bid, ask := ratOfDec(t, q.GetBidPrice()), ratOfDec(t, q.GetAskPrice()); bid.Cmp(big.NewRat(50000, 1)) != 0 || ask.Cmp(big.NewRat(50001, 1)) != 0 {
		t.Errorf("touch = %s/%s, want 50000/50001 — the BEST bid and BEST offer, not a deeper level",
			bid.RatString(), ask.RatString())
	}
	// The sizes ride along because they are real and free: they came from the
	// same book read as the prices.
	if s := ratOfDec(t, q.GetBidSize()); s.Cmp(big.NewRat(3, 1)) != 0 {
		t.Errorf("bid_size = %s, want 3", s.RatString())
	}
	if s := ratOfDec(t, q.GetAskSize()); s.Cmp(big.NewRat(2, 1)) != 0 {
		t.Errorf("ask_size = %s, want 2", s.RatString())
	}
	if got := string(cc.sent[0].Key); got != "BTC-USD" {
		t.Errorf("partition key = %q, want the instrument id", got)
	}
}

// THE ONE THAT CLOSES #876: the published bytes, through the REAL fold, become a
// width the OMS's arrival stamp can read.
//
// mark.Source is what services/oms wires as its ArrivalMarks, and Touch is the
// accessor services/oms/internal/order/arrival.go calls. Asserting against it
// rather than against the payload is what makes this a proof that the spread leg
// becomes measurable, and not merely a proof that a Quote was serialised.
func TestAPublishedQuoteBecomesALiveWidthInTheMarkFold(t *testing.T) {
	cc := runOneBook(t,
		[]*marketpb.PriceLevel{lvl("50000", "3")},
		[]*marketpb.PriceLevel{lvl("50001", "2")})
	got := framed(t, cc, SubjectMarketCryptoQuote)
	if len(got) == 0 {
		t.Fatal("nothing was published on the quote subject")
	}

	src := mark.New(time.Now, 30*time.Second)
	if err := src.Handle(context.Background(), got[0].env, got[0].payload); err != nil {
		t.Fatalf("mark.Handle: %v", err)
	}

	bid, ask, asOf, ok := src.Touch("BTC-USD")
	if !ok {
		t.Fatal("the fold holds NO usable width for an instrument this producer just quoted. " +
			"That is the #876 state exactly: Touch answers false, arrival.go stamps no bid/ask, " +
			"and every attribution for this instrument comes back quality = TOTAL_ONLY")
	}
	if bid.Cmp(big.NewRat(50000, 1)) != 0 || ask.Cmp(big.NewRat(50001, 1)) != 0 {
		t.Errorf("Touch = %s/%s, want 50000/50001", bid.RatString(), ask.RatString())
	}
	if asOf.IsZero() {
		t.Error("the width carries no observation time, so nothing downstream can discount it for staleness")
	}
	// The mid rides the same fold, so the mark and the width describe one market.
	if m := src.Mark("BTC-USD"); m == nil || m.Cmp(big.NewRat(100001, 2)) != 0 {
		t.Errorf("Mark = %v, want the 50000.5 mid of the same quote", m)
	}
	if held, live := src.TouchStats(); held != 1 || live != 1 {
		t.Errorf("TouchStats = (%d held, %d live), want (1, 1) — this is the gauge #875 added and "+
			"the one OMSQuoteCoverageAbsent reads", held, live)
	}
}

// A ONE-SIDED BOOK IS UNKNOWN, NOT ZERO-WIDTH.
//
// A book with bids and no asks is not a tight market; it is a market whose offer
// this process cannot see. Publishing anything for it — a zero width, or the bid
// on both legs — would be the datamaster shape, and it would score the
// instrument as the cheapest thing the fund trades. The snapshot still
// publishes, because a one-sided book is a real book.
func TestAOneSidedBookPublishesNoQuote(t *testing.T) {
	cc := runOneBook(t, []*marketpb.PriceLevel{lvl("50000", "3")}, nil)

	if n := len(framed(t, cc, SubjectMarketCryptoQuote)); n != 0 {
		t.Fatalf("%d quote(s) published from a book with no offer — an unobservable width must be "+
			"published as NOTHING, never as a zero or a mid", n)
	}
	if n := len(framed(t, cc, SubjectBookSnapshot)); n == 0 {
		t.Error("the book snapshot stopped publishing too — a one-sided book is still a real book, " +
			"and the quote's refusal must not take the depth feed down with it")
	}
}

// A CROSSED BOOK IS REFUSED AT THE PRODUCER, not merely at the fold.
//
// ask <= bid is a book that cannot have been seen in one consistent state, and a
// negative half-spread arrives in the attribution as the fund being PAID to take
// liquidity. recordTouchLocked refuses it; this asserts the producer never emits
// a shape its own consumer must throw away.
func TestACrossedBookPublishesNoQuote(t *testing.T) {
	cc := runOneBook(t,
		[]*marketpb.PriceLevel{lvl("50002", "3")},
		[]*marketpb.PriceLevel{lvl("50001", "2")})

	if n := len(framed(t, cc, SubjectMarketCryptoQuote)); n != 0 {
		t.Fatalf("%d quote(s) published from a crossed book", n)
	}
}

// A LOCKED BOOK (ask == bid) IS THE datamaster SHAPE, AND IT IS REFUSED TWICE.
//
// The producer never emits it, and — proven here from the other end — the fold
// would refuse it anyway. Both halves matter: the first is what this change
// added, the second is the guarantee that a FUTURE producer getting this wrong
// still cannot make a zero spread look like a measurement.
func TestALockedBookIsRefusedByBothTheProducerAndTheFold(t *testing.T) {
	cc := runOneBook(t,
		[]*marketpb.PriceLevel{lvl("50000", "3")},
		[]*marketpb.PriceLevel{lvl("50000", "2")})
	// Errorf, NOT Fatalf. The two halves of this test are two independent locks
	// on the same door, and a failure of the first must not skip the second: the
	// question "if a producer ever emits a zero width, does the fold still
	// refuse it" is exactly the one worth answering while the producer is broken.
	if n := len(framed(t, cc, SubjectMarketCryptoQuote)); n != 0 {
		t.Errorf("%d quote(s) published from a locked book — a zero width is a CLAIM that crossing "+
			"was free, and it is a different claim from an unobservable one", n)
	}

	// The other end: hand the fold the exact shape datamaster publishes, on this
	// producer's own subject, and prove it still holds no width.
	zero := &marketpb.MarketDataEvent{
		InstrumentId: "BTC-USD", Symbol: "BTCUSDT", Mic: "BINANCE",
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
			BidPrice: lvl("50000", "1").GetPrice(), AskPrice: lvl("50000", "1").GetPrice(),
		}},
	}
	payload, err := proto.Marshal(zero)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	src := mark.New(time.Now, 30*time.Second)
	env := &envelopepb.Envelope{EventType: SubjectMarketCryptoQuote}
	if err := src.Handle(context.Background(), env, payload); err != nil {
		t.Fatalf("mark.Handle: %v", err)
	}
	if _, _, _, ok := src.Touch("BTC-USD"); ok {
		t.Fatal("the fold accepted a zero-width quote. recordTouchLocked's ask <= bid refusal is what " +
			"stops every unquoted instrument scoring as the cheapest thing the fund trades")
	}
}

// THE DOOR THAT STAYS SHUT.
//
// market.v1.OrderBookSnapshot is WIRE-COMPATIBLE with market.v1.MarketDataEvent
// — instrument_id/symbol/mic/event_time/uint64 occupy fields 1-5 in both — so a
// bids-only snapshot (field 6, repeated PriceLevel) unmarshals cleanly as a
// Trade (field 6, Trade{price=1,size=2}) at the deepest resting bid. That is why
// mark.Handle refuses market.book.snapshot BY EVENT TYPE, and this producer does
// not ask it to stop.
//
// The invariant asserted here is the structural one that makes the hazard
// ABSENT rather than guarded against: nothing this engine publishes on a
// mark-bearing event type carries an OrderBookSnapshot. The quote is a
// separately constructed MarketDataEvent; the snapshot keeps its own,
// non-mark-bearing subject.
//
// FOR THE RECORD, THE REVERSE DIRECTION WAS CHECKED TOO and is not reachable: a
// Quote sits at field 7, which an OrderBookSnapshot reader would decode as
// `asks`, so a mis-typed reader would see a one-level book and no bids. No
// service in this repository unmarshals an OrderBookSnapshot off the bus at all
// (grep OrderBookSnapshot under services/ finds three comments and no decode),
// so there is no such reader to protect — but if one is ever added, it must
// dispatch on event type exactly as mark.Handle does.
func TestNoBookSnapshotIsEverPublishedOnAMarkBearingSubject(t *testing.T) {
	cc := runOneBook(t,
		[]*marketpb.PriceLevel{lvl("50000", "3")},
		[]*marketpb.PriceLevel{lvl("50001", "2")})

	<-cc.mu
	msgs := append([]bus.Message(nil), cc.sent...)
	cc.mu <- struct{}{}
	if len(msgs) < 2 {
		t.Fatalf("expected both a snapshot and a quote, got %d message(s)", len(msgs))
	}

	for _, m := range msgs {
		env, payload, err := bus.Unframe(m.Body)
		if err != nil {
			t.Fatalf("Unframe: %v", err)
		}
		markBearing := env.GetEventType() == SubjectMarketCryptoQuote
		var ev marketpb.MarketDataEvent
		isMarketData := proto.Unmarshal(payload, &ev) == nil && ev.GetQuote() != nil
		switch {
		case markBearing && !isMarketData:
			t.Errorf("event %q is mark-bearing but does not carry a MarketDataEvent Quote — this is "+
				"the shape that lets a book snapshot become a Trade at the deepest bid", env.GetEventType())
		case !markBearing && env.GetEventType() != SubjectBookSnapshot:
			t.Errorf("unexpected subject %q from this engine", env.GetEventType())
		}
	}
}

// THE WIDTH AGES FROM THE VENUE'S CLOCK, NOT OURS.
//
// mark.Source measures staleness from the event's own timestamp because that is
// what is true about the quote rather than about our plumbing. Stamping
// time.Now() in the payload would make every width look as fresh as this
// engine's ticker no matter how long ago the venue last spoke — which is
// precisely the condition OMS_PRICE_MAX_AGE exists to detect, made undetectable.
func TestTheQuoteCarriesTheVenueBookTimeNotOurs(t *testing.T) {
	venueTime := time.Now().UTC().Add(-90 * time.Second).Truncate(time.Second)
	cc := newCaptureClient()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "market-edge", ProducerVersion: "test"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	src := &scriptedSource{updates: []depth.Update{
		{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: "BTC-USD", LastUpdateSequence: 7,
			EventTime: tsOf(venueTime),
			Bids:      []*marketpb.PriceLevel{lvl("50000", "3")},
			Asks:      []*marketpb.PriceLevel{lvl("50001", "2")},
		}},
	}}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: prod,
		SnapshotInterval: 10 * time.Millisecond, SnapshotDepth: 10, Tenant: "test-tenant",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	<-ctx.Done()
	<-done

	got := framed(t, cc, SubjectMarketCryptoQuote)
	if len(got) == 0 {
		t.Fatal("nothing was published on the quote subject")
	}
	var ev marketpb.MarketDataEvent
	if err := proto.Unmarshal(got[0].payload, &ev); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := ev.GetEventTime().AsTime(); !got.Equal(venueTime) {
		t.Fatalf("payload event_time = %s, want the venue book time %s", got, venueTime)
	}

	// And the consequence, asserted through the fold rather than asserted about
	// it: a 90-second-old book is refused by a 30-second bound.
	s := mark.New(time.Now, 30*time.Second)
	if err := s.Handle(context.Background(), got[0].env, got[0].payload); err != nil {
		t.Fatalf("mark.Handle: %v", err)
	}
	if _, _, _, ok := s.Touch("BTC-USD"); ok {
		t.Fatal("a width from a book the venue last updated 90s ago is being served under a 30s " +
			"staleness bound — the bound is measuring our ticker, not the market")
	}
}

// A BOOK WITH NO VENUE TIME PRODUCES NO USABLE WIDTH, AND THAT IS THE SAFE
// DIRECTION.
//
// Neither shipped depth source can reach this state — depth/binance.go and
// depth/okx.go stamp EventTime on every snapshot and every delta — so this is
// recording a property rather than covering a live path. It is recorded because
// the behaviour is SURPRISING and the surprise is load-bearing: a nil
// google.protobuf.Timestamp reads back as the UNIX EPOCH, not as a zero
// time.Time, so book.Snapshot's `if et.IsZero() { et = time.Now() }` fallback
// does NOT fire for a source that omitted the field. The quote is then stamped
// 1970 and every staleness bound refuses it.
//
// Refusing is right: a width whose observation time is unknown must not be
// treated as fresh. But a future source that forgets EventTime would go dark
// with no error anywhere, so the failure mode belongs written down. The same
// epoch stamp also reaches the market.book.snapshot FACT, which is a separate
// (and today unreachable) defect in book.Snapshot rather than one here.
func TestABookWithNoVenueTimeYieldsNoUsableWidth(t *testing.T) {
	cc := newCaptureClient()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "market-edge", ProducerVersion: "test"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	src := &scriptedSource{updates: []depth.Update{
		{Snapshot: &marketpb.OrderBookSnapshot{
			InstrumentId: "BTC-USD", LastUpdateSequence: 7,
			Bids: []*marketpb.PriceLevel{lvl("50000", "3")},
			Asks: []*marketpb.PriceLevel{lvl("50001", "2")},
		}}, // no EventTime
	}}
	eng := New(Config{
		Book: book.New("BTC-USD", "BTCUSDT", "BINANCE"), Source: src, Publisher: prod,
		SnapshotInterval: 10 * time.Millisecond, SnapshotDepth: 10, Tenant: "test-tenant",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	<-ctx.Done()
	<-done

	got := framed(t, cc, SubjectMarketCryptoQuote)
	if len(got) == 0 {
		t.Fatal("nothing was published on the quote subject")
	}
	s := mark.New(time.Now, 30*time.Second)
	if err := s.Handle(context.Background(), got[0].env, got[0].payload); err != nil {
		t.Fatalf("mark.Handle: %v", err)
	}
	if _, _, _, ok := s.Touch("BTC-USD"); ok {
		t.Fatal("a quote from a book with no venue timestamp is being served as a live width — " +
			"an observation whose time is unknown must never be treated as fresh")
	}
}

// --- helpers ---

// ratOfDec reads a common.v1.Decimal exactly. No float anywhere on this path.
func ratOfDec(t *testing.T, v *commonpb.Decimal) *big.Rat {
	t.Helper()
	r, ok := dec.FromProtoChecked(v)
	if !ok {
		t.Fatalf("decimal %v is out of domain", v)
	}
	return r
}

func tsOf(at time.Time) *timestamppb.Timestamp { return timestamppb.New(at) }
