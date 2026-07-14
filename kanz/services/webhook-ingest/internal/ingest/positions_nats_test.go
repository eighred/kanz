package ingest

// The EXEC-M19b payoff, over the REAL bus.
//
// positions_test.go proves the cache folds FACTs correctly. This proves the part that can
// only lie in production: that a COLD-BOOTING POD actually learns the fund's book from the
// compacted POSITION stream — the subject really is bound to the stream, the replay really
// does deliver the current state of every holding, and the ready signal really does fire
// once it has drained. Get any of those wrong and webhook-ingest boots with an empty cache,
// which is precisely the bug EXEC-M19 exists to kill.
//
// Gated on TEST_NATS_URL alone: it publishes its own FACTs, so it needs no OMS.

import (
	"context"
	"math/big"
	"os"
	"testing"
	"time"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/dec"
	"github.com/kanz-eng/kanz/internal/platform/subject"
	"github.com/kanz-eng/kanz/pkg/bus"
)

func TestIntegration_ABootingPodLearnsTheBookFromTheCompactedStream(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL (a bootstrapped NATS with the POSITION stream) to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const tenant, fund, instrument = "acme", "fund-alpha", "BTC-USD"

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "ingest-positions-it"})
	if err != nil {
		t.Fatalf("dial NATS: %v", err)
	}
	defer client.Close()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "ingest-positions-it",
		ProducerVersion: "test",
		Tenant:          tenant,
	})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	// The OMS publishes the fund's per-venue holdings BEFORE this pod exists — as it would
	// have while the pod was being rolled. 0.6 BTC at BINANCE, 0.4 at OKX.
	//
	// The OKX holding is published TWICE: 9 BTC, then corrected to 0.4. The stream is
	// compacted per subject, so a booting pod must learn 0.4 — the CURRENT state — and never
	// the superseded 9. Folding history instead of state is how a CLOSE oversells.
	for _, p := range []struct {
		venue string
		qty   *big.Rat
	}{
		{"BINANCE", big.NewRat(6, 10)},
		{"OKX", big.NewRat(9, 1)},
		{"OKX", big.NewRat(4, 10)},
	} {
		st := &domainpb.PositionState{
			PortfolioId:  fund,
			Venue:        p.venue,
			InstrumentId: instrument,
			Quantity:     dec.ToProto(p.qty),
			AsOf:         timestamppb.New(time.Now().UTC()),
		}
		if err := producer.Publish(ctx, bus.Event{
			Subject:          subject.VenuePositionFor(tenant, fund, p.venue, instrument),
			EventType:        subject.VenuePositionChanged,
			EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion:    1,
			Domain:           "risk",
			EventTime:        st.GetAsOf().AsTime(),
			PartitionKey:     fund,
			PayloadSchemaRef: "domain.v1.PositionState:1",
			Payload:          st,
		}); err != nil {
			// A hard error here means the subject is not bound to any stream — exactly the
			// failure that would leave production's cache empty forever.
			t.Fatalf("publish %s: %v — is subject.VenuePositionAll bound to the POSITION stream?", p.venue, err)
		}
	}

	// NOW the pod boots. It has seen nothing; it learns the book entirely from the replay.
	cache := NewPositionCache()
	if cache.Armed() {
		t.Fatal("a cache reported armed before it had read anything")
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	go func() {
		_ = consumer.SubscribeBroadcastReady(ctx, subject.VenuePositionAll, cache.Handle, cache.Arm)
	}()

	deadline := time.Now().Add(20 * time.Second)
	for !cache.Armed() {
		if time.Now().After(deadline) {
			t.Fatal("the book never armed — a booting pod would serve CLOSE signals against an empty cache, or never report ready at all")
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, want := range []struct {
		venue string
		qty   *big.Rat
	}{
		{"BINANCE", big.NewRat(6, 10)},
		{"OKX", big.NewRat(4, 10)}, // the CURRENT state, not the superseded 9
	} {
		got, err := cache.Position(ctx, fund, want.venue, instrument)
		if err != nil {
			t.Fatalf("%s: %v", want.venue, err)
		}
		if got.Cmp(want.qty) != 0 {
			t.Errorf("%s holds %s, want %s", want.venue, got.RatString(), want.qty.RatString())
		}
	}
}
