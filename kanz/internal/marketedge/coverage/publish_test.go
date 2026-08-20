package coverage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketedge/coverage"
	"github.com/eighred/kanz/pkg/bus"
)

// THIS RUNS A REAL bus.Producer OVER A FAKE CLIENT, and that is the point.
//
// CLAUDE.md: "fakeBus does not validate envelopes, so it accepts what a real
// broker rejects. A green suite using it is not a broker proof." The rest of
// this package's tests prove the ACCOUNTING against a double. This proves the
// WIRE — and the cost of getting it wrong here is worse than for a candle: a
// rejected candle can be re-derived from the venue tomorrow, while a rejected
// attestation is gone permanently, and the interval it covered reads as UNKNOWN
// forever with nothing anywhere saying why.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func TestTheCoverageEnvelopeSurvivesValidation(t *testing.T) {
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "market-ingest/test",
		ProducerVersion: "coverage-1.0.0",
		// TENANT ON THE PRODUCER, matching market-ingest's composition root:
		// coverage publishes off a subscription heartbeat with no inbound
		// delivery to inherit a tenant from, so there is nothing else for the
		// envelope to take.
		Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	rec, err := coverage.NewRecorder(coverage.Config{
		Publisher: prod, Tenant: "acme", MaxSilence: 45 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	obs := rec.Observe(coverage.Series{InstrumentID: "BTC-USDT", Venue: "XBIN"}, "binance:trades:BTCUSDT")
	for at := base.Add(-time.Minute); !at.After(base.Add(time.Minute)); at = at.Add(20 * time.Second) {
		obs.Live(at)
	}

	if len(cc.sent) == 0 {
		t.Fatal("nothing was published")
	}
	env, payload, err := bus.Unframe(cc.sent[len(cc.sent)-1].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("a real broker would REJECT this attestation, and the interval would read as "+
			"UNKNOWN forever for a reason nobody would look for in the recorder: %v", err)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT", env.GetEventClass())
	}
	if env.GetTenantId() != "acme" {
		t.Errorf("tenant = %q, want acme", env.GetTenantId())
	}
	if env.GetEventType() != coverage.Subject {
		t.Errorf("event type = %q, want %q — marketdata.Ingestor discriminates on this string "+
			"BEFORE unmarshalling, so a mismatch routes the attestation into the MarketDataEvent "+
			"path and DLQs it", env.GetEventType(), coverage.Subject)
	}

	var cov marketpb.IngestionCoverage
	if err := proto.Unmarshal(payload, &cov); err != nil {
		t.Fatalf("payload is not an IngestionCoverage: %v", err)
	}
	if cov.GetObserved() == nil {
		t.Fatal("observed is unset on the wire — marketdata.TranslateCoverage refuses that, " +
			"deliberately, because an unset duration and a proven zero are different facts")
	}
	if got := cov.GetBucketEnd().AsTime().Sub(cov.GetBucketStart().AsTime()); got != time.Minute {
		t.Errorf("interval = %s, want 1m — TranslateCoverage refuses anything else", got)
	}
	if cov.GetMic() != "XBIN" {
		t.Errorf("mic = %q, want XBIN — coverage is per venue", cov.GetMic())
	}
	if cov.GetAttestor() == "" {
		t.Error("attestor is empty — a coverage claim nothing can be traced to cannot be audited")
	}
	if !env.GetEventTime().AsTime().Equal(cov.GetBucketEnd().AsTime()) {
		t.Errorf("envelope event_time %s != bucket_end %s — the attestation becomes true only when "+
			"the interval has elapsed", env.GetEventTime().AsTime(), cov.GetBucketEnd().AsTime())
	}
}
