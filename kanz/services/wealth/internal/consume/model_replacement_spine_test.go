package consume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/wealth"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/drift"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func replacementPostgres(t *testing.T) func(string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires NOSUPERUSER/NOBYPASSRLS PostgreSQL")
	}
	setup, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(setup.Close)
	var super, bypass bool
	if err := setup.QueryRow(t.Context(), `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatal("household proof refuses privileged RLS bypass")
	}
	schema := pgx.Identifier{fmt.Sprintf("wealth_replacement_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := setup.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := setup.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	poolFor := func(tenant string) *pgxpool.Pool {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, `SELECT set_config('app.tenant_id',$1,false)`, tenant)
			return err
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	pool := poolFor("migration")
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) != 2 {
		t.Fatalf("expected actual wealth migrations: %v %v", files, err)
	}
	sort.Strings(files)
	for _, file := range files {
		ddl, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), string(ddl)); err != nil {
			t.Fatal(err)
		}
	}
	pool.Close()
	return poolFor
}

type replacementObservedTransport struct {
	*bus.NATSClient
	created chan [2]string
}

func (c *replacementObservedTransport) SubscribeBroadcastObserved(ctx context.Context, filter string, h bus.Handler, ready func(), created bus.ConsumerCreated) error {
	return c.NATSClient.SubscribeBroadcastObserved(ctx, filter, h, ready, func(stream, consumer string) {
		if created != nil {
			created(stream, consumer)
		}
		c.created <- [2]string{stream, consumer}
	})
}

func TestWealthModelReplacementCanonicalBroadcastRecoveryAndHouseholdPersistence(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires canonical WEALTH JetStream and NOSUPERUSER/NOBYPASSRLS PostgreSQL")
	}
	poolFor := replacementPostgres(t)
	id := fmt.Sprintf("wealth-replacement-%d", time.Now().UnixNano())
	tenant, otherTenant := id+"-a", id+"-b"
	pool := poolFor(tenant)
	store := book.NewPostgres(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "WEALTH")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.Config.MaxMsgsPerSubject != 1 || info.Config.MaxAge != 0 {
		t.Fatalf("requires canonical compacted never-ageing WEALTH: %v", err)
	}
	primary, sibling := id+".growth", id+"%2Egrowth"
	household := replacementHousehold()
	modelSubject := wealth.SubjectModelFor(tenant, primary)
	householdSubject := wealth.SubjectHouseholdFor(tenant, household.HouseholdID)
	ownedSubjects := []string{modelSubject, wealth.SubjectModelFor(tenant, sibling), householdSubject}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, topic := range ownedSubjects {
			if !strings.Contains(topic, "."+subject.Token(tenant)+".") || strings.ContainsAny(topic, "*>") {
				t.Error("unsafe wealth fixture reclaim")
				continue
			}
			if err := stream.Purge(cleanup, jetstream.WithPurgeSubject(topic)); err != nil {
				t.Error(err)
			}
		}
	})
	publisher, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id + "-publisher"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	producer, err := bus.NewProducer(publisher, bus.ProducerConfig{Source: "kanz-model", ProducerVersion: "replacement-test"})
	if err != nil {
		t.Fatal(err)
	}
	type sourceFact struct {
		env            *envelopepb.Envelope
		payload, frame []byte
		seq            uint64
	}
	publish := func(m *wealthpb.ModelPortfolio, envelopeTenant, key string) sourceFact {
		t.Helper()
		// Exactly the canonical CLI contract: plain model_id is the partition
		// key, with a separately encoded per-tenant/model compacted subject.
		if err := producer.Publish(ctx, bus.Event{Subject: wealth.SubjectModelFor(tenant, m.ModelId), EventType: wealth.EventTypeModelPublished,
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: wealth.Domain, EventTime: time.Now().UTC(),
			PartitionKey: key, TenantID: envelopeTenant, PayloadSchemaRef: "wealth.v1.ModelPortfolio:1", Payload: m}); err != nil {
			t.Fatal(err)
		}
		raw, err := stream.GetLastMsgForSubject(ctx, wealth.SubjectModelFor(tenant, m.ModelId))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(raw.Data)
		var actual wealthpb.ModelPortfolio
		if err != nil || bus.Validate(env) != nil || proto.Unmarshal(payload, &actual) != nil || !proto.Equal(m, &actual) || env.GetPartitionKey() != key || env.GetTenantId() != envelopeTenant {
			t.Fatalf("canonical publisher changed model identity/policy: %v", err)
		}
		return sourceFact{env, bytes.Clone(payload), bytes.Clone(raw.Data), raw.Sequence}
	}
	healthy := replacementModel(primary, wealthpb.RiskProfile_RISK_PROFILE_GROWTH)
	first := publish(healthy, tenant, primary)
	side := publish(replacementModel(sibling, wealthpb.RiskProfile_RISK_PROFILE_BALANCED), tenant, sibling)
	type delivery struct {
		env     *envelopepb.Envelope
		payload []byte
		err     error
	}
	type session struct {
		reg      *wealth.ModelRegistry
		consumer jetstream.Consumer
		ready    <-chan struct{}
		handled  <-chan delivery
		stop     func()
	}
	start := func() session {
		t.Helper()
		client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id + "-replica"})
		if err != nil {
			t.Fatal(err)
		}
		transport := &replacementObservedTransport{client, make(chan [2]string, 1)}
		consumer, err := bus.NewConsumer(transport, bus.WithDLQ(client))
		if err != nil {
			_ = client.Close()
			t.Fatal(err)
		}
		reg := wealth.NewModelRegistry()
		folder := replacementFolder(t, tenant, reg)
		subCtx, stop := context.WithCancel(ctx)
		ready, done, handled := make(chan struct{}), make(chan error, 1), make(chan delivery, 64)
		go func() {
			done <- consumer.SubscribeBroadcastReady(subCtx, wealth.EventTypeModelPublished+"."+subject.Token(tenant)+".>", func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				err := folder.Handle(ctx, env, payload)
				select {
				case handled <- delivery{proto.Clone(env).(*envelopepb.Envelope), bytes.Clone(payload), err}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return err
			}, func() { reg.Arm(); close(ready) })
		}()
		stopped := false
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
				t.Error("wealth broadcast did not join")
			}
			_ = client.Close()
		}
		t.Cleanup(join)
		var identity [2]string
		select {
		case identity = <-transport.created:
		case <-ctx.Done():
			t.Fatal("broker-assigned broadcast identity missing")
		}
		c, err := js.Consumer(ctx, identity[0], identity[1])
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), identity[0], identity[1]) })
		return session{reg, c, ready, handled, join}
	}
	waitReady := func(s session) {
		t.Helper()
		select {
		case <-s.ready:
		case <-ctx.Done():
			t.Fatal("retained model replay did not arm after repair")
		}
		if !s.reg.Armed() {
			t.Fatal("ready callback did not arm catalogue")
		}
	}
	waitACK := func(s session, sequence uint64) {
		t.Helper()
		for {
			info, err := s.consumer.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Stream >= sequence {
				return
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("model repair was not broker-ACKed")
			}
		}
	}
	waitRefusals := func(s session, fact sourceFact, reason string) {
		t.Helper()
		for count := 0; count < 2; {
			select {
			case got := <-s.handled:
				if got.env.GetEventId() != fact.env.GetEventId() {
					continue
				}
				if !proto.Equal(got.env, fact.env) || !bytes.Equal(got.payload, fact.payload) || got.err == nil || !strings.Contains(got.err.Error(), reason) {
					t.Fatalf("poison delivery lost source/refusal: %v", got.err)
				}
				count++
			case <-ctx.Done():
				t.Fatal("refused model was not actually redelivered")
			}
		}
		for {
			info, err := s.consumer.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumRedelivered > 0 && info.NumAckPending > 0 && info.AckFloor.Stream < fact.seq {
				break
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("poison was ACKed/parked instead of remaining NAKed")
			}
		}
		raw, err := stream.GetMsg(ctx, fact.seq)
		if err != nil || !bytes.Equal(raw.Data, fact.frame) {
			t.Fatalf("retained poison bytes disappeared or changed: %v", err)
		}
	}
	warm := start()
	waitReady(warm)
	waitACK(warm, side.seq)
	requireReplacementModel(t, warm.reg, tenant, wealth.ProfileGrowth, primary, healthy.ExactTargetWeights)
	// Fold the actual canonical household FACT through the real durable bus
	// path and Postgres, rather than seeding a parallel read-only test store.
	householdFolder, err := NewFolder(tenant, store, DecodeProto, WithDriftObserver(drift.New(tenant, warm.reg, nil, nil)))
	if err != nil {
		t.Fatal(err)
	}
	householdConsumer, err := bus.NewConsumer(publisher, bus.WithDLQ(publisher))
	if err != nil {
		t.Fatal(err)
	}
	hCtx, stopHousehold := context.WithCancel(ctx)
	hReady, hDone, hHandled := make(chan struct{}), make(chan error, 1), make(chan error, 1)
	go func() {
		hDone <- householdConsumer.SubscribeReady(hCtx, householdSubject, id+"-household", func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			err := householdFolder.Handle(ctx, env, payload)
			select {
			case hHandled <- err:
			case <-ctx.Done():
				return ctx.Err()
			}
			return err
		}, func() { close(hReady) })
	}()
	householdStopped := false
	joinHousehold := func() {
		if householdStopped {
			return
		}
		householdStopped = true
		stopHousehold()
		select {
		case err := <-hDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("household consumer did not join")
		}
	}
	t.Cleanup(joinHousehold)
	select {
	case <-hReady:
	case <-ctx.Done():
		t.Fatal("household consumer was not ready")
	}
	wireHousehold := &wealthpb.HouseholdValued{HouseholdId: household.HouseholdID, RiskProfile: wealthpb.RiskProfile_RISK_PROFILE_GROWTH,
		CurrencyCode: household.CurrencyCode, AsOf: timestamppb.New(household.AsOf), RecordedBy: household.RecordedBy, Reason: household.Reason,
		Accounts: []*wealthpb.ValuedAccount{{AccountId: "account", Cash: &commonpb.Decimal{}, Holdings: []*wealthpb.ValuedHolding{
			{InstrumentId: "VTI", AssetClass: "EQUITY", MarketValue: &commonpb.Decimal{Coefficient: 800}},
			{InstrumentId: "BND", AssetClass: "FIXED_INCOME", MarketValue: &commonpb.Decimal{Coefficient: 200}},
		}}}}
	if err := producer.Publish(ctx, bus.Event{Subject: householdSubject, EventType: wealth.EventTypeHouseholdValued, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: wealth.Domain, EventTime: household.AsOf, PartitionKey: household.HouseholdID, TenantID: tenant, PayloadSchemaRef: "wealth.v1.HouseholdValued:1", Payload: wireHousehold}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-hHandled:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("canonical household did not reach Postgres")
	}
	var householdDurable string
	listing := stream.ListConsumers(ctx)
	for c := range listing.Info() {
		if c.Config.FilterSubject == householdSubject && c.Config.Durable != "" {
			if householdDurable != "" {
				t.Fatal("ambiguous household durable")
			}
			householdDurable = c.Name
		}
	}
	if listing.Err() != nil || householdDurable == "" {
		t.Fatal("household durable missing", listing.Err())
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), "WEALTH", householdDurable) })
	hConsumer, err := js.Consumer(ctx, "WEALTH", householdDurable)
	if err != nil {
		t.Fatal(err)
	}
	for {
		info, err := hConsumer.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.NumAckPending == 0 && info.NumPending == 0 && info.AckFloor.Consumer > 0 {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("household FACT was not ACKed")
		}
	}
	joinHousehold()
	requireReplacementView(t, tenant, store, warm.reg, drift.OutcomeBreached, true)
	snapshot := func(pool *pgxpool.Pool) []byte {
		t.Helper()
		var data []byte
		if err := pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(h) ORDER BY household_id) FROM households h`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := snapshot(pool)
	// A valid envelope for another tenant must not invalidate this resource.
	foreign := publish(replacementModel(primary, wealthpb.RiskProfile(99)), otherTenant, primary)
	waitRefusals(warm, foreign, "tenant")
	requireReplacementView(t, tenant, store, warm.reg, drift.OutcomeBreached, true)
	foreignRepair := publish(healthy, tenant, primary)
	waitACK(warm, foreignRepair.seq)
	poison := publish(replacementModel(primary, wealthpb.RiskProfile(99)), tenant, primary)
	waitRefusals(warm, poison, "risk_profile")
	requireReplacementView(t, tenant, store, warm.reg, drift.OutcomeInvalidModel, false)
	requireReplacementModel(t, warm.reg, tenant, wealth.ProfileBalanced, sibling, replacementModel(sibling, 0).ExactTargetWeights)
	// A decodable cross-key repair cannot clear the known-profile rejection.
	wrongKey := publish(healthy, tenant, sibling)
	waitRefusals(warm, wrongKey, "partition_key")
	requireReplacementView(t, tenant, store, warm.reg, drift.OutcomeInvalidModel, false)
	poison = publish(replacementModel(primary, wealthpb.RiskProfile(99)), tenant, primary)
	waitRefusals(warm, poison, "risk_profile")
	warm.stop()
	if !bytes.Equal(before, snapshot(pool)) {
		t.Fatal("model control changes rewrote the durable household")
	}
	pool.Close()
	reopenedPool := poolFor(tenant)
	reopened := book.NewPostgres(reopenedPool)
	if !bytes.Equal(before, snapshot(reopenedPool)) {
		t.Fatal("household reopen changed exact state/provenance")
	}
	cold := start()
	waitRefusals(cold, poison, "risk_profile")
	if cold.reg.Armed() || cold.reg.Count() != 1 || len(cold.reg.Rejections()) != 1 {
		t.Fatal("cold retained poison armed the model catalogue")
	}
	select {
	case <-cold.ready:
		t.Fatal("cold ready callback fired through poison")
	default:
	}
	requireReplacementView(t, tenant, reopened, cold.reg, drift.OutcomeCatalogueUnarmed, false)
	repair := publish(healthy, tenant, primary)
	if repair.env.GetEventId() == poison.env.GetEventId() || repair.env.GetEventId() == first.env.GetEventId() {
		t.Fatal("repair reused a superseded/poisoned frame identity")
	}
	waitReady(cold)
	waitACK(cold, repair.seq)
	if len(cold.reg.Rejections()) != 0 || cold.reg.Count() != 2 {
		t.Fatal("matching repair did not clear rejection")
	}
	requireReplacementView(t, tenant, reopened, cold.reg, drift.OutcomeBreached, true)
	cold.stop()
	fresh := start()
	waitReady(fresh)
	waitACK(fresh, repair.seq)
	if fresh.reg.Count() != 2 || len(fresh.reg.Rejections()) != 0 {
		t.Fatal("fresh replay lost a valid retained model or retained a refusal")
	}
	expected := map[string]sourceFact{repair.env.GetEventId(): repair, side.env.GetEventId(): side}
	for range len(expected) {
		select {
		case got := <-fresh.handled:
			fact, ok := expected[got.env.GetEventId()]
			if !ok || got.err != nil || !proto.Equal(got.env, fact.env) || !bytes.Equal(got.payload, fact.payload) {
				t.Fatal("fresh replay did not consume the retained matching repair/sibling")
			}
			delete(expected, got.env.GetEventId())
		case <-ctx.Done():
			t.Fatal("fresh retained replay evidence missing")
		}
	}
	requireReplacementView(t, tenant, reopened, fresh.reg, drift.OutcomeBreached, true)
	fresh.stop()
	if !bytes.Equal(before, snapshot(reopenedPool)) {
		t.Fatal("retained model replay mutated household history")
	}
	other := book.NewPostgres(poolFor(otherTenant))
	if _, ok, err := other.Get(ctx, household.HouseholdID); err != nil || ok {
		t.Fatalf("household RLS leak: %t %v", ok, err)
	}
	household.Accounts[0].Cash = "9"
	if err := other.Put(ctx, household); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, snapshot(reopenedPool)) {
		t.Fatal("same household ID in another tenant changed this book")
	}
}
