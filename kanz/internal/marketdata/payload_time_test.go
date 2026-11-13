package marketdata

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/internal/marketedge/coverage"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
)

type payloadTimeCase struct {
	name, eventType string
	payload         proto.Message
}

// The +/- one-billion encodings normalize to the exact same valid instant.
// Accepting them would test normalization, rather than protobuf validity.
func invalidPayloadTimes(at time.Time) map[string]*timestamppb.Timestamp {
	return map[string]*timestamppb.Timestamp{
		"negative nanos":  {Seconds: at.Unix() + 1, Nanos: -1_000_000_000},
		"negative one":    {Seconds: at.Unix(), Nanos: -1},
		"overflow nanos":  {Seconds: at.Unix() - 1, Nanos: 1_000_000_000},
		"before year one": {Seconds: -62135596801},
		"after year 9999": {Seconds: 253402300800},
	}
}

func malformedPayloadTimeCases(at time.Time) []payloadTimeCase {
	var cases []payloadTimeCase
	for name, stamp := range invalidPayloadTimes(at) {
		for _, typ := range []string{"trade", "quote", "bar"} {
			ev := timedMarketEvent(typ, at)
			ev.EventTime = proto.Clone(stamp).(*timestamppb.Timestamp)
			cases = append(cases, payloadTimeCase{name + "/" + typ + " event", "market.crypto." + typ, ev})
		}
		for _, field := range []string{"open", "close"} {
			ev := timedMarketEvent("bar", at)
			if field == "open" {
				ev.GetBar().OpenTime = proto.Clone(stamp).(*timestamppb.Timestamp)
			} else {
				ev.GetBar().CloseTime = invalidPayloadTimes(at.Add(time.Minute))[name]
			}
			cases = append(cases, payloadTimeCase{name + "/bar " + field, "market.crypto.bar", ev})
		}
		for _, field := range []string{"start", "end"} {
			cov := covMsg()
			cov.BucketStart, cov.BucketEnd = timestamppb.New(at), timestamppb.New(at.Add(time.Minute))
			if field == "start" {
				cov.BucketStart = proto.Clone(stamp).(*timestamppb.Timestamp)
			} else {
				cov.BucketEnd = invalidPayloadTimes(at.Add(time.Minute))[name]
			}
			cases = append(cases, payloadTimeCase{name + "/coverage " + field, coverage.Subject, cov})
		}
	}
	return cases
}

func timedMarketEvent(typ string, at time.Time) *marketpb.MarketDataEvent {
	switch typ {
	case "bar":
		return barEvent("XBIN", at, at.Add(time.Minute))
	case "quote":
		return &marketpb.MarketDataEvent{InstrumentId: "TIME", EventTime: timestamppb.New(at), Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{BidPrice: decv(100, 0), AskPrice: decv(102, 0)}}}
	default:
		return &marketpb.MarketDataEvent{InstrumentId: "TIME", EventTime: timestamppb.New(at), Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: decv(101, 0)}}}
	}
}

func TestPayloadTimestampRefusalBeforeAnyWrite(t *testing.T) {
	for _, tc := range malformedPayloadTimeCases(evtTime) {
		t.Run(tc.name, func(t *testing.T) {
			fw := &fakeWriter{}
			ing, err := NewIngestor(fw)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := proto.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := ing.Handler(context.Background(), env(tc.eventType), payload); err == nil {
				t.Fatal("malformed present timestamp was normalized or substituted")
			}
			if len(fw.got)+len(fw.bars)+len(fw.cov) != 0 {
				t.Fatal("refused timestamp produced a partial observation, candle or attestation")
			}
		})
	}
}

func TestPayloadTimestampValidPrecisionAndFallback(t *testing.T) {
	for _, at := range []time.Time{
		time.Unix(evtTime.Unix(), 1).UTC(),
		time.Unix(evtTime.Unix(), 999999999).UTC(),
		time.Unix(-62135596800, 1).UTC(),
		time.Unix(253402300799, 999999999).UTC(),
	} {
		for _, typ := range []string{"trade", "quote"} {
			t.Run(typ+"/"+at.Format(time.RFC3339Nano), func(t *testing.T) {
				ev := timedMarketEvent(typ, at)
				if err := ev.EventTime.CheckValid(); err != nil {
					t.Fatal(err)
				}
				obs, err := TranslateEvent(env("market.crypto."+typ), ev)
				if err != nil || !obs.ObservationTime.Equal(at) || !obs.KnowledgeTime.Equal(ingTime) {
					t.Fatalf("valid instant changed: %+v error=%v", obs, err)
				}
			})
		}
	}
	// Nil alone retains the established envelope fallback; a malformed present
	// field above must not choose this otherwise-valid source.
	ev := timedMarketEvent("trade", evtTime)
	ev.EventTime = nil
	obs, err := TranslateEvent(env("market.crypto.trade"), ev)
	if err != nil || !obs.ObservationTime.Equal(evtTime) || obs.Kind != store.PriceKindLast {
		t.Fatalf("nil fallback changed: %+v error=%v", obs, err)
	}
	for _, nanos := range []int32{0, 1, 999999999} {
		at := time.Unix(evtTime.Unix(), int64(nanos)).UTC()
		ev := timedMarketEvent("bar", at)
		bar, ok, err := TranslateBar(env("market.crypto.bar"), ev)
		if err != nil || !ok || !bar.BucketStart.Equal(at) {
			t.Fatalf("valid bar precision changed: %+v ok=%v error=%v", bar, ok, err)
		}
	}
}

// Direct translators are also used outside Handler; bar validation must finish
// before either a candle or its scalar close can be consumed.
func TestPayloadTimestampDirectTranslatorRefusal(t *testing.T) {
	for _, tc := range malformedPayloadTimeCases(evtTime) {
		t.Run(tc.name, func(t *testing.T) {
			switch payload := tc.payload.(type) {
			case *marketpb.MarketDataEvent:
				if _, err := TranslateEvent(env(tc.eventType), payload); err == nil {
					t.Fatal("event translator accepted malformed event or bar timestamp")
				}
				if _, err := TranslateBatch(env(tc.eventType), &marketpb.MarketDataBatch{Events: []*marketpb.MarketDataEvent{timedMarketEvent("trade", evtTime), payload}}); err == nil {
					t.Fatal("batch translator returned partial valid observations")
				}
				if payload.GetBar() != nil {
					if _, _, err := TranslateBar(env(tc.eventType), payload); err == nil {
						t.Fatal("bar translator accepted malformed event or interval timestamp")
					}
				}
			case *marketpb.IngestionCoverage:
				if _, err := TranslateCoverage(env(tc.eventType), payload); err == nil {
					t.Fatal("coverage translator accepted malformed interval timestamp")
				}
			}
		})
	}
}
