// THE MARK TICK ENVELOPE, DRIVEN THROUGH A REAL PRODUCER (#245's rule, #673's code).
//
// tickSink in marktick_test.go is an EVENT-LEVEL double: it appends the
// bus.Event and answers nil. That is the right tool for asserting WHICH events
// the publisher emits and what they say, and it is the wrong tool for asserting
// that a broker would take them — it never runs bus.Validate, and none of these
// sites sets PayloadSchemaRef, which the producer DERIVES and Validate requires
// non-empty. pkg/bus/producer.go records twelve sites that validated fine
// against doubles and failed on the first real broker.
//
// THE OBLIGATION MOVED HERE WITH THE CODE. This envelope used to be built inside
// each venue adapter, where publish_test.go carried the proof; #673 folded both
// into one publisher, so the proof belongs beside it. It mirrors
// services/venue-okx/internal/okx/publish_test.go and its Binance twin
// deliberately — a reader comparing them should find the same assertions rather
// than a third dialect.
//
// AND IT IS THE ENVELOPE WITH THE THINNEST MARGIN. A mark tick has no inbound
// delivery to inherit a tenant from, so ProducerConfig.Tenant is its only
// source; that is exactly what was empty when both venue feeds were refusing
// every publish in silence. See TestEveryBusProducerConfigSetsATenant.
package execution

import (
	"context"
	"errors"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// markCaptureClient records the framed wire bytes — the Tier-B helper from
// internal/risk/publish and services/accounting/internal/cashmove.
type markCaptureClient struct{ sent []bus.Message }

func (c *markCaptureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *markCaptureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *markCaptureClient) Close() error { return nil }

// realVenueProducer mirrors the venue composition roots' producer config,
// INCLUDING the Tenant fallback.
func realVenueProducer(t *testing.T) (*bus.Producer, *markCaptureClient) {
	t.Helper()
	cc := &markCaptureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "venue-binance",
		ProducerVersion: "test",
		Tenant:          "fund-alpha",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

func TestMarkTickEmitsAValidEnvelope(t *testing.T) {
	prod, cc := realVenueProducer(t)
	p := NewMarkTickPublisher(prod, nil, "BINANCE", nil)

	p.PublishTrade(context.Background(), "BTC-USD", "BTCUSDT", markPrice(t, "50123.5"))

	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1 — the mark tick did not reach the transport", len(cc.sent))
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("the emitted envelope fails Validate: %v — a real broker refuses this, and until #673 "+
			"the feed would have discarded the refusal", err)
	}
	if got := env.GetEventType(); got != SubjectMarketCryptoTrade {
		t.Errorf("event_type = %q, want %q", got, SubjectMarketCryptoTrade)
	}
	if got := env.GetPayloadSchemaRef(); got != "market.v1.MarketDataEvent:1" {
		t.Errorf("payload_schema_ref = %q, want market.v1.MarketDataEvent:1 — this site sets none, so the "+
			"producer DERIVES it from the payload type, and a wrong or empty derivation fails every "+
			"publish", got)
	}
	if got := env.GetTenantId(); got != "fund-alpha" {
		t.Errorf("tenant_id = %q, want fund-alpha", got)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", got)
	}
	if got := string(cc.sent[0].Key); got != "BTC-USD" {
		t.Errorf("partition key = %q, want BTC-USD", got)
	}
}

// THE TENANT FALLBACK IS LOAD-BEARING, AND ITS FAILURE USED TO BE SILENT.
//
// A time.Ticker poll has no inbound delivery, and this publisher stamps no
// Event.TenantID, so ProducerConfig.Tenant is the only route left. Dropping it
// refuses every mark tick — the state both venue adapters actually shipped in.
// It is no longer silent, which this test asserts alongside the refusal: the
// counter fires and the WARN carries the broker's reason.
func TestAMarkTickWithoutTheTenantFallbackIsRefusedAndObserved(t *testing.T) {
	cc := &markCaptureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "venue-binance",
		ProducerVersion: "test",
		// Tenant deliberately absent.
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	dropped := 0
	p := NewMarkTickPublisher(prod, nil, "BINANCE", func(string, string) { dropped++ })
	p.PublishTrade(context.Background(), "BTC-USD", "BTCUSDT", markPrice(t, "50123.5"))

	if len(cc.sent) != 0 {
		t.Fatalf("a mark tick with no tenant reached the transport (%d message(s)) — bus.Validate is "+
			"supposed to refuse it, and this whole path was designed around that refusal", len(cc.sent))
	}
	if dropped != 1 {
		t.Fatalf("the refused tick was observed %d times, want 1. This is the exact production state both "+
			"venue adapters ran in, and it produced no log line, no series and no downstream difference", dropped)
	}
}
