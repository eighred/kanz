package consume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/wealth"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/drift"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestHouseholdIdentityCanonicalDLQAckRestartAndRepair(t *testing.T) {
	brokerURL := os.Getenv("TEST_NATS_URL")
	if brokerURL == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires canonical WEALTH/DLQ and NOSUPERUSER/NOBYPASSRLS PostgreSQL")
	}
	poolFor := replacementPostgres(t)
	tenant := fmt.Sprintf("household-identity-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	nc, err := nats.Connect(brokerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	storedMessages, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "WEALTH")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.Config.MaxMsgsPerSubject != 1 || info.Config.MaxAge != 0 {
		t.Fatal("requires actual compacted never-ageing WEALTH", err)
	}
	filter := wealth.EventTypeHouseholdValued + "." + subject.Token(tenant) + ".>"
	deadStream, err := bustest.StreamFor(ctx, js, "dlq."+wealth.SubjectHouseholdFor(tenant, "victim"))
	if err != nil {
		t.Fatal("canonical DLQ stream required", err)
	}
	deadName := tenant + "-dead"
	dead, err := js.CreateConsumer(ctx, deadStream, jetstream.ConsumerConfig{Name: deadName, FilterSubject: "dlq." + filter,
		DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), deadStream, deadName) })
	ids := []string{"victim", "source-key", "source-type", "source-class", "source-tenant"}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, id := range ids {
			topic := wealth.SubjectHouseholdFor(tenant, id)
			if !strings.Contains(topic, "."+subject.Token(tenant)+".") || strings.ContainsAny(topic, "*>") {
				t.Error("unsafe household fixture reclaim")
				continue
			}
			if err := stream.Purge(cleanup, jetstream.WithPurgeSubject(topic)); err != nil {
				t.Error(err)
			}
			deadOwner, err := js.Stream(cleanup, deadStream)
			if err != nil {
				t.Error(err)
				continue
			}
			if err := deadOwner.Purge(cleanup, jetstream.WithPurgeSubject("dlq."+topic)); err != nil {
				t.Error(err)
			}
		}
	})
	publisher, err := bus.DialNATS(ctx, bus.NATSConfig{URL: brokerURL, Name: tenant + "-publisher"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	producer, err := bus.NewProducer(publisher, bus.ProducerConfig{Source: "kanz-household", ProducerVersion: "identity-fixture", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	type delivery struct {
		env     *envelopepb.Envelope
		payload []byte
		err     error
	}
	type session struct {
		handled <-chan delivery
		source  jetstream.Consumer
		stop    func()
	}
	start := func(t *testing.T, store book.Store, group string) session {
		t.Helper()
		client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: brokerURL, Name: group})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
		if err != nil {
			t.Fatal(err)
		}
		registry := wealth.NewModelRegistry()
		model := replacementModel("identity-model", wealthpb.RiskProfile_RISK_PROFILE_GROWTH)
		if err := replacementFolder(t, tenant, registry).Handle(ctx, replacementEnvelope(tenant, model.ModelId), modelPayload(t, model)); err != nil {
			t.Fatal(err)
		}
		registry.Arm()
		folder, err := NewFolder(tenant, store, DecodeProto, WithDriftObserver(drift.New(tenant, registry, nil, nil)))
		if err != nil {
			t.Fatal(err)
		}
		subCtx, stop := context.WithCancel(ctx)
		done, ready, handled := make(chan error, 1), make(chan struct{}), make(chan delivery, 32)
		go func() {
			defer close(done)
			done <- consumer.SubscribeReady(subCtx, filter, group, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				err := folder.Handle(ctx, env, payload)
				select {
				case handled <- delivery{proto.Clone(env).(*envelopepb.Envelope), bytes.Clone(payload), err}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return err
			}, func() { close(ready) })
		}()
		stopped := false
		var durableName string
		join := func() {
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
			case <-time.After(10 * time.Second):
				t.Error("household durable did not join")
			}
			if durableName != "" {
				cleanup, stopCleanup := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopCleanup()
				if err := js.DeleteConsumer(cleanup, "WEALTH", durableName); err != nil {
					t.Error(err)
				}
			}
			_ = client.Close()
		}
		// Register before readiness and discovery can fail. Join precedes durable
		// deletion, socket closure and the store/schema cleanup on every exit.
		t.Cleanup(join)
		select {
		case <-ready:
		case err := <-done:
			t.Fatal("household subscription exited before catch-up", err)
		case <-ctx.Done():
			join()
			t.Fatal(ctx.Err())
		}
		listing := stream.ListConsumers(ctx)
		for c := range listing.Info() {
			if strings.HasPrefix(c.Name, group+"-") && c.Config.FilterSubject == filter {
				if durableName != "" {
					t.Fatal("ambiguous household source durable")
				}
				durableName = c.Name
			}
		}
		if listing.Err() != nil || durableName == "" {
			t.Fatal("missing household source durable", listing.Err())
		}
		source, err := js.Consumer(ctx, "WEALTH", durableName)
		if err != nil {
			t.Fatal(err)
		}
		return session{handled, source, join}
	}
	next := func(t *testing.T, s session) delivery {
		t.Helper()
		select {
		case d := <-s.handled:
			if bus.Validate(d.env) != nil {
				t.Fatal("broker delivered a noncanonical envelope")
			}
			return d
		case <-ctx.Done():
			t.Fatal("household was not delivered", ctx.Err())
			return delivery{}
		}
	}
	var lastSequence uint64
	awaitACK := func(t *testing.T, s session) {
		t.Helper()
		for {
			info, err := s.source.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Stream >= lastSequence {
				return
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("original source frame was not ACKed", ctx.Err())
			}
		}
	}
	publish := func(t *testing.T, s session, sourceID, envelopeTenant, key, kind string, class envelopepb.EventClass, h *wealthpb.HouseholdValued) (*jetstream.RawStreamMsg, delivery) {
		t.Helper()
		topic := wealth.SubjectHouseholdFor(tenant, sourceID)
		if err := producer.Publish(ctx, bus.Event{Subject: topic, EventType: kind, EventClass: class,
			SchemaVersion: 1, Domain: wealth.Domain, TenantID: envelopeTenant, PartitionKey: key,
			EventTime: h.AsOf.AsTime(), PayloadSchemaRef: "wealth.v1.HouseholdValued:1", Payload: h}); err != nil {
			t.Fatal(err)
		}
		d := next(t, s)
		// The management read returns stored wire headers. A direct get adds
		// response-only Nats-Stream/Sequence/Subject/Time-Stamp headers, which
		// never traveled through the consumer and must not be expected in DLQ.
		stored, err := storedMessages.GetLastMsg("WEALTH", topic, nats.Context(ctx))
		if err != nil {
			t.Fatal(err)
		}
		raw := &jetstream.RawStreamMsg{Subject: stored.Subject, Sequence: stored.Sequence,
			Header: stored.Header, Data: stored.Data, Time: stored.Time}
		env, payload, err := bus.Unframe(raw.Data)
		if err != nil || !proto.Equal(env, d.env) || !bytes.Equal(payload, d.payload) || !bytes.Equal(payload, identityPayload(t, h)) {
			t.Fatal("canonical retained original differs from delivered frame", err)
		}
		lastSequence = raw.Sequence
		return raw, d
	}
	pool := poolFor(tenant)
	store := book.NewPostgres(pool)
	warm := start(t, store, tenant+"-warm")
	for _, id := range ids {
		_, d := publish(t, warm, id, tenant, id, wealth.EventTypeHouseholdValued, envelopepb.EventClass_EVENT_CLASS_FACT, identityHousehold(id))
		if d.err != nil {
			t.Fatal(d.err)
		}
	}
	awaitACK(t, warm)
	type durableRow struct {
		composition string
		updatedAt   time.Time
	}
	readRow := func(t *testing.T, id string) durableRow {
		t.Helper()
		var row durableRow
		if err := pool.QueryRow(ctx, `SELECT composition::text,updated_at FROM households WHERE household_id=$1`, id).Scan(&row.composition, &row.updatedAt); err != nil {
			t.Fatal(err)
		}
		return row
	}
	beforeRows := make(map[string]durableRow, len(ids))
	for _, id := range ids {
		beforeRows[id] = readRow(t, id)
	}
	bad := make(map[string]*jetstream.RawStreamMsg)
	for _, tc := range []struct {
		name, sourceID, payloadID, envelopeTenant, eventType string
		class                                                envelopepb.EventClass
	}{
		{"key", "source-key", "victim", tenant, wealth.EventTypeHouseholdValued, envelopepb.EventClass_EVENT_CLASS_FACT},
		{"type", "source-type", "source-type", tenant, wealth.EventTypeModelPublished, envelopepb.EventClass_EVENT_CLASS_FACT},
		{"class", "source-class", "source-class", tenant, wealth.EventTypeHouseholdValued, envelopepb.EventClass_EVENT_CLASS_OBSERVATION},
		{"tenant", "source-tenant", "source-tenant", tenant + "-foreign", wealth.EventTypeHouseholdValued, envelopepb.EventClass_EVENT_CLASS_FACT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := identityHousehold(tc.payloadID)
			h.Accounts[0].Holdings[0].MarketValue.Coefficient = 1200
			h.Accounts[0].Holdings[1].MarketValue.Coefficient = 800
			raw, d := publish(t, warm, tc.sourceID, tc.envelopeTenant, tc.sourceID, tc.eventType, tc.class, h)
			if d.err == nil {
				t.Fatal("mismatched broker frame reached a household projection")
			}
			parked, err := dead.Next(jetstream.FetchMaxWait(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(parked.Data(), raw.Data) || parked.Headers().Get(bus.HeaderDLQOriginalSubject) != raw.Subject ||
				parked.Headers().Get(bus.HeaderDLQAttempts) != "1" || !strings.Contains(parked.Headers().Get(bus.HeaderDLQError), d.err.Error()) {
				t.Fatal("DLQ lost original bytes/subject/attempt/reason")
			}
			if raw.Header.Get(nats.MsgIdHdr) == "" {
				t.Fatal("canonical source omitted its broker deduplication identity")
			}
			for key, values := range raw.Header {
				if !reflect.DeepEqual(parked.Headers().Values(key), values) {
					t.Fatalf("DLQ changed original wire header %s", key)
				}
			}
			if err := parked.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
			bad[d.env.EventId] = raw
			awaitACK(t, warm)
			for _, id := range ids {
				if readRow(t, id) != beforeRows[id] {
					t.Fatal("refused frame changed stored composition or update time")
				}
				requireIdentityView(t, tenant, id, store, false)
			}
		})
	}
	warm.stop()
	pool.Close()
	pool = poolFor(tenant)
	store = book.NewPostgres(pool)
	other := book.NewPostgres(poolFor(tenant + "-other"))
	foreignHousehold := replacementHousehold()
	foreignHousehold.HouseholdID, foreignHousehold.Accounts[0].Cash = "victim", "25"
	if err := other.Put(ctx, foreignHousehold); err != nil {
		t.Fatal(err)
	}
	requireIdentityHousehold(t, other, foreignHousehold)
	for _, id := range ids {
		if _, ok, err := other.Get(ctx, id); err != nil || ok != (id == "victim") {
			t.Fatal("RLS exposed another tenant's household", err)
		}
		requireIdentityView(t, tenant, id, store, false)
	}
	checkReplay := func(t *testing.T, s session) {
		t.Helper()
		seen := map[string]bool{}
		for range len(ids) {
			d := next(t, s)
			if seen[d.env.EventId] {
				t.Fatal("duplicate retained delivery in fresh durable")
			}
			seen[d.env.EventId] = true
			if raw, invalid := bad[d.env.EventId]; invalid {
				if d.err == nil || !bytes.Equal(d.payload, mustUnframeIdentity(t, raw.Data)) {
					t.Fatal("fresh retained replay accepted or changed bad attribution")
				}
			} else if d.env.PartitionKey != "victim" || d.err != nil {
				t.Fatal("valid retained victim disappeared or changed", d.err)
			}
		}
		for id, raw := range bad {
			actual, err := stream.GetMsg(ctx, raw.Sequence)
			if !seen[id] || err != nil || !bytes.Equal(actual.Data, raw.Data) {
				t.Fatal("original retained refusal was lost", err)
			}
		}
		awaitACK(t, s)
	}
	t.Run("fresh_retained_refusal", func(t *testing.T) {
		coldStore := book.NewMemoryStore()
		cold := start(t, coldStore, tenant+"-cold")
		checkReplay(t, cold)
		cold.stop()
		requireIdentityView(t, tenant, "victim", coldStore, false)
		for _, id := range ids[1:] {
			if _, ok, err := coldStore.Get(ctx, id); err != nil || ok {
				t.Fatal("invalid retained identity created a fresh household", err)
			}
		}
	})
	t.Run("matching_repair_and_postgres_reopen", func(t *testing.T) {
		restarted := start(t, store, tenant+"-restarted")
		checkReplay(t, restarted)
		for _, id := range ids {
			h := identityHousehold(id)
			h.Accounts[0].Holdings[0].MarketValue.Coefficient = 1200
			h.Accounts[0].Holdings[1].MarketValue.Coefficient = 800
			_, d := publish(t, restarted, id, tenant, id, wealth.EventTypeHouseholdValued, envelopepb.EventClass_EVENT_CLASS_FACT, h)
			if d.err != nil {
				t.Fatal(d.err)
			}
			if _, reused := bad[d.env.EventId]; reused {
				t.Fatal("matching repair reused a parked frame UUID")
			}
		}
		awaitACK(t, restarted)
		restarted.stop()
		pool.Close()
		store = book.NewPostgres(poolFor(tenant))
		for _, id := range ids {
			requireIdentityView(t, tenant, id, store, true)
		}
		requireIdentityHousehold(t, other, foreignHousehold)
	})
}

func mustUnframeIdentity(t *testing.T, frame []byte) []byte {
	t.Helper()
	_, payload, err := bus.Unframe(frame)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
