package compliance_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/platform/subject"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Observe the broker-assigned identity while retaining the production
// NATSClient's delivery, validation, ACK and retained replay implementation.
type identityObservedTransport struct {
	*bus.NATSClient
	created chan [2]string
}

func (c *identityObservedTransport) SubscribeBroadcastObserved(ctx context.Context, filter string, h bus.Handler, ready func(), created bus.ConsumerCreated) error {
	return c.NATSClient.SubscribeBroadcastObserved(ctx, filter, h, ready, func(stream, consumer string) {
		if created != nil {
			created(stream, consumer)
		}
		c.created <- [2]string{stream, consumer}
	})
}

func TestMandateIdentityCanonicalReplayGateAndRepair(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL with canonical MANDATE topology")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id := fmt.Sprintf("mandate-identity-%d", time.Now().UnixNano())
	tenantA, tenantB, portfolio := id+"-a", id+"-b", "PF-"+id
	topicA, topicB := comp.SubjectMandateFor(tenantA, portfolio), comp.SubjectMandateFor(tenantB, portfolio)
	filter := comp.SubjectMandateChanged + ".*." + subject.Token(portfolio)
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "MANDATE")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.Config.MaxMsgsPerSubject != 1 || info.Config.MaxAge != 0 {
		t.Fatalf("requires canonical never-ageing compacted MANDATE: %v", err)
	}
	// Register unique subject reclaim before the first publish, never purge a
	// whole shared control stream or another tenant's mandate.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, topic := range []string{topicA, topicB} {
			if !strings.Contains(topic, subject.Token(portfolio)) || strings.ContainsAny(topic, "*>") {
				t.Error("unsafe mandate fixture reclaim")
				continue
			}
			if err := stream.Purge(cleanup, jetstream.WithPurgeSubject(topic)); err != nil {
				t.Error(err)
			}
		}
	})
	bustest.EnsureSubjects(t, ctx, js, "IDENTITY_DECISION_"+strings.ReplaceAll(id, "-", "_"), []string{compliancebus.SubjectDecision})
	decisionStreamName, err := bustest.StreamFor(ctx, js, compliancebus.SubjectDecision)
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := js.Stream(ctx, decisionStreamName)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := decisions.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: id, FilterSubject: compliancebus.SubjectDecision, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	var decisionSequences []uint64
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := decisions.DeleteConsumer(cleanup, id); err != nil {
			t.Error(err)
		}
		for _, seq := range decisionSequences {
			if err := decisions.DeleteMsg(cleanup, seq); err != nil {
				t.Error(err)
			}
		}
	})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "kanz-mandate", ProducerVersion: "identity-test"})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	publisher := comp.NewPublisher(producer, client)
	mandate := func(tenant string, version uint64) *compliancepb.Mandate {
		return &compliancepb.Mandate{MandateId: "M-" + tenant, TenantId: tenant, PortfolioId: portfolio, Version: version, EffectiveAt: timestamppb.New(time.Unix(0, 0))}
	}
	first, victim := mandate(tenantA, 1), mandate(tenantB, 1)
	victim.Rules = []*compliancepb.Rule{{RuleId: "deny-A", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION, Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT, Mode: compliancepb.RestrictionMode_RESTRICTION_MODE_DENY, Values: []string{"A"}}}}}
	approved := func(m *compliancepb.Mandate) {
		t.Helper()
		if err := publisher.Publish(ctx, m, mustApprove(t, m, id), id); err != nil {
			t.Fatal(err)
		}
	}
	approved(first)
	approved(victim)
	setValue, err := comp.MarshalMandateSetValue([]*compliancepb.Mandate{mandate(tenantA, 2), mandate(tenantB, 9)})
	if err != nil {
		t.Fatal(err)
	}
	type session struct {
		reg  *comp.MandateRegistry
		c    jetstream.Consumer
		stop func()
	}
	start := func() session {
		t.Helper()
		reg := comp.NewMandateRegistry(comp.WithMandateLogger(logger))
		impl := comp.NewMandateConsumer(reg, logger)
		transport := &identityObservedTransport{client, make(chan [2]string, 1)}
		consumer, err := bus.NewConsumer(transport, bus.WithDLQ(client))
		if err != nil {
			t.Fatal(err)
		}
		subCtx, stop := context.WithCancel(ctx)
		t.Cleanup(stop)
		ready, done := make(chan struct{}), make(chan error, 1)
		go func() {
			done <- consumer.SubscribeBroadcastReady(subCtx, filter, impl.Handle, func() { reg.Arm(); close(ready) })
		}()
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("mandate consumer stopped: %v", err)
		case <-ctx.Done():
			t.Fatal("mandate replay never armed")
		}
		var identity [2]string
		select {
		case identity = <-transport.created:
		case <-ctx.Done():
			t.Fatal("broker consumer identity was not observed")
		}
		c, err := js.Consumer(ctx, identity[0], identity[1])
		if err != nil {
			t.Fatal(err)
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
				t.Error("mandate consumer did not stop")
			}
		}
		t.Cleanup(stopSession)
		return session{reg, c, stopSession}
	}
	awaitACK := func(s session, count uint64) {
		t.Helper()
		for {
			info, err := s.c.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.AckFloor.Consumer >= count && info.NumAckPending == 0 && info.NumPending == 0 {
				return
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("mandate delivery not broker-acknowledged")
			}
		}
	}
	gate := func(reg *comp.MandateRegistry, tenant, order string, allowed, unreadable bool) {
		t.Helper()
		g := comp.NewPreTradeGate(comp.NewEngine(nil), comp.MapBookSource{portfolio: {PortfolioID: portfolio, BaseCurrency: "USD"}}, reg, nil, compliancebus.NewBusRecorder(producer, logger), logger)
		d, err := g.Evaluate(bus.WithTenantID(ctx, tenant), comp.OrderDelta{TenantID: tenant, PortfolioID: portfolio, InstrumentID: "A", SignedQuantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 10}, Currency: "USD", OrderID: order, Issuer: "operator:identity-test", AsOf: time.Now().UTC()})
		if err != nil || d.Allowed != allowed || d.Unreadable != unreadable {
			t.Fatalf("gate %s allowed=%v unreadable=%v error=%v", order, d.Allowed, d.Unreadable, err)
		}
		msg, err := reader.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		env, data, err := bus.Unframe(msg.Data())
		var log observationpb.DecisionLog
		if err != nil || proto.Unmarshal(data, &log) != nil || bus.Validate(env) != nil || env.GetTenantId() != tenant || log.GetAttributes()["order_id"] != order || log.GetAttributes()["allowed"] != fmt.Sprint(allowed) {
			t.Fatal("actual gate decision lost tenant/order/outcome")
		}
		decisionSequences = append(decisionSequences, meta.Sequence.Stream)
		if unreadable && log.GetAttributes()["not_evaluated"] != comp.CodeMandateUnreadable {
			t.Fatal("unreadable refusal not identified in the real decision FACT")
		}
		if err := msg.Ack(); err != nil {
			t.Fatal(err)
		}
	}
	poison := func(key string) {
		t.Helper()
		if err := producer.Publish(ctx, bus.Event{Subject: topicA, EventType: comp.EventTypeMandateChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: comp.Domain, EventTime: time.Now().UTC(), PartitionKey: key, TenantID: tenantA, PayloadSchemaRef: "lifecycle.v1.ConfigChanged:1", Payload: &lifecyclepb.ConfigChanged{ConfigKey: key, NewValue: setValue}}); err != nil {
			t.Fatal(err)
		}
	}
	live := start()
	awaitACK(live, 2)
	gate(live.reg, tenantB, id+"-victim-original", false, false)
	poison(comp.MandateConfigKey(tenantA, portfolio))
	awaitACK(live, 3)
	gate(live.reg, tenantA, id+"-trusted-poison", false, true)
	gate(live.reg, tenantB, id+"-victim-preserved", false, false)
	poison(comp.MandateConfigKey(tenantB, portfolio))
	awaitACK(live, 4)
	if live.reg.Dropped() != 1 || len(live.reg.Rejections()) != 1 || live.reg.Rejections()[0] != tenantA+"/"+portfolio {
		t.Fatal("foreign envelope/key changed the victim rejection set")
	}
	gate(live.reg, tenantB, id+"-foreign-key-preserved", false, false)
	live.stop()
	poison(comp.MandateConfigKey(tenantA, portfolio))
	restarted := start()
	awaitACK(restarted, 2)
	gate(restarted.reg, tenantA, id+"-retained-poison", false, true)
	gate(restarted.reg, tenantB, id+"-retained-victim", false, false)
	repair := mandate(tenantA, 10)
	repair.EffectiveAt = timestamppb.New(time.Now().UTC().Add(-time.Second))
	approved(repair)
	awaitACK(restarted, 3)
	gate(restarted.reg, tenantA, id+"-repair", true, false)
	gate(restarted.reg, tenantB, id+"-repair-victim", false, false)
	restarted.stop()
	repairedBoot := start()
	awaitACK(repairedBoot, 2)
	gate(repairedBoot.reg, tenantA, id+"-repaired-restart", true, false)
	gate(repairedBoot.reg, tenantB, id+"-repaired-restart-victim", false, false)
	repairedBoot.stop()
}
