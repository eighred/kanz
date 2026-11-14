package state_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/platform/subject"
	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func stateTimeSameRecord(t *testing.T, want, got persist.PortfolioRecord) {
	t.Helper()
	canonical := func(r persist.PortfolioRecord) []byte {
		// SQL stamps the authenticated tenant and orders the durable key set;
		// neither changes the logical snapshot restored into the hot store.
		r.TenantID = ""
		r.AppliedKeys = append([]string(nil), r.AppliedKeys...)
		sort.Strings(r.AppliedKeys)
		r.AsOf = r.AsOf.UTC()
		r.Positions = append(r.Positions[:0:0], r.Positions...)
		for i := range r.Positions {
			r.Positions[i].AsOf = r.Positions[i].AsOf.UTC()
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if a, b := canonical(want), canonical(got); !bytes.Equal(a, b) {
		t.Fatalf("snapshot changed across checkpoint/restore: want %s got %s", a, b)
	}
}

func TestStateTimeCanonicalJetStreamAndPostgresRecovery(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires TEST_NATS_URL JetStream and TEST_POSTGRES_URL restricted PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id := fmt.Sprintf("state-time-%d", time.Now().UnixNano())
	tenant, portfolio := id, "PF-"+id
	topic := subject.PositionFor(tenant, portfolio, "A")
	dlqTopic := "dlq." + topic
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	streamFor := func(topic string) jetstream.Stream {
		t.Helper()
		name, err := bustest.StreamFor(ctx, js, topic)
		if err != nil {
			t.Fatal(err)
		}
		s, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	bustest.EnsureSubjects(t, ctx, js, "STATE_TIME_"+strings.ReplaceAll(id, "-", "_"), []string{subject.PositionAll})
	bustest.EnsureSubjects(t, ctx, js, "STATE_TIME_DLQ_"+strings.ReplaceAll(id, "-", "_"), []string{dlqTopic})
	input, parked := streamFor(topic), streamFor(dlqTopic)
	if cfg := input.CachedInfo().Config; cfg.MaxMsgsPerSubject != 1 || cfg.MaxAge != 0 {
		t.Fatal("requires canonical compacted POSITION topology (one message per subject, no max age)")
	}
	// Registered before publishing; only the two unique fixture subjects are
	// reclaimed, including on a canonical never-ageing POSITION stream.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, entry := range []struct {
			s     jetstream.Stream
			topic string
		}{{input, topic}, {parked, dlqTopic}} {
			if strings.ContainsAny(entry.topic, "*>") || !strings.Contains(entry.topic, subject.Token(portfolio)) {
				t.Error("unsafe fixture reclaim")
				continue
			}
			if err := entry.s.Purge(cleanup, jetstream.WithPurgeSubject(entry.topic)); err != nil {
				t.Error(err)
			}
		}
	})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "state-time-test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	pool := pgPool(t, tenant)
	var superuser, bypass bool
	if err := pool.QueryRow(ctx, "SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&superuser, &bypass); err != nil {
		t.Fatal(err)
	}
	if superuser || bypass {
		t.Fatal("state persistence proof requires NOSUPERUSER NOBYPASSRLS")
	}
	applyStateSchema(t, pool)
	sink := persist.NewPostgres(pool)
	foreign := persist.NewPostgres(pgPool(t, tenant+"-foreign"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	checkpoint := func(s *state.Store) persist.PortfolioRecord {
		t.Helper()
		if err := engine.NewSnapshotter(s, sink, nil, time.Hour, logger).Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		r, err := sink.Load(ctx, v1.PortfolioID(portfolio))
		if err != nil {
			t.Fatal(err)
		}
		if r.TenantID != tenant {
			t.Fatal("checkpoint lost authenticated tenant")
		}
		if _, err := foreign.Load(ctx, v1.PortfolioID(portfolio)); !errors.Is(err, persist.ErrNotFound) {
			t.Fatalf("foreign tenant discovered checkpoint: %v", err)
		}
		stateTimeSameRecord(t, stateTimeRecord(t, s, portfolio), r)
		return r
	}
	restore := func(r persist.PortfolioRecord) *state.Store {
		t.Helper()
		s := state.NewStore()
		if err := s.Restore(r.ToPortfolio(), r.AppliedKeys); err != nil {
			t.Fatal(err)
		}
		stateTimeSameRecord(t, r, stateTimeRecord(t, s, portfolio))
		return s
	}
	type delivery struct {
		id  string
		err error
	}
	type session struct {
		seen  <-chan delivery
		c     jetstream.Consumer
		cache *risk.Cache
		stop  func()
	}
	start := func(s *state.Store, suffix string) session {
		t.Helper()
		cache := risk.NewCache()
		r := engine.NewRecomputer(ctx, s, compute.DefaultRegistry(), cache, nil, time.Hour, logger)
		t.Cleanup(r.Close)
		i, err := ingest.NewIngestor(engine.NewTriggeringApplier(s, r))
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
		if err != nil {
			t.Fatal(err)
		}
		subCtx, stop := context.WithCancel(ctx)
		t.Cleanup(stop)
		seen, ready, done := make(chan delivery, 8), make(chan struct{}), make(chan error, 1)
		group := id + suffix
		go func() {
			done <- consumer.SubscribeReady(subCtx, topic, group, func(hctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				err := i.Handler(hctx, env, payload)
				seen <- delivery{env.GetEventId(), err}
				return err
			}, func() { close(ready) })
		}()
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("consumer stopped before readiness: %v", err)
		case <-ctx.Done():
			t.Fatal("consumer never reached initial catch-up")
		}
		var bound jetstream.Consumer
		list := input.ListConsumers(ctx)
		for info := range list.Info() {
			if info.Config.FilterSubject == topic && strings.HasPrefix(info.Name, group+"-") {
				bound, err = input.Consumer(ctx, info.Name)
				if err != nil {
					t.Fatal(err)
				}
				name := info.Name
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := input.DeleteConsumer(cleanup, name); err != nil {
						t.Error(err)
					}
				})
			}
		}
		if err := list.Err(); err != nil {
			t.Fatal(err)
		}
		if bound == nil {
			t.Fatal("real durable consumer not found")
		}
		stopped := false
		stopSession := func() {
			if stopped {
				return
			}
			stopped = true
			stop()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-ctx.Done():
				t.Error("durable consumer failed to stop")
			}
			r.Drain()
		}
		t.Cleanup(stopSession)
		return session{seen, bound, cache, stopSession}
	}
	await := func(s session, reject bool, acked uint64) string {
		t.Helper()
		var d delivery
		select {
		case d = <-s.seen:
		case <-ctx.Done():
			t.Fatal("missing state delivery")
		}
		if (d.err != nil) != reject || (reject && !strings.Contains(d.err.Error(), "as_of")) {
			t.Fatalf("delivery rejection=%v error=%v", reject, d.err)
		}
		for {
			info, err := s.c.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.AckFloor.Consumer >= acked && info.NumAckPending == 0 && info.NumPending == 0 {
				return d.id
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("state delivery not acknowledged by JetStream")
			}
		}
	}
	publish := func(ts *timestamppb.Timestamp) {
		t.Helper()
		if err := producer.Publish(ctx, bus.Event{Subject: topic, EventType: ingest.EventTypePositionChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "risk", EventTime: baseTime.Add(time.Hour), PartitionKey: portfolio, PayloadSchemaRef: "domain.v1.PositionState:1", Payload: &domainpb.PositionState{PortfolioId: portfolio, InstrumentId: "A", Quantity: &commonpb.Decimal{Coefficient: 7}, AveragePrice: &commonpb.Decimal{Coefficient: 10}, MarketValue: money(70, "USD"), AsOf: ts}}); err != nil {
			t.Fatal(err)
		}
	}
	assertDLQ := func(eventID string, original []byte) {
		t.Helper()
		m, err := parked.GetLastMsgForSubject(ctx, dlqTopic)
		if err != nil {
			t.Fatal(err)
		}
		e, _, err := bus.Unframe(m.Data)
		if err != nil || e.GetEventId() != eventID || !bytes.Equal(m.Data, original) || m.Header.Get(bus.HeaderDLQOriginalSubject) != topic || m.Header.Get(bus.HeaderDLQAttempts) != "1" || !strings.Contains(m.Header.Get(bus.HeaderDLQError), "as_of") {
			t.Fatalf("DLQ lost rejected bytes/identity/failure metadata: %v", err)
		}
	}
	live := state.NewStore()
	seedStateTime(t, live, portfolio)
	initial := checkpoint(live)
	s1 := start(live, "-live")
	publish(&timestamppb.Timestamp{Seconds: baseTime.Unix(), Nanos: -1})
	badID := await(s1, true, 1)
	bad, err := input.GetLastMsgForSubject(ctx, topic)
	if err != nil {
		t.Fatal(err)
	}
	badEnvelope, _, err := bus.Unframe(bad.Data)
	if err != nil || bus.Validate(badEnvelope) != nil || badEnvelope.GetEventId() != badID {
		t.Fatal("fixture was rejected before the state owner")
	}
	assertDLQ(badID, bad.Data)
	stateTimeSameRecord(t, initial, checkpoint(live))
	s1.stop()
	if _, ok := s1.cache.LookupExposure(v1.PortfolioID(portfolio)); ok {
		t.Fatal("rejected live delivery triggered recomputation")
	}
	// The invalid compacted value survives the first process and is refused
	// again on retained delivery into an independently restored hot store.
	restarted := restore(initial)
	s2 := start(restarted, "-restart")
	if id := await(s2, true, 1); id != badID {
		t.Fatal("restart did not consume the retained invalid event")
	}
	assertDLQ(badID, bad.Data)
	stateTimeSameRecord(t, initial, checkpoint(restarted))
	valid := timestamppb.New(baseTime.Add(time.Hour + 123*time.Millisecond))
	publish(valid)
	goodID := await(s2, false, 2)
	if goodID == badID {
		t.Fatal("repair did not have its own immutable FACT identity")
	}
	good := checkpoint(restarted)
	if len(good.Positions) != 2 || !good.Positions[0].AsOf.Equal(valid.AsTime()) {
		t.Fatal("valid repair failed to persist the exact position/time")
	}
	s2.stop()
	if _, ok := s2.cache.LookupExposure(v1.PortfolioID(portfolio)); !ok {
		t.Fatal("successful repair did not trigger recomputation")
	}
	replayed := restore(good)
	s3 := start(replayed, "-valid-replay")
	if id := await(s3, false, 1); id != goodID {
		t.Fatal("restart did not consume the retained repaired event")
	}
	stateTimeSameRecord(t, good, checkpoint(replayed))
	s3.stop()
}
