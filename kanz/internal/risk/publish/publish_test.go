package publish_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/publish"
	"github.com/kanz-eng/kanz/pkg/bus"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// captureClient mirrors the helper used in EVT-17 tests and the
// EVT-21a contract tests — records every Publish for inspection.
type captureClient struct {
	sent []bus.Message
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func newPublisher(t *testing.T) (*publish.Publisher, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "risk-engine/test",
		ProducerVersion: "risk-1.0.0",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := publish.NewPublisher(prod)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	return pub, cc
}

func money(amount int64, exp int32, ccy string) *commonpb.Money {
	return &commonpb.Money{
		Amount:       &commonpb.Decimal{Coefficient: amount, Exponent: exp},
		CurrencyCode: ccy,
	}
}

func TestNewPublisher_NilProducerRejected(t *testing.T) {
	if _, err := publish.NewPublisher(nil); err == nil {
		t.Fatal("expected error for nil producer")
	}
}

func TestEmitExposure_PublishesValidEnvelope(t *testing.T) {
	pub, cc := newPublisher(t)
	es := domain.NewExposureSet("PORT-1", baseTime, []domain.Exposure{
		{Dimension: domain.ExposureByInstrument, Key: "AAPL", Gross: money(1000, 0, "USD"), Net: money(1000, 0, "USD")},
		{Dimension: domain.ExposureByCurrency, Key: "USD", Gross: money(1000, 0, "USD"), Net: money(1000, 0, "USD")},
	})

	if err := pub.EmitExposure(context.Background(), es); err != nil {
		t.Fatalf("EmitExposure: %v", err)
	}
	if got := len(cc.sent); got != 1 {
		t.Fatalf("messages=%d want 1", got)
	}
	msg := cc.sent[0]
	if msg.Subject != publish.EventTypeExposureRecomputed {
		t.Errorf("Subject=%q want %q", msg.Subject, publish.EventTypeExposureRecomputed)
	}
	if string(msg.Key) != "PORT-1" {
		t.Errorf("Key=%q want PORT-1", msg.Key)
	}

	env, payloadBytes, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("emitted envelope fails Validate: %v", err)
	}
	if env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("EventClass=%v want FACT", env.EventClass)
	}
	if env.Domain != "risk" {
		t.Errorf("Domain=%q want risk", env.Domain)
	}
	if env.PartitionKey != "PORT-1" {
		t.Errorf("PartitionKey=%q want PORT-1", env.PartitionKey)
	}
	// FACT contract: idempotency_key == event_id.
	if env.IdempotencyKey != env.EventId {
		t.Errorf("IdempotencyKey=%q want event_id=%q (FACT)", env.IdempotencyKey, env.EventId)
	}

	// Payload round-trip.
	var got domainpb.ExposureSet
	if err := proto.Unmarshal(payloadBytes, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if got.PortfolioId != "PORT-1" {
		t.Errorf("payload.PortfolioId=%q", got.PortfolioId)
	}
	if len(got.Exposures) != 2 {
		t.Fatalf("payload.Exposures=%d want 2", len(got.Exposures))
	}
}

func TestEmitExposure_DimensionMappingCoversInstrumentAndCurrency(t *testing.T) {
	pub, cc := newPublisher(t)
	es := domain.NewExposureSet("PORT-1", baseTime, []domain.Exposure{
		{Dimension: domain.ExposureByInstrument, Key: "AAPL", Gross: money(100, 0, "USD"), Net: money(100, 0, "USD")},
		{Dimension: domain.ExposureByCurrency, Key: "USD", Gross: money(100, 0, "USD"), Net: money(100, 0, "USD")},
		{Dimension: domain.ExposureBySector, Key: "TECH", Gross: money(100, 0, "USD"), Net: money(100, 0, "USD")},
	})
	if err := pub.EmitExposure(context.Background(), es); err != nil {
		t.Fatalf("EmitExposure: %v", err)
	}
	_, payloadBytes, _ := bus.Unframe(cc.sent[0].Body)
	var got domainpb.ExposureSet
	if err := proto.Unmarshal(payloadBytes, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantDim := map[string]domainpb.ExposureDimension{
		"AAPL": domainpb.ExposureDimension_EXPOSURE_DIMENSION_INSTRUMENT,
		"USD":  domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY,
		"TECH": domainpb.ExposureDimension_EXPOSURE_DIMENSION_SECTOR,
	}
	for _, e := range got.Exposures {
		if e.Dimension != wantDim[e.Bucket] {
			t.Errorf("bucket=%q dimension=%v want %v", e.Bucket, e.Dimension, wantDim[e.Bucket])
		}
	}
}

func TestEmitExposure_NilExposureRejected(t *testing.T) {
	pub, _ := newPublisher(t)
	if err := pub.EmitExposure(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil exposure")
	}
}

func TestEmitMeasures_PublishesValidEnvelope(t *testing.T) {
	pub, cc := newPublisher(t)
	ms := domain.NewMeasureSet("PORT-1", baseTime, map[v1.MeasureName]v1.Measure{
		"VaR99": {
			Name:           "VaR99",
			Value:          &commonpb.Decimal{Coefficient: 100, Exponent: 0},
			UncertaintyAbs: &commonpb.Decimal{Coefficient: 5, Exponent: 0},
		},
		"Delta": {
			Name:  "Delta",
			Value: &commonpb.Decimal{Coefficient: 1000, Exponent: 0},
		},
	})
	sourceIDs := []string{"src-evt-1", "src-evt-2", "src-evt-3"}

	if err := pub.EmitMeasures(context.Background(), ms, sourceIDs); err != nil {
		t.Fatalf("EmitMeasures: %v", err)
	}
	msg := cc.sent[0]
	if msg.Subject != publish.EventTypeMeasuresComputed {
		t.Errorf("Subject=%q want %q", msg.Subject, publish.EventTypeMeasuresComputed)
	}
	env, payloadBytes, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("emitted envelope fails Validate: %v", err)
	}

	var got domainpb.RiskMeasureSet
	if err := proto.Unmarshal(payloadBytes, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if got.PortfolioId != "PORT-1" {
		t.Errorf("payload.PortfolioId=%q", got.PortfolioId)
	}
	if len(got.Measures) != 2 {
		t.Fatalf("payload.Measures=%d want 2", len(got.Measures))
	}
	for _, m := range got.Measures {
		if len(m.SourceEventIds) != len(sourceIDs) {
			t.Errorf("measure %q SourceEventIds=%d want %d", m.Name, len(m.SourceEventIds), len(sourceIDs))
		}
		if m.Name == "VaR99" {
			if m.UncertaintyAbs == nil || m.UncertaintyAbs.Coefficient != 5 {
				t.Errorf("VaR99 UncertaintyAbs=%v want coef 5", m.UncertaintyAbs)
			}
		}
		if m.Name == "Delta" {
			if m.UncertaintyAbs != nil {
				t.Errorf("Delta UncertaintyAbs=%v want nil (no propagation signal)", m.UncertaintyAbs)
			}
		}
	}
}

func TestEmitMeasures_NilMeasuresRejected(t *testing.T) {
	pub, _ := newPublisher(t)
	if err := pub.EmitMeasures(context.Background(), nil, nil); err == nil {
		t.Fatal("expected error for nil measures")
	}
}

func TestEmit_PartitionKeyIsPortfolioID(t *testing.T) {
	// Per event-class-rules §1, per-aggregate ordering keys on
	// partition_key. For risk outputs that aggregate IS the
	// portfolio — partition_key must always be portfolio_id so the
	// per-portfolio ordering guarantee holds end-to-end (ingest →
	// state → compute → publish → consumer).
	pub, cc := newPublisher(t)
	es := domain.NewExposureSet("PORT-XYZ", baseTime, []domain.Exposure{
		{Dimension: domain.ExposureByCurrency, Key: "USD", Gross: money(1, 0, "USD"), Net: money(1, 0, "USD")},
	})
	if err := pub.EmitExposure(context.Background(), es); err != nil {
		t.Fatalf("EmitExposure: %v", err)
	}
	if string(cc.sent[0].Key) != "PORT-XYZ" {
		t.Errorf("Key=%q want PORT-XYZ", cc.sent[0].Key)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.PartitionKey != "PORT-XYZ" {
		t.Errorf("envelope.PartitionKey=%q want PORT-XYZ", env.PartitionKey)
	}
}

