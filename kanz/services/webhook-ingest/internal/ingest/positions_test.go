package ingest

// EXEC-M19b — a `close` signal did NOTHING.
//
// cmd/webhook-ingest wired `Positions: ingest.StaticPositions{}` — an EMPTY MAP — with the
// comment "M1 sim: flat by default; M3+ binds the OMS projection". M3+ never bound it.
// translate.legSizeAndSide sizes a CLOSE leg from the position AT THAT VENUE, got zero, and
// the fan-out skipped the leg: ZERO ORDERS, and the webhook answered 202 Accepted. A
// strategy that opened a position and later told Kanz to close it was silently ignored, and
// the position stayed open.
//
// The cache below is the real source: it folds the per-venue position FACTs the OMS
// publishes (EXEC-M19a) off a compacted stream, so it is correct after a restart and on
// every replica.

import (
	"context"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

// feed folds one per-venue PositionState FACT into the cache, as the bus would.
func feed(t *testing.T, c *PositionCache, fund, venue, instrument string, qty int64) {
	t.Helper()
	feedRat(t, c, fund, venue, instrument, big.NewRat(qty, 1))
}

func feedRat(t *testing.T, c *PositionCache, fund, venue, instrument string, qty *big.Rat) {
	t.Helper()
	st := &domainpb.PositionState{
		PortfolioId:  fund,
		Venue:        venue,
		InstrumentId: instrument,
		Quantity:     dec.ToProto(qty),
		AsOf:         timestamppb.New(time.Now().UTC()),
	}
	payload, err := proto.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(context.Background(), &envelopepb.Envelope{}, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// TestTheCacheKnowsWhichVenueHoldsIt: a fund holding 1 BTC at XBIN and 2 at XOKX must be
// reported that way. Reporting the fund's total for each venue would make a CLOSE at XBIN
// try to sell 3 BTC when XBIN holds 1 — over-selling into a naked short.
func TestTheCacheKnowsWhichVenueHoldsIt(t *testing.T) {
	c := NewPositionCache()
	c.Arm()
	feed(t, c, "fund-alpha", "XBIN", "BTC-USD", 1)
	feed(t, c, "fund-alpha", "XOKX", "BTC-USD", 2)

	for _, tc := range []struct {
		venue string
		want  int64
	}{{"XBIN", 1}, {"XOKX", 2}} {
		got, err := c.Position(context.Background(), "fund-alpha", tc.venue, "BTC-USD")
		if err != nil {
			t.Fatalf("%s: %v", tc.venue, err)
		}
		if got.Cmp(big.NewRat(tc.want, 1)) != 0 {
			t.Errorf("%s holds %s, want %d", tc.venue, got.RatString(), tc.want)
		}
	}
}

// TestAnUnarmedCacheREFUSES is the startup hole, closed.
//
// Before the compacted stream has replayed, the cache knows NOTHING — and "I have not
// learned the book yet" must never be reported as "the fund is flat". Flat is what makes a
// CLOSE emit zero orders and answer 202: the signal would be swallowed, silently, exactly
// as it was before. It REFUSES until armed, so the pipeline errors (and webhook-ingest's
// readiness holds the pod out of its Service until then).
func TestAnUnarmedCacheREFUSES(t *testing.T) {
	c := NewPositionCache() // not armed: the replay has not landed

	if _, err := c.Position(context.Background(), "fund-alpha", "XBIN", "BTC-USD"); err == nil {
		t.Fatal("an unarmed cache reported a position — 'I have not learned the book' was rendered as 'the fund is flat'")
	}
}

// TestAnArmedCacheReportsFlatForAHoldingThatIsNotThere: once armed, the cache HAS seen the
// whole book, so an instrument it has no entry for is genuinely not held. Closing it is a
// no-op, and that is a correct answer rather than a swallowed one.
func TestAnArmedCacheReportsFlatForAHoldingThatIsNotThere(t *testing.T) {
	c := NewPositionCache()
	c.Arm()
	feed(t, c, "fund-alpha", "XBIN", "BTC-USD", 1)

	got, err := c.Position(context.Background(), "fund-alpha", "XBIN", "ETH-USD")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sign() != 0 {
		t.Errorf("ETH = %s, want 0 — the fund does not hold it", got.RatString())
	}
}

// TestAClosedPositionIsZeroNotAbsent: when a holding is flattened the OMS publishes a
// PositionState with quantity 0, and the compacted stream RETAINS it. The cache must fold
// that to zero rather than keep the old size — a CLOSE against a stale non-zero would sell
// something the fund no longer has.
func TestAClosedPositionIsZeroNotAbsent(t *testing.T) {
	c := NewPositionCache()
	c.Arm()
	feed(t, c, "fund-alpha", "XBIN", "BTC-USD", 3)
	feed(t, c, "fund-alpha", "XBIN", "BTC-USD", 0) // flattened

	got, err := c.Position(context.Background(), "fund-alpha", "XBIN", "BTC-USD")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sign() != 0 {
		t.Errorf("BTC = %s after being flattened, want 0", got.RatString())
	}
}

// TestACloseSignalActuallyCloses is the point of EXEC-M19.
//
// The pipeline's CLOSE logic was always right. What was wrong was the COMPOSITION ROOT:
// production wired `Positions: ingest.StaticPositions{}` — an empty map — so every venue
// reported flat, every leg was skipped, and a `close` alert produced ZERO ORDERS while the
// webhook answered 202 Accepted. This drives the pipeline over the REAL source.
func TestACloseSignalActuallyCloses(t *testing.T) {
	positions := NewPositionCache()
	positions.Arm()
	// The fund is long 0.6 BTC at BINANCE and 0.4 at OKX — what the OMS's per-venue FACTs say.
	feedRat(t, positions, "fund-alpha", "BINANCE", "BTC-USD", big.NewRat(6, 10))
	feedRat(t, positions, "fund-alpha", "OKX", "BTC-USD", big.NewRat(4, 10))

	pub := &brokenPublisher{}
	p := pipelineWithPositions(t, positions, pub)

	raw := body("close", "1", "absolute_qty", "close-1")
	if err := post(t, p, raw); err != nil {
		t.Fatalf("close: %v", err)
	}

	cmds := pub.submitted()
	if len(cmds) != 2 {
		t.Fatalf("a `close` produced %d orders, want 2 — the loop can open a position and not close one", len(cmds))
	}
	got := map[string]string{}
	for _, c := range cmds {
		if c.GetSide() != orderpb.Side_SIDE_SELL {
			t.Errorf("close side = %v, want SELL (long → sell to flatten)", c.GetSide())
		}
		got[c.GetVenue()] = dec.FromProto(c.GetQuantity()).RatString()
	}
	// EACH VENUE FLATTENS WHAT IT HOLDS. Sizing both legs from the fund's total (1 BTC)
	// would over-sell at each exchange and flip a flat position into a naked short.
	if got["BINANCE"] != "3/5" || got["OKX"] != "2/5" {
		t.Errorf("flatten sizes = %v, want BINANCE 3/5 and OKX 2/5", got)
	}
}

// THE POSITION CACHE REFUSES AN OUT-OF-DOMAIN QUANTITY (#95).
//
// This cache is what a CLOSE is sized from, and its input is a PositionState off
// the bus whose Decimal.exponent is unvalidated. Before the domain check,
// dec.FromProto would materialise 10^abs(exponent) and Handle would never
// return — the position subscription stalls, and every later CLOSE is sized from
// a cache that stopped updating. The timeout is what separates "refused" from
// "still computing".
func TestPositionCacheRefusesAnOutOfDomainQuantity(t *testing.T) {
	c := NewPositionCache()
	c.Arm()
	feed(t, c, "FUND", "XBIN", "BTC-USD", 1) // a known-good holding first

	st := &domainpb.PositionState{
		PortfolioId:  "FUND",
		Venue:        "XBIN",
		InstrumentId: "BTC-USD",
		Quantity:     &commonpb.Decimal{Coefficient: 1, Exponent: 2000000000},
		AsOf:         timestamppb.New(time.Now().UTC()),
	}
	payload, err := proto.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Handle(context.Background(), &envelopepb.Envelope{}, payload) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an out-of-domain quantity was ACKED — a holding this cache cannot read " +
				"must nack, exactly as a malformed payload does")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Handle did not return within 4s — the domain check is not in front of the conversion")
	}

	// AND THE OLD HOLDING SURVIVES. The refusal must not half-apply: this cache
	// SETS rather than adds, so a partial fold would leave the fund's size at
	// whatever the refused message implied — and zero here reads as "flat", which
	// is the value a CLOSE would size from.
	got, err := c.Position(context.Background(), "FUND", "XBIN", "BTC-USD")
	if err != nil {
		t.Fatalf("Position after a refused update: %v", err)
	}
	if got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("holding is now %s, want 1 — the refused update overwrote a known-good position", got)
	}
}
