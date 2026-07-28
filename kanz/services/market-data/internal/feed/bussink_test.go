package feed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/market-data/internal/feed"
)

// captureClient is a bus.Client that records every framed message.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func newProducer(t *testing.T, cc bus.Client) *bus.Producer {
	t.Helper()
	p, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source: "market-data/test", ProducerVersion: "v0", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return p
}

func mustEvent(t *testing.T, build func() (*marketpb.MarketDataEvent, error)) *marketpb.MarketDataEvent {
	t.Helper()
	ev, err := build()
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	return ev
}

func TestBusSinkPublishesPerVariantSubjectAndPartitionKey(t *testing.T) {
	cc := &captureClient{}
	sink, err := feed.NewBusSink(newProducer(t, cc), "equity")
	if err != nil {
		t.Fatalf("NewBusSink: %v", err)
	}

	et := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	m := feed.Meta{InstrumentID: "AAPL", Symbol: "AAPL", MIC: "XNAS", EventTime: et, SourceSequence: 1}
	trade := mustEvent(t, func() (*marketpb.MarketDataEvent, error) {
		return feed.Trade(m, feed.DecimalFromFloat(101.25, -2), feed.DecimalFromFloat(100, 0), "t1")
	})
	quote := mustEvent(t, func() (*marketpb.MarketDataEvent, error) {
		return feed.Quote(m,
			feed.DecimalFromFloat(101.24, -2), feed.DecimalFromFloat(100, 0),
			feed.DecimalFromFloat(101.26, -2), feed.DecimalFromFloat(100, 0))
	})

	ctx := context.Background()
	if err := sink.Publish(ctx, trade); err != nil {
		t.Fatalf("publish trade: %v", err)
	}
	if err := sink.Publish(ctx, quote); err != nil {
		t.Fatalf("publish quote: %v", err)
	}

	if len(cc.sent) != 2 {
		t.Fatalf("published %d messages, want 2", len(cc.sent))
	}

	wantSubjects := []string{"market.equity.trade", "market.equity.quote"}
	for i, msg := range cc.sent {
		env, payload, err := bus.Unframe(msg.Body)
		if err != nil {
			t.Fatalf("unframe %d: %v", i, err)
		}
		if env.GetEventType() != wantSubjects[i] {
			t.Errorf("event %d event_type=%q want %q", i, env.GetEventType(), wantSubjects[i])
		}
		if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
			t.Errorf("event %d class=%v want FACT", i, env.GetEventClass())
		}
		if env.GetPartitionKey() != "AAPL" {
			t.Errorf("event %d partition_key=%q want AAPL (per-instrument ordering)", i, env.GetPartitionKey())
		}
		if env.GetPayloadSchemaRef() != "market.v1.MarketDataEvent:1" {
			t.Errorf("event %d schema_ref=%q", i, env.GetPayloadSchemaRef())
		}
		if got := env.GetEventTime().AsTime(); !got.Equal(et) {
			t.Errorf("event %d event_time=%s want tick time %s (not wall-clock)", i, got, et)
		}
		// Payload round-trips to the same MarketDataEvent.
		var decoded marketpb.MarketDataEvent
		if err := proto.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("payload unmarshal %d: %v", i, err)
		}
		if decoded.GetInstrumentId() != "AAPL" {
			t.Errorf("event %d payload instrument=%q", i, decoded.GetInstrumentId())
		}
	}
}

func TestBusSinkNilProducer(t *testing.T) {
	if _, err := feed.NewBusSink(nil, "equity"); err == nil {
		t.Fatal("NewBusSink(nil) = nil error, want non-nil")
	}
}

func TestBusSinkRejectsMalformedEvent(t *testing.T) {
	cc := &captureClient{}
	sink, err := feed.NewBusSink(newProducer(t, cc), "")
	if err != nil {
		t.Fatalf("NewBusSink: %v", err)
	}
	// No data variant + missing fields ⇒ Validate rejects, nothing published.
	if err := sink.Publish(context.Background(), &marketpb.MarketDataEvent{}); err == nil {
		t.Fatal("publish malformed = nil error, want reject")
	}
	if len(cc.sent) != 0 {
		t.Fatalf("published %d messages for malformed event, want 0", len(cc.sent))
	}
}

// The Gate (PARITY-01g) decorates a BusSink without change — a valid event flows
// through the gate to the producer, proving the innermost-Sink composition.
func TestBusSinkComposesBehindGate(t *testing.T) {
	cc := &captureClient{}
	sink, err := feed.NewBusSink(newProducer(t, cc), "equity")
	if err != nil {
		t.Fatalf("NewBusSink: %v", err)
	}
	gate := feed.NewGate(sink, time.Hour, nil) // generous staleness budget

	et := time.Now().Add(-time.Second)
	ev := mustEvent(t, func() (*marketpb.MarketDataEvent, error) {
		return feed.Trade(
			feed.Meta{InstrumentID: "MSFT", Symbol: "MSFT", MIC: "XNAS", EventTime: et, SourceSequence: 1},
			feed.DecimalFromFloat(410.10, -2), feed.DecimalFromFloat(50, 0), "t1")
	})
	if err := gate.Publish(context.Background(), ev); err != nil {
		t.Fatalf("gate publish: %v", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("gate→bus published %d, want 1", len(cc.sent))
	}
}

func TestSyntheticSessionIsValidAndGapFree(t *testing.T) {
	start := time.Date(2026, 7, 3, 9, 30, 0, 0, time.UTC)
	sess := feed.SyntheticSession(start, time.Second, []string{"AAPL", "MSFT"}, 4)
	if len(sess) != 8 {
		t.Fatalf("session len=%d want 8 (2 instruments × 4 ticks)", len(sess))
	}
	for i, ev := range sess {
		if err := feed.Validate(ev); err != nil {
			t.Errorf("event %d invalid: %v", i, err)
		}
	}
	if breaks := feed.Gap(sess); len(breaks) != 0 {
		t.Errorf("synthetic session has sequence gaps: %v", breaks)
	}
	if bad := feed.OutOfOrder(sess); len(bad) != 0 {
		t.Errorf("synthetic session has ordering inversions: %v", bad)
	}
}
