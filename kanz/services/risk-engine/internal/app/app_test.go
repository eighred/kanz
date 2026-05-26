package app_test

// End-to-end harness for the ORCH-01 runtime over an in-memory bus.Client
// (memBus): publish state events, let the ingest→recompute→publish pipeline
// react, and assert the emitted risk FACTs + a latency budget + the
// degraded-on-stale query path. This is the composition the individual
// ORCH-01b/c/d/e unit tests exercise in isolation, proven against a single
// looping broker so a regression in any seam surfaces here.

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	risk "github.com/kanz-eng/kanz/internal/risk"
	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/engine"
	"github.com/kanz-eng/kanz/internal/risk/ingest"
	"github.com/kanz-eng/kanz/internal/risk/publish"
	"github.com/kanz-eng/kanz/internal/risk/state"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/app"
)

// memBus is an in-memory bus.Client that routes every Publish to the
// handlers Subscribed on that subject — a looping broker so the engine's
// own output (published by its recomputer) can be observed by a test
// subscriber on the same client. Delivery is async per subscription
// (buffered channel) so a handler that republishes (apply → recompute →
// publish) cannot deadlock the publisher.
type memBus struct {
	mu   sync.Mutex
	subs map[string][]chan bus.Message
}

func newMemBus() *memBus { return &memBus{subs: map[string][]chan bus.Message{}} }

func (m *memBus) Publish(ctx context.Context, msg bus.Message) error {
	m.mu.Lock()
	chans := append([]chan bus.Message(nil), m.subs[msg.Subject]...)
	m.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- msg:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *memBus) Subscribe(ctx context.Context, subject, _ string, h bus.Handler) error {
	ch := make(chan bus.Message, 1024)
	m.mu.Lock()
	m.subs[subject] = append(m.subs[subject], ch)
	m.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-ch:
			_ = h(ctx, msg)
		}
	}
}

func (m *memBus) Close() error { return nil }

func (m *memBus) subscriberCount(subject string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs[subject])
}

// inputSubjects are the three risk state subjects the engine subscribes.
var inputSubjects = []string{
	ingest.EventTypePortfolioRevalued,
	ingest.EventTypePositionChanged,
	ingest.EventTypePortfolioSnapshot,
}

// waitSubscribed blocks until every subject has at least one subscriber.
// memBus only delivers to subscribers present at Publish time (no retention),
// so a publish before the async Subscribe goroutines register would be lost.
func waitSubscribed(t *testing.T, broker *memBus, subjects ...string) {
	t.Helper()
	waitFor(t, func() bool {
		for _, s := range subjects {
			if broker.subscriberCount(s) == 0 {
				return false
			}
		}
		return true
	})
}

// startPipeline wires store + recomputer + ingest over the broker and runs
// ingestion until the test ends. Returns the store (for query-side checks)
// and the recomputer.
func startPipeline(t *testing.T, broker *memBus, debounce time.Duration) (*state.Store, *engine.Recomputer) {
	t.Helper()
	prod, err := bus.NewProducer(broker, bus.ProducerConfig{Source: "risk-engine/e2e", ProducerVersion: "v0", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := publish.NewPublisher(prod)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	store := state.NewStore()
	rec := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(), pub, debounce, discardLogger())
	consumer, err := bus.NewConsumer(broker)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	ing, err := app.NewIngest(consumer, engine.NewTriggeringApplier(store, rec), "", discardLogger())
	if err != nil {
		t.Fatalf("NewIngest: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = ing.Run(ctx) }()
	t.Cleanup(func() { cancel(); rec.Close() })
	return store, rec
}

// subscribeOutput observes an output subject, delivering the unframed
// (envelope, payload) of each emitted FACT to onMsg.
func subscribeOutput(t *testing.T, broker *memBus, subject string, onMsg func(*envelopepb.Envelope, []byte)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = broker.Subscribe(ctx, subject, "e2e-observer", func(_ context.Context, msg bus.Message) error {
			env, payload, err := bus.Unframe(msg.Body)
			if err != nil {
				return err
			}
			onMsg(env, payload)
			return nil
		})
	}()
}

// publishState publishes a portfolio-revalued + position-changed pair for
// one portfolio through the broker, mirroring a real upstream producer.
func publishState(t *testing.T, prod *bus.Producer, pk string, asOf time.Time, marketValue int64) {
	t.Helper()
	emit := func(subject, schemaRef string, payload proto.Message) {
		if err := prod.Publish(context.Background(), bus.Event{
			Subject:          subject,
			EventType:        subject,
			EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion:    1,
			Domain:           "risk",
			EventTime:        asOf,
			PartitionKey:     pk,
			PayloadSchemaRef: schemaRef,
			Payload:          payload,
		}); err != nil {
			t.Fatalf("publish %s: %v", subject, err)
		}
	}
	emit(ingest.EventTypePortfolioRevalued, "domain.v1.PortfolioState:1", &domainpb.PortfolioState{
		PortfolioId: pk, BaseCurrency: "USD", AsOf: timestamppb.New(asOf),
	})
	emit(ingest.EventTypePositionChanged, "domain.v1.PositionState:1", &domainpb.PositionState{
		PortfolioId: pk, InstrumentId: "AAPL", MarketValue: money(marketValue, "USD"), AsOf: timestamppb.New(asOf),
	})
}

func testProducer(t *testing.T, broker *memBus) *bus.Producer {
	t.Helper()
	prod, err := bus.NewProducer(broker, bus.ProducerConfig{Source: "e2e/upstream", ProducerVersion: "v0", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod
}

func TestE2E_PublishProducesOutputFacts(t *testing.T) {
	broker := newMemBus()
	startPipeline(t, broker, 5*time.Millisecond)

	var mu sync.Mutex
	var gotExposure bool
	var grossValue float64
	subscribeOutput(t, broker, publish.EventTypeExposureRecomputed, func(_ *envelopepb.Envelope, _ []byte) {
		mu.Lock()
		gotExposure = true
		mu.Unlock()
	})
	subscribeOutput(t, broker, publish.EventTypeMeasuresComputed, func(_ *envelopepb.Envelope, payload []byte) {
		var ms domainpb.RiskMeasureSet
		if err := proto.Unmarshal(payload, &ms); err != nil {
			t.Errorf("unmarshal RiskMeasureSet: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, m := range ms.Measures {
			if m.Name == string(compute.MeasureGrossExposure) {
				grossValue = float64(m.Value.GetCoefficient()) * pow10(m.Value.GetExponent())
			}
		}
	})

	waitSubscribed(t, broker, append(inputSubjects, publish.EventTypeExposureRecomputed, publish.EventTypeMeasuresComputed)...)
	publishState(t, testProducer(t, broker), "PORT-1", time.Now(), 1000)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotExposure && grossValue != 0
	})

	mu.Lock()
	defer mu.Unlock()
	if !gotExposure {
		t.Error("no exposure_recomputed FACT emitted")
	}
	if abs(grossValue-1000) > 0.001 {
		t.Errorf("GrossExposure = %v want 1000 (one USD position of 1000)", grossValue)
	}
}

func TestE2E_RecomputeLatencyP99(t *testing.T) {
	broker := newMemBus()
	startPipeline(t, broker, 5*time.Millisecond)

	const n = 100
	var mu sync.Mutex
	recvAt := make(map[string]time.Time, n)
	subscribeOutput(t, broker, publish.EventTypeExposureRecomputed, func(env *envelopepb.Envelope, _ []byte) {
		mu.Lock()
		if _, seen := recvAt[env.PartitionKey]; !seen {
			recvAt[env.PartitionKey] = time.Now()
		}
		mu.Unlock()
	})

	waitSubscribed(t, broker, append(inputSubjects, publish.EventTypeExposureRecomputed)...)
	prod := testProducer(t, broker)
	publishAt := make(map[string]time.Time, n)
	now := time.Now()
	for i := 0; i < n; i++ {
		pk := "PORT-" + itoa(i)
		publishState(t, prod, pk, now, int64(1000+i))
		publishAt[pk] = time.Now() // time of the last input for this portfolio
	}

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(recvAt) == n
	})

	mu.Lock()
	latencies := make([]time.Duration, 0, n)
	for pk, rt := range recvAt {
		latencies = append(latencies, rt.Sub(publishAt[pk]))
	}
	mu.Unlock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	p99 := latencies[int(float64(len(latencies))*0.99)]
	// Generous budget: in-memory + 5ms debounce should land well under this;
	// the assertion guards against an O(n) regression in the recompute path.
	const budget = 500 * time.Millisecond
	if p99 > budget {
		t.Errorf("recompute p99 latency = %v exceeds budget %v", p99, budget)
	}
}

// TestE2E_DegradedOnStaleQuery ingests stale state through the bus, then
// queries the EngineImpl (ORCH-01b) over the same store — the query path
// must flag DEGRADED because the state is older than the degraded threshold.
func TestE2E_DegradedOnStaleQuery(t *testing.T) {
	broker := newMemBus()
	store, _ := startPipeline(t, broker, 5*time.Millisecond)

	waitSubscribed(t, broker, inputSubjects...)
	publishState(t, testProducer(t, broker), "PORT-STALE", time.Now().Add(-10*time.Minute), 1000)
	waitFor(t, func() bool {
		p, ok := store.Snapshot("PORT-STALE")
		if !ok {
			return false
		}
		_, has := p.Position("AAPL")
		return has
	})

	eng := engine.New(store, compute.DefaultRegistry(), risk.NewCache(), risk.NewDetector())
	resp, err := eng.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-STALE"})
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if !hasFlag(resp.QualityFlags, v1.QualityFlagDegraded) {
		t.Errorf("flags=%v want to contain DEGRADED (10m-stale state)", resp.QualityFlags)
	}
}

// --- tiny local helpers (avoid extra imports / cross-file coupling) ----

func pow10(e int32) float64 {
	out := 1.0
	for i := int32(0); i < e; i++ {
		out *= 10
	}
	for i := int32(0); i > e; i-- {
		out /= 10
	}
	return out
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func hasFlag(flags []v1.QualityFlag, want v1.QualityFlag) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}
