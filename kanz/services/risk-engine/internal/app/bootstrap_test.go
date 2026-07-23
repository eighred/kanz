package app

// Internal (package app) test: the replay-source seam (Bootstrap.newSource)
// is unexported, so injecting an in-memory log without Kafka requires being
// inside the package. This is where the epic's idempotent-replay property is
// proven on every run — the postgres_test.go integration tests are gated on a
// live database and skip in normal CI, so the "re-applied events don't
// double-count" guarantee would otherwise go untested.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	risk "github.com/kanz-eng/kanz/internal/risk"
	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/engine"
	"github.com/kanz-eng/kanz/internal/risk/ingest"
	"github.com/kanz-eng/kanz/internal/risk/publish"
	"github.com/kanz-eng/kanz/internal/risk/state"
	"github.com/kanz-eng/kanz/internal/risk/state/persist"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/tools/replay"
)

// --- fakes ------------------------------------------------------------

// memSink is an in-memory persist.StateStore for the bootstrap tests.
type memSink struct {
	recs    []persist.PortfolioRecord
	loadErr error
}

func (m *memSink) Save(context.Context, persist.PortfolioRecord) error { return nil }
func (m *memSink) Load(context.Context, v1.PortfolioID) (persist.PortfolioRecord, error) {
	return persist.PortfolioRecord{}, persist.ErrNotFound
}
func (m *memSink) LoadAll(context.Context) ([]persist.PortfolioRecord, error) {
	return m.recs, m.loadErr
}
func (m *memSink) Ping(context.Context) error { return nil }

// srcItem is one (event, error) the fake source yields; err lets a test
// inject a malformed-frame or fatal error mid-stream.
type srcItem struct {
	ev  replay.Event
	err error
}

type fakeSource struct {
	items  []srcItem
	i      int
	closed bool
}

func (f *fakeSource) Next(context.Context) (replay.Event, error) {
	if f.i >= len(f.items) {
		return replay.Event{}, io.EOF
	}
	it := f.items[f.i]
	f.i++
	return it.ev, it.err
}
func (f *fakeSource) Close() error { f.closed = true; return nil }

// --- builders ---------------------------------------------------------

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func money(coef int64, ccy string) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: coef}, CurrencyCode: ccy}
}

// posEvent builds a replayable position-changed event for (port, instr).
func posEvent(key, port, instr string, mv int64, asOf time.Time, partition int, offset int64) replay.Event {
	payload, _ := proto.Marshal(&domainpb.PositionState{
		PortfolioId:  port,
		InstrumentId: instr,
		MarketValue:  money(mv, "USD"),
		AsOf:         timestamppb.New(asOf),
	})
	return replay.Event{
		Envelope: &envelopepb.Envelope{
			EventType:      ingest.EventTypePositionChanged,
			IdempotencyKey: key,
			PartitionKey:   port,
		},
		Payload:   payload,
		Topic:     ingest.EventTypePositionChanged,
		Partition: partition,
		Offset:    offset,
	}
}

// recWithPosition builds a durable record at MarketValue mv, with the given
// applied-key tail and a LogPosition so the replay phase has a resume point.
func recWithPosition(mv int64, asOf time.Time, appliedKeys []string, offset uint64) persist.PortfolioRecord {
	return persist.PortfolioRecord{
		ID:           "PORT-1",
		BaseCurrency: "USD",
		AsOf:         asOf,
		Positions: []domain.Position{
			{InstrumentID: "AAPL", MarketValue: money(mv, "USD"), AsOf: asOf},
		},
		AppliedKeys: appliedKeys,
		LogPosition: &commonpb.LogPosition{
			Topic:     ingest.EventTypePositionChanged,
			Partition: 0,
			Offset:    offset,
		},
	}
}

func newBoot(t *testing.T, store *state.Store, sink persist.StateStore, brokers []string, src *fakeSource, called *bool) *Bootstrap {
	t.Helper()
	b, err := NewBootstrap(store, sink, store, brokers, quiet())
	if err != nil {
		t.Fatalf("NewBootstrap: %v", err)
	}
	b.now = func() time.Time { return time.Unix(0, 0) }
	b.newSource = func(replay.Config) (eventSource, error) {
		if called != nil {
			*called = true
		}
		return src, nil
	}
	return b
}

// --- tests ------------------------------------------------------------

// Restore rehydrates aggregate state + positions from the durable store.
func TestBootstrap_RestoreRehydratesState(t *testing.T) {
	store := state.NewStore()
	sink := &memSink{recs: []persist.PortfolioRecord{recWithPosition(1000, time.Unix(100, 0), nil, 5)}}
	// No brokers ⇒ replay skipped; we are testing restore alone.
	b := newBoot(t, store, sink, nil, &fakeSource{}, nil)

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p, ok := store.Lookup("PORT-1")
	if !ok {
		t.Fatal("portfolio not restored")
	}
	pos, ok := p.Position("AAPL")
	if !ok || pos.MarketValue.GetAmount().GetCoefficient() != 1000 {
		t.Fatalf("position not restored: %+v ok=%v", pos.MarketValue, ok)
	}
}

// The core epic property: an event already folded into the snapshot — whose
// idempotency key is in the persisted AppliedKeys — is recognized by the
// seeded dedup window and skipped on replay, so re-applying a SUPERSEDED
// older event cannot regress (double-count) live state.
func TestBootstrap_ReplayIdempotent_NoRegress(t *testing.T) {
	store := state.NewStore()
	// Restored state is the newer value (2000) with both events' keys applied.
	rec := recWithPosition(2000, time.Unix(200, 0), []string{"key-t1", "key-t2"}, 5)
	sink := &memSink{recs: []persist.PortfolioRecord{rec}}

	// Replay re-delivers the OLDER event (key-t1, value 1000). Without the
	// seeded dedup it would overwrite AAPL back to 1000.
	src := &fakeSource{items: []srcItem{
		{ev: posEvent("key-t1", "PORT-1", "AAPL", 1000, time.Unix(100, 0), 0, 6)},
	}}
	b := newBoot(t, store, sink, []string{"broker:9092"}, src, nil)

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p, _ := store.Lookup("PORT-1")
	pos, _ := p.Position("AAPL")
	if got := pos.MarketValue.GetAmount().GetCoefficient(); got != 2000 {
		t.Fatalf("replayed superseded event regressed state: AAPL=%d want 2000", got)
	}
	if !src.closed {
		t.Error("replay source not closed")
	}
}

// A genuinely-new event (key not in AppliedKeys) IS applied on replay — the
// gap between the snapshot and the crash is recovered.
func TestBootstrap_ReplayAppliesNewEvents(t *testing.T) {
	store := state.NewStore()
	rec := recWithPosition(1000, time.Unix(100, 0), []string{"key-t1"}, 5)
	sink := &memSink{recs: []persist.PortfolioRecord{rec}}

	src := &fakeSource{items: []srcItem{
		{ev: posEvent("key-t2", "PORT-1", "AAPL", 3000, time.Unix(300, 0), 0, 6)},
	}}
	b := newBoot(t, store, sink, []string{"broker:9092"}, src, nil)

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p, _ := store.Lookup("PORT-1")
	pos, _ := p.Position("AAPL")
	if got := pos.MarketValue.GetAmount().GetCoefficient(); got != 3000 {
		t.Fatalf("new replayed event not applied: AAPL=%d want 3000", got)
	}
}

// A malformed frame mid-stream is logged and skipped; replay continues and
// Run succeeds.
func TestBootstrap_MalformedFrameSkipped(t *testing.T) {
	store := state.NewStore()
	rec := recWithPosition(1000, time.Unix(100, 0), nil, 5)
	sink := &memSink{recs: []persist.PortfolioRecord{rec}}

	src := &fakeSource{items: []srcItem{
		{err: &replay.MalformedFrameError{Topic: ingest.EventTypePositionChanged, Partition: 0, Offset: 6, Err: errors.New("bad frame")}},
		{ev: posEvent("key-new", "PORT-1", "AAPL", 4000, time.Unix(400, 0), 0, 7)},
	}}
	b := newBoot(t, store, sink, []string{"broker:9092"}, src, nil)

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p, _ := store.Lookup("PORT-1")
	pos, _ := p.Position("AAPL")
	if got := pos.MarketValue.GetAmount().GetCoefficient(); got != 4000 {
		t.Fatalf("replay did not continue past malformed frame: AAPL=%d want 4000", got)
	}
}

// A non-malformed source error aborts the replay (Run returns it).
func TestBootstrap_SourceErrorAborts(t *testing.T) {
	store := state.NewStore()
	sink := &memSink{recs: []persist.PortfolioRecord{recWithPosition(1000, time.Unix(100, 0), nil, 5)}}
	src := &fakeSource{items: []srcItem{{err: errors.New("kafka fetch failed")}}}
	b := newBoot(t, store, sink, []string{"broker:9092"}, src, nil)

	if err := b.Run(context.Background()); err == nil {
		t.Fatal("expected source error to abort Run")
	}
}

// No durable resume position ⇒ replay is skipped (relies on the live spine),
// the source factory is never invoked, and restore still happened.
func TestBootstrap_NoResumePositionSkipsReplay(t *testing.T) {
	store := state.NewStore()
	rec := persist.PortfolioRecord{
		ID: "PORT-1", BaseCurrency: "USD", AsOf: time.Unix(100, 0),
		Positions: []domain.Position{{InstrumentID: "AAPL", MarketValue: money(1000, "USD"), AsOf: time.Unix(100, 0)}},
		// LogPosition nil ⇒ no resume key.
	}
	sink := &memSink{recs: []persist.PortfolioRecord{rec}}
	var called bool
	b := newBoot(t, store, sink, []string{"broker:9092"}, &fakeSource{}, &called)

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Error("replay source created despite no resume position")
	}
	if _, ok := store.Lookup("PORT-1"); !ok {
		t.Error("restore did not run")
	}
}

// A LoadAll failure aborts bootstrap — we must not run on partial state.
func TestBootstrap_LoadAllErrorAborts(t *testing.T) {
	store := state.NewStore()
	sink := &memSink{loadErr: errors.New("db down")}
	b := newBoot(t, store, sink, nil, &fakeSource{}, nil)

	if err := b.Run(context.Background()); err == nil {
		t.Fatal("expected LoadAll error to abort Run")
	}
}

// Restore must never clobber state already present (a snapshot is never newer
// than the running engine).
func TestBootstrap_RestoreDoesNotClobberLiveState(t *testing.T) {
	store := state.NewStore()
	// Seed a live newer value via the apply path.
	live := &domainpb.PositionState{
		PortfolioId: "PORT-1", InstrumentId: "AAPL",
		MarketValue: money(9000, "USD"), AsOf: timestamppb.New(time.Unix(500, 0)),
	}
	if err := store.ApplyPositionChanged(context.Background(),
		&envelopepb.Envelope{EventType: ingest.EventTypePositionChanged, IdempotencyKey: "live", PartitionKey: "PORT-1"},
		live); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	sink := &memSink{recs: []persist.PortfolioRecord{recWithPosition(1000, time.Unix(100, 0), nil, 5)}}
	b := newBoot(t, store, sink, nil, &fakeSource{}, nil)

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p, _ := store.Lookup("PORT-1")
	pos, _ := p.Position("AAPL")
	if got := pos.MarketValue.GetAmount().GetCoefficient(); got != 9000 {
		t.Fatalf("restore clobbered live state: AAPL=%d want 9000", got)
	}
}

// --- FINDING 1: post-bootstrap recompute arming ------------------------
//
// Bootstrap.Run repopulates STATE (restore + replay) but never triggers a
// recompute — by design, so replay never emits a per-event storm of
// superseded risk FACTs (see Bootstrap's doc comment). That means the
// pushed FACT stream can otherwise go quiet after a crash: the bus already
// acked the event that led to the lost recompute (Ingestor.Handler returns
// nil the instant the bare-store apply commits), and nothing regenerates
// its FACT until the portfolio's next live event. ArmPostBootstrapRecomputes
// (called by main.go right after boot.Run succeeds) closes that gap.

// capturePublisher is a bus.Client that counts publishes by subject — enough
// to prove a REAL recompute ran (which emits FACTs), not just that some
// internal counter was incremented.
type capturePublisher struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCapturePublisher() *capturePublisher { return &capturePublisher{counts: map[string]int{}} }

func (c *capturePublisher) Publish(_ context.Context, msg bus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[msg.Subject]++
	return nil
}
func (c *capturePublisher) Subscribe(context.Context, string, string, bus.Handler) error { return nil }
func (c *capturePublisher) Close() error                                                 { return nil }

func (c *capturePublisher) count(subject string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[subject]
}

// TestArmPostBootstrapRecomputes_FiresOnePerRestoredPortfolio proves the
// fix: after a bootstrap-shaped restore, arming triggers a real recompute —
// observed as an emitted exposure FACT — for the restored portfolio. A long
// debounce plus Drain (rather than a fixed sleep) proves the recompute was
// actually ARMED by this call, not merely due to fire on its own.
func TestArmPostBootstrapRecomputes_FiresOnePerRestoredPortfolio(t *testing.T) {
	store := state.NewStore()
	sink := &memSink{recs: []persist.PortfolioRecord{recWithPosition(1000, time.Unix(100, 0), nil, 5)}}
	// No brokers ⇒ replay skipped; this test is about arming after restore,
	// which is exactly what Bootstrap.Run leaves in the store either way.
	b := newBoot(t, store, sink, nil, &fakeSource{}, nil)
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cc := newCapturePublisher()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "test/bootstrap", ProducerVersion: "v0", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := publish.NewPublisher(prod)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	// A debounce far longer than this test's lifetime: if arming didn't
	// really schedule the recompute, nothing would ever fire without Drain.
	rec := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(), pub, 10*time.Second, quiet())
	defer rec.Close()

	armed := ArmPostBootstrapRecomputes(store, rec, quiet())
	if armed != 1 {
		t.Fatalf("armed = %d want 1 (one restored portfolio)", armed)
	}

	rec.Drain() // flush the armed recompute now, rather than waiting 10s

	if got := cc.count(publish.EventTypeExposureRecomputed); got != 1 {
		t.Errorf("exposure FACTs = %d want 1 (post-bootstrap arming never fired a recompute)", got)
	}
}

// No restored portfolios ⇒ nothing armed, and Drain emits nothing.
func TestArmPostBootstrapRecomputes_NoPortfoliosArmsNothing(t *testing.T) {
	store := state.NewStore() // empty — nothing restored
	cc := newCapturePublisher()
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "test/bootstrap", ProducerVersion: "v0", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := publish.NewPublisher(prod)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	rec := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), risk.NewCache(), pub, 10*time.Second, quiet())
	defer rec.Close()

	if armed := ArmPostBootstrapRecomputes(store, rec, quiet()); armed != 0 {
		t.Fatalf("armed = %d want 0 (empty store)", armed)
	}
	rec.Drain()
	if got := cc.count(publish.EventTypeExposureRecomputed); got != 0 {
		t.Errorf("exposure FACTs = %d want 0 (nothing was restored to arm)", got)
	}
}
