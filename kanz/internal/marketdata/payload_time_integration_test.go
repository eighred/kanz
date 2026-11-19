package marketdata

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/marketdata/marketdatatest"
	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/marketedge/coverage"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

func publishPayloadTimeCases(t *testing.T, ctx context.Context, b marketdatatest.Broker, cases []payloadTimeCase, at time.Time) {
	t.Helper()
	frames := make([]marketdatatest.Frame, 0, len(cases))
	for _, c := range cases {
		frames = append(frames, marketdatatest.Frame{EventType: c.eventType, Payload: c.payload})
	}
	b.Publish(t, ctx, frames, at)
}

func TestPayloadTimestampJetStreamPostgresIngressReplay(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" || os.Getenv("TEST_NATS_URL") == "" {
		t.Skip("requires TEST_POSTGRES_URL and TEST_NATS_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := boundaryPool(t, ctx, dsn)
	t.Log("actual PostgreSQL role and memberships: NOSUPERUSER NOBYPASSRLS; market facts are global reference data")
	st := store.NewPostgres(pool)
	ing, err := NewIngestor(st)
	if err != nil {
		t.Fatal(err)
	}
	b := marketdatatest.NewBroker(t, ctx)
	at := time.Now().UTC().Truncate(time.Minute)
	frames := malformedPayloadTimeCases(at)
	badCount := len(frames)
	for _, typ := range []string{"trade", "quote", "bar"} {
		frames = append(frames, payloadTimeCase{"valid " + typ, "market.crypto." + typ, timedMarketEvent(typ, at)})
	}
	nilTime := timedMarketEvent("trade", at)
	nilTime.EventTime = nil
	frames = append(frames, payloadTimeCase{"valid nil fallback", "market.crypto.trade", nilTime})
	cov := covMsg()
	cov.BucketStart, cov.BucketEnd = timestamppb.New(at), timestamppb.New(at.Add(time.Minute))
	frames = append(frames, payloadTimeCase{"valid coverage", coverage.Subject, cov})
	refused := map[string]bool{}
	for n, frame := range frames {
		id := fmt.Sprintf("%s-%d", b.ID, n)
		switch payload := frame.payload.(type) {
		case *marketpb.MarketDataEvent:
			payload.InstrumentId = id
		case *marketpb.IngestionCoverage:
			payload.InstrumentId = id
		}
		if n < badCount {
			refused[id] = true
		}
	}
	publishPayloadTimeCases(t, ctx, b, frames, at.Add(2*time.Minute))
	for pass := range 2 {
		b.Consume(t, ctx, pass, ing.Handler, len(frames), refused)
		for n, frame := range frames {
			id := frame.payload.GetInstrumentId()
			var marks, bars, attestations int
			if err := pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM price_observations WHERE instrument_id=$1),
				(SELECT count(*) FROM ohlcv_bars WHERE instrument_id=$1),
				(SELECT count(*) FROM ingestion_coverage WHERE instrument_id=$1)`, id).Scan(&marks, &bars, &attestations); err != nil {
				t.Fatal(err)
			}
			wantMarks, wantBars, wantCoverage := 0, 0, 0
			if n >= badCount {
				switch frame.eventType {
				case coverage.Subject:
					wantCoverage = 1
				case "market.crypto.bar":
					wantMarks, wantBars = 1, 1
				default:
					wantMarks = 1
				}
			}
			if marks != wantMarks || bars != wantBars || attestations != wantCoverage {
				t.Fatalf("pass %d %s: marks/bar/coverage=%d/%d/%d, want %d/%d/%d", pass, frame.name, marks, bars, attestations, wantMarks, wantBars, wantCoverage)
			}
			if wantMarks == 1 {
				rows, err := st.History(ctx, store.Query{InstrumentID: id})
				wantAt := at
				if frame.eventType == "market.crypto.bar" {
					wantAt = at.Add(time.Minute)
				} else if frame.name == "valid nil fallback" {
					wantAt = at.Add(2 * time.Minute)
				}
				if len(rows) != 1 || err != nil || !rows[0].ObservationTime.Equal(wantAt) || !rows[0].KnowledgeTime.Equal(at.Add(2*time.Minute)) {
					t.Fatalf("valid stored timestamp/fallback changed: %+v error=%v", rows, err)
				}
			}
		}
		t.Logf("pass %d: %d malformed frames durably parked, five valid controls retained; no partial writes", pass, badCount)
	}
}

func TestPayloadTimestampJetStreamLiveMonitor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b := marketdatatest.NewBroker(t, ctx)
	at := time.Now().UTC().Truncate(time.Second)
	var frames []payloadTimeCase
	for name, stamp := range invalidPayloadTimes(at) {
		for _, typ := range []string{"trade", "quote"} {
			id := fmt.Sprintf("%s-%d", b.ID, len(frames))
			seed := timedMarketEvent("quote", at)
			seed.InstrumentId = id
			bad := timedMarketEvent(typ, at)
			bad.InstrumentId, bad.EventTime = id, stamp
			if typ == "trade" {
				bad.GetTrade().Price = decv(900, 0)
			} else {
				bad.GetQuote().BidPrice, bad.GetQuote().AskPrice = decv(900, 0), decv(902, 0)
			}
			valid := timedMarketEvent("quote", at.Add(time.Nanosecond))
			valid.InstrumentId = id
			valid.GetQuote().BidPrice, valid.GetQuote().AskPrice = decv(200, 0), decv(202, 0)
			frames = append(frames,
				payloadTimeCase{name + "/seed", "market.crypto.quote", seed},
				payloadTimeCase{name + "/" + typ, "market.crypto." + typ, bad},
				payloadTimeCase{name + "/valid recovery", "market.crypto.quote", valid})
		}
	}
	publishPayloadTimeCases(t, ctx, b, frames, at)
	for pass := range 2 {
		s := mark.New(func() time.Time { return at }, time.Minute)
		stages := map[string]int{}
		h := func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			if err := s.Handle(ctx, env, payload); err != nil {
				return err
			}
			id := env.GetPartitionKey()
			stage := stages[id]
			stages[id]++
			wantPrice, wantBid, wantAsk, wantAt := "101", "100", "102", at
			if stage == 2 {
				wantPrice, wantBid, wantAsk, wantAt = "201", "200", "202", at.Add(time.Nanosecond)
			}
			price, gotAt, seen := s.Lookup(id)
			bid, ask, touchAt, ok := s.Touch(id)
			if !seen || price == nil || price.FloatString(0) != wantPrice || !gotAt.Equal(wantAt) || !ok || bid.FloatString(0) != wantBid || ask.FloatString(0) != wantAsk || !touchAt.Equal(wantAt) {
				return fmt.Errorf("stage %d: price/touch changed or valid recovery lost precision", stage)
			}
			return nil
		}
		b.Consume(t, ctx, pass, h, len(frames), nil)
		t.Logf("pass %d: %d real broker deliveries acknowledged; malformed times preserved prior price/touch, valid recovery kept nanoseconds", pass, len(frames))
	}
}
