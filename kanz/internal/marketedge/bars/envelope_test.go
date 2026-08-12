package bars_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketedge/bars"
	"github.com/eighred/kanz/internal/marketedge/trades"
	"github.com/eighred/kanz/pkg/bus"
)

// THIS RUNS A REAL bus.Producer OVER A FAKE CLIENT, and that is the point.
//
// CLAUDE.md says it plainly: "fakeBus does not validate envelopes, so it accepts
// what a real broker rejects. A green suite using it is not a broker proof." The
// other tests in this package use such a double — they prove the fold's
// decisions, not the wire. This proves the envelope: a missing tenant, an absent
// partition key or a wrong event class fails HERE rather than on the first live
// broker, where the symptom would be a bar series that silently stayed empty.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func TestTheBarEnvelopeSurvivesValidation(t *testing.T) {
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "market-ingest/test",
		ProducerVersion: "bars-1.0.0",
		// TENANT ON THE PRODUCER, matching market-ingest's composition root: book
		// snapshots and candles publish off a ticker with no inbound delivery to
		// inherit a tenant from, so there is nothing else for the envelope to take.
		Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	c := bars.NewCollector(prod, "acme", nil)
	at := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	s := bars.Series{InstrumentID: "BTC-USDT", Venue: "XBIN"}
	c.Observe(context.Background(), s, trades.Trade{
		Price: big.NewRat(100, 1), Size: big.NewRat(1, 1), EventTime: at,
	})
	c.Flush(context.Background(), at.Add(2*time.Minute))

	if len(cc.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(cc.sent))
	}
	env, payload, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("a real broker would REJECT this candle, and the series would stay empty for a "+
			"reason nobody would look for in the fold: %v", err)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT", env.GetEventClass())
	}
	if env.GetTenantId() != "acme" {
		t.Errorf("tenant = %q, want acme", env.GetTenantId())
	}

	var ev marketpb.MarketDataEvent
	if err := proto.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("payload is not a MarketDataEvent: %v", err)
	}
	b := ev.GetBar()
	if b == nil {
		t.Fatal("the payload carries no Bar")
	}
	if got := b.GetCloseTime().AsTime().Sub(b.GetOpenTime().AsTime()); got != time.Minute {
		t.Errorf("interval = %s, want 1m — marketdata.TranslateBar refuses anything else", got)
	}
	if ev.GetMic() != "XBIN" {
		t.Errorf("mic = %q, want XBIN — TranslateBar refuses a bar with no venue", ev.GetMic())
	}
}
