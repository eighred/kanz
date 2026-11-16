package bustest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/platform/subject"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReclaimPreservesAnUnrelatedCanonicalPosition(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL for actual canonical POSITION cleanup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "POSITION")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.Config.MaxAge != 0 || info.Config.MaxMsgsPerSubject != 1 {
		t.Fatalf("requires canonical POSITION compaction: %+v %v", info, err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "position-reclaim-control"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tenant, portfolio := "reclaim-"+uuid.NewString(), "fixture"
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "bustest/reclaim", ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	foreign := subject.PositionFor(tenant, "foreign", "sentinel")
	owned := subject.PositionFor(tenant, portfolio, "owned")
	bustest.ReclaimSubjects(t, js, foreign)
	publish := func(t *testing.T, topic, fund, instrument string) {
		t.Helper()
		now := time.Now().UTC()
		if err := producer.Publish(ctx, bus.Event{Subject: topic, EventType: subject.PositionChanged,
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "risk",
			EventTime: now, PartitionKey: topic, PayloadSchemaRef: "domain.v1.PositionState:1",
			Payload: &domainpb.PositionState{PortfolioId: fund, InstrumentId: instrument,
				Quantity: &commonpb.Decimal{Coefficient: 7}, AsOf: timestamppb.New(now)}}); err != nil {
			t.Fatal(err)
		}
	}
	publish(t, foreign, "foreign", "sentinel")
	before, err := stream.GetLastMsgForSubject(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if !t.Run("owned", func(t *testing.T) {
		bustest.ReclaimSubjects(t, js, owned)
		publish(t, owned, portfolio, "owned")
	}) {
		t.Fatal("owned fixture failed")
	}
	if _, err := stream.GetLastMsgForSubject(ctx, owned); !errors.Is(err, jetstream.ErrMsgNotFound) {
		t.Fatalf("owned subject survived cleanup: %v", err)
	}
	after, err := stream.GetLastMsgForSubject(ctx, foreign)
	if err != nil || after.Sequence != before.Sequence || !bytes.Equal(after.Data, before.Data) {
		t.Fatalf("unrelated canonical sentinel was removed or changed: %v", err)
	}
	env, _, err := bus.Unframe(after.Data)
	if err != nil || bus.Validate(env) != nil {
		t.Fatal("sentinel is not a canonical framed FACT")
	}
}
