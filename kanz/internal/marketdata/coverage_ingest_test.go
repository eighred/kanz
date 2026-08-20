package marketdata

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketedge/coverage"
)

var covStart = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func covEnv() *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventType:     coverage.Subject,
		PartitionKey:  "BTC-USDT",
		EventTime:     timestamppb.New(covStart.Add(time.Minute)),
		IngestionTime: timestamppb.New(ingTime),
	}
}

func covMsg() *marketpb.IngestionCoverage {
	return &marketpb.IngestionCoverage{
		InstrumentId: "BTC-USDT",
		Mic:          "XBIN",
		BucketStart:  timestamppb.New(covStart),
		BucketEnd:    timestamppb.New(covStart.Add(time.Minute)),
		Observed:     durationpb.New(time.Minute),
		Attestor:     "binance:trades:BTCUSDT",
	}
}

// THE HANDLER MUST DISCRIMINATE BEFORE UNMARSHALLING.
//
// market-data subscribes to `market.>`, so the coverage FACT lands on the same
// handler as every candle. Once the bytes are decoded an IngestionCoverage and a
// MarketDataEvent are indistinguishable: proto3 reads the coverage record's
// fields into the wrong slots and the event is then refused for a missing
// event_time — which reads as a malformed market event rather than as the right
// payload on the wrong path, and the attestation is DLQ'd and lost. It cannot be
// re-observed, so this routing is the difference between a record and a gap.
func TestHandlerRoutesCoverageToTheCoverageSink(t *testing.T) {
	fw := &fakeWriter{}
	ing, err := NewIngestor(fw)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := proto.Marshal(covMsg())
	if err != nil {
		t.Fatal(err)
	}
	if err := ing.Handler(context.Background(), covEnv(), payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if len(fw.cov) != 1 {
		t.Fatalf("wrote %d coverage records, want 1 — the attestation did not reach the sink and "+
			"the interval now reads as UNKNOWN with nothing saying why", len(fw.cov))
	}
	if len(fw.bars) != 0 || len(fw.got) != 0 {
		t.Fatalf("a coverage FACT also wrote %d bars and %d observations — it carries no price and "+
			"must never become one", len(fw.bars), len(fw.got))
	}
	got := fw.cov[0]
	if got.Venue != "XBIN" || got.Attestor != "binance:trades:BTCUSDT" {
		t.Errorf("stored = %+v; venue and attestor are identity, not labels", got)
	}
	if !got.Whole() {
		t.Errorf("observed = %s, want a whole bucket", got.Observed)
	}
	if !got.RecordedAt.Equal(ingTime) {
		t.Errorf("recorded_at = %s, want the envelope's ingestion_time %s — an attestation that "+
			"arrived late is not evidence that was available live", got.RecordedAt, ingTime)
	}
}

// AN UNSET DURATION IS REFUSED, NOT READ AS ZERO.
//
// This is why the field is a Duration MESSAGE rather than an int64: proto3
// cannot tell an unset scalar from a zero one, and "the producer said nothing"
// and "we proved no coverage" are different facts. Only the second is a
// measurement, and only the second may be stored.
func TestTranslateCoverageRefusesAnUnsetObservation(t *testing.T) {
	msg := covMsg()
	msg.Observed = nil
	_, err := TranslateCoverage(covEnv(), msg)
	if err == nil {
		t.Fatal("an attestation with no observed duration was accepted — it would be stored as a " +
			"measured zero, which is a claim nobody made")
	}
	if !strings.Contains(err.Error(), "observed") {
		t.Errorf("the refusal must name the field: %v", err)
	}
}

// ONLY THE BASE RESOLUTION IS ATTESTED, matching TranslateBar.
//
// Coverage explains an absence in the 1-minute series. An hourly attestation
// would be a second, coarser answer to the same question — right or wrong in
// ways nothing could check against the 1m record.
func TestTranslateCoverageRefusesACoarserInterval(t *testing.T) {
	msg := covMsg()
	msg.BucketEnd = timestamppb.New(covStart.Add(time.Hour))
	msg.Observed = durationpb.New(time.Hour)
	if _, err := TranslateCoverage(covEnv(), msg); err == nil {
		t.Fatal("an hourly coverage record was accepted — two sources for one answer is two answers")
	}
}

// AN OVER-CLAIM IS REFUSED AT THE INGEST SEAM TOO, not only in the store.
func TestTranslateCoverageRefusesAnOverClaim(t *testing.T) {
	msg := covMsg()
	msg.Observed = durationpb.New(90 * time.Second)
	if _, err := TranslateCoverage(covEnv(), msg); err == nil {
		t.Fatal("an attestation claiming 90s of a 60s bucket was accepted — that is the strongest " +
			"possible claim about a window, produced by a bug")
	}
}

// AN UNSTAMPED ENVELOPE IS REFUSED, NOT SUBSTITUTED (#427), on this path too.
// Stamping the bucket's own end would claim Kanz knew the attestation the
// instant the interval closed, which is the one direction nothing audits.
func TestTranslateCoverageRefusesAnUnstampedEnvelope(t *testing.T) {
	e := covEnv()
	e.IngestionTime = nil
	e.PublishTime = nil
	if _, err := TranslateCoverage(e, covMsg()); err == nil {
		t.Fatal("an unstamped envelope was accepted and the attestation acquired a knowledge time " +
			"nobody recorded")
	}
}

// A MARKET EVENT IS STILL A MARKET EVENT. The dispatch must not swallow the
// path it was added beside.
func TestHandlerStillRoutesMarketEvents(t *testing.T) {
	fw := &fakeWriter{}
	ing, err := NewIngestor(fw)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: "AAPL",
		EventTime:    timestamppb.New(evtTime),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(15000, -2)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ing.Handler(context.Background(), env("market.equity.trade"), payload); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if len(fw.got) != 1 {
		t.Fatalf("wrote %d observations, want 1", len(fw.got))
	}
	if len(fw.cov) != 0 {
		t.Fatalf("a trade produced %d coverage records — coverage is never derived from market "+
			"data, which is the whole reason it can vouch for it", len(fw.cov))
	}
}
