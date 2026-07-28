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
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/platform/subject"
	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk-engine/internal/app"
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

// subjectMatches is NATS subject matching: tokens split on `.`, `*` matches exactly one
// token, `>` matches one or more trailing tokens.
//
// The fake used to compare subjects with `==`, which a real broker does not do — and the
// moment the risk engine bound a WILDCARD (risk.position.>, EXEC-M20) the fake silently
// delivered nothing while the broker would have delivered everything. A fake that cannot
// match the way the broker matches is a fake that cannot fail the way the broker fails.
func subjectMatches(pattern, subj string) bool {
	p := strings.Split(pattern, ".")
	s := strings.Split(subj, ".")
	for i, tok := range p {
		if tok == ">" {
			return i < len(s) // `>` needs at least one token to swallow
		}
		if i >= len(s) {
			return false
		}
		if tok != "*" && tok != s[i] {
			return false
		}
	}
	return len(p) == len(s)
}

func (m *memBus) Publish(ctx context.Context, msg bus.Message) error {
	m.mu.Lock()
	var chans []chan bus.Message
	for pattern, cs := range m.subs {
		if subjectMatches(pattern, msg.Subject) {
			chans = append(chans, cs...)
		}
	}
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

// inputSubjects are the three risk state subjects the engine subscribes. The position
// one is the WILDCARD (EXEC-M20): a holding rides one subject per (tenant, portfolio,
// instrument) on a compacted stream, so the engine binds risk.position.> — and this list
// must say what the engine actually binds, or waitSubscribed waits for a subscription
// nobody ever makes.
var inputSubjects = []string{
	ingest.EventTypePortfolioRevalued,
	subject.PositionAll,
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
	// subj and eventType are SEPARATE now (EXEC-M20): a position rides one subject per
	// holding, while its event_type stays the taxonomy name. Publishing the flat subject
	// here would test a producer that no longer exists — the OMS emits
	// risk.position.changed.<tenant>.<portfolio>.<instrument>, and the engine binds the
	// wildcard over it.
	emit := func(subj, eventType, schemaRef string, payload proto.Message) {
		if err := prod.Publish(context.Background(), bus.Event{
			Subject:          subj,
			EventType:        eventType,
			EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion:    1,
			Domain:           "risk",
			EventTime:        asOf,
			PartitionKey:     pk,
			PayloadSchemaRef: schemaRef,
			Payload:          payload,
		}); err != nil {
			t.Fatalf("publish %s: %v", subj, err)
		}
	}
	emit(ingest.EventTypePortfolioRevalued, ingest.EventTypePortfolioRevalued, "domain.v1.PortfolioState:1", &domainpb.PortfolioState{
		PortfolioId: pk, BaseCurrency: "USD", AsOf: timestamppb.New(asOf),
	})
	emit(subject.PositionFor("acme", pk, "AAPL"), ingest.EventTypePositionChanged, "domain.v1.PositionState:1", &domainpb.PositionState{
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
