package publish_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/volsurface"
	"github.com/eighred/kanz/internal/risk/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Only financial observations are fixtures; publication crosses real JetStream.
type invalidVolInputs struct{}

func (invalidVolInputs) Quotes(context.Context, string, time.Time) ([]volsurface.OptionQuote, float64, error) {
	return []volsurface.OptionQuote{{Type: pricing.Call, Strike: 100, Expiry: 1, Price: math.NaN()}}, 100, nil
}
func (invalidVolInputs) Spot(context.Context, string, time.Time) (float64, bool) { return 100, true }
func (invalidVolInputs) OptionTerms(_ context.Context, _ string, asOf time.Time) (compute.OptionSpec, bool) {
	return compute.OptionSpec{UnderlyingID: "UND", Strike: 100, Expiry: asOf.AddDate(1, 0, 0), Type: pricing.Call, Exercise: pricing.European, Multiplier: 1}, true
}

func TestInvalidVolCoverageSurvivesRealJetStream(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	bustest.EnsureSubjects(t, ctx, js, "INVALID_VOL_"+suffix, []string{publish.EventTypeMeasuresComputed})
	stream, err := bustest.StreamFor(ctx, js, publish.EventTypeMeasuresComputed)
	if err != nil {
		t.Fatal(err)
	}
	name := "invalid-vol-" + suffix
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: publish.EventTypeMeasuresComputed, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(context.Background(), stream, name) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "risk-engine", ProducerVersion: "test", Tenant: "invalid-vol-panel"})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := publish.NewPublisher(producer)
	if err != nil {
		t.Fatal(err)
	}

	st := volsurface.NewStore()
	cal := volsurface.Calibrator{Source: invalidVolInputs{}, Store: st, Disc: pricing.FlatCurve(0)}
	if _, err := cal.Refresh(ctx, "UND", baseTime); !errors.Is(err, volsurface.ErrInvalidInput) {
		t.Fatalf("bad calibration accepted: %v", err)
	}
	reg := compute.NewRegistry()
	compute.RegisterGreeks(ctx, reg, compute.GreeksProviders{Terms: invalidVolInputs{}, Spot: invalidVolInputs{}, Vol: st})
	portfolio := domain.NewPortfolio("invalid-vol", "USD")
	portfolio.SetPosition(domain.Position{InstrumentID: "OPT", AsOf: baseTime, Quantity: &commonpb.Decimal{Coefficient: 1}, MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 10}}})
	set := compute.ComputeMeasures(portfolio, reg, nil)
	if err := publisher.EmitMeasures(ctx, set, nil); err != nil {
		t.Fatal(err)
	}
	message, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	env, payload, err := bus.Unframe(message.Data())
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatal(err)
	}
	var record domainpb.RiskMeasureSet
	if err := proto.Unmarshal(payload, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Measures) != 5 {
		t.Fatalf("expected 5 Greeks, got %d", len(record.Measures))
	}
	for _, m := range record.Measures {
		coverage := m.GetCoverage()
		if m.GetValue().GetCoefficient() != 0 || coverage.GetContributed() != 0 || coverage.GetExcludedCount() != 1 || len(coverage.GetExclusions()) != 1 || coverage.GetExclusions()[0].GetReason() != compute.SkipNoVol {
			t.Fatalf("lost incomplete pricing coverage: %v", m)
		}
	}
	degraded := false
	for _, flag := range env.GetQualityFlags() {
		if flag == envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED {
			degraded = true
		}
	}
	if !degraded {
		t.Fatal("unpriceable book published as healthy")
	}
	if err := message.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
}
