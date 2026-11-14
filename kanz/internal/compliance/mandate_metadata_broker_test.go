package compliance_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/platform/subject"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestMandateMetadataCanonicalReplayGateAndRepair(t *testing.T) {
	if os.Getenv("TEST_NATS_URL") == "" {
		t.Skip("requires TEST_NATS_URL with canonical MANDATE and PLATFORM topology")
	}
	for _, field := range []string{"mandate_id", "version", "effective_at"} {
		t.Run(field, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			id := "metadata-" + strings.ReplaceAll(uuid.NewString(), "-", "")
			tenant, victimTenant, portfolio := id+"-a", id+"-b", "PF-"+id
			topic, victimTopic := comp.SubjectMandateFor(tenant, portfolio), comp.SubjectMandateFor(victimTenant, portfolio)
			filter := comp.SubjectMandateChanged + ".*." + subject.Token(portfolio)
			admin, err := nats.Connect(os.Getenv("TEST_NATS_URL"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(admin.Close)
			js, err := jetstream.New(admin)
			if err != nil {
				t.Fatal(err)
			}
			mandates, err := js.Stream(ctx, "MANDATE")
			if err != nil {
				t.Fatal(err)
			}
			info, err := mandates.Info(ctx)
			if err != nil || info.Config.MaxMsgsPerSubject != 1 || info.Config.MaxAge != 0 {
				t.Fatalf("fixture requires canonical never-ageing compacted MANDATE: %v", err)
			}
			decisionStream, err := js.StreamNameBySubject(ctx, compliancebus.SubjectDecision)
			if err != nil || decisionStream != "PLATFORM" {
				t.Fatalf("fixture requires canonical PLATFORM decision route: %s %v", decisionStream, err)
			}
			decisions, err := js.Stream(ctx, decisionStream)
			if err != nil {
				t.Fatal(err)
			}
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: os.Getenv("TEST_NATS_URL"), Name: id})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			var consumers [][2]string
			var decisionSequences []uint64
			// Registered before subscriptions: all cancel/join hooks run before
			// these exact owned consumers/messages are removed and connections close.
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				for _, c := range consumers {
					if err := js.DeleteConsumer(cleanup, c[0], c[1]); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
						t.Error(err)
					}
				}
				for _, seq := range decisionSequences {
					if err := decisions.DeleteMsg(cleanup, seq); err != nil {
						t.Error(err)
					}
				}
				for _, ownTopic := range []string{topic, victimTopic} {
					if !strings.HasSuffix(ownTopic, "."+subject.Token(portfolio)) || strings.ContainsAny(ownTopic, "*>") {
						t.Error("unsafe mandate fixture reclaim")
						continue
					}
					if err := mandates.Purge(cleanup, jetstream.WithPurgeSubject(ownTopic)); err != nil {
						t.Error(err)
					}
				}
			})
			producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "kanz-mandate", ProducerVersion: "metadata-test"})
			if err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			publisher := comp.NewPublisher(producer, client)
			approved := func(m *compliancepb.Mandate) {
				t.Helper()
				if err := publisher.Publish(ctx, m, approvalFor(t, m, id), id); err != nil {
					t.Fatal(err)
				}
			}
			waitACK := func(c jetstream.Consumer, count uint64) {
				t.Helper()
				for {
					info, err := c.Info(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if info.AckFloor.Consumer >= count && info.NumPending == 0 && info.NumAckPending == 0 {
						return
					}
					select {
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						t.Fatalf("consumer %s did not ACK %d deliveries", info.Name, count)
					}
				}
			}
			type session struct {
				reg  *comp.MandateRegistry
				c    jetstream.Consumer
				stop func()
			}
			start := func() session {
				t.Helper()
				reg := comp.NewMandateRegistry(comp.WithMandateLogger(logger))
				transport := &identityObservedTransport{client, make(chan [2]string, 1)}
				consumer, err := bus.NewConsumer(transport)
				if err != nil {
					t.Fatal(err)
				}
				subCtx, stop := context.WithCancel(ctx)
				ready, done := make(chan struct{}), make(chan error, 1)
				var once sync.Once
				join := func() {
					once.Do(func() {
						stop()
						select {
						case err := <-done:
							if err != nil && !errors.Is(err, context.Canceled) {
								t.Error(err)
							}
						case <-time.After(5 * time.Second):
							t.Error("mandate subscription did not join")
						}
					})
				}
				t.Cleanup(join)
				go func() {
					defer close(done)
					done <- consumer.SubscribeBroadcastReady(subCtx, filter, comp.NewMandateConsumer(reg, logger).Handle, func() { reg.Arm(); close(ready) })
				}()
				var owned [2]string
				select {
				case owned = <-transport.created:
					consumers = append(consumers, owned)
				case err := <-done:
					t.Fatalf("mandate subscription stopped before creation: %v", err)
				case <-ctx.Done():
					t.Fatal("mandate broker identity was not observed")
				}
				c, err := js.Consumer(ctx, owned[0], owned[1])
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("mandate replay did not arm after ACK")
				}
				return session{reg, c, join}
			}
			reader, err := decisions.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: id, FilterSubject: compliancebus.SubjectDecision, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
			if err != nil {
				t.Fatal(err)
			}
			consumers = append(consumers, [2]string{decisionStream, id})
			gate := func(reg *comp.MandateRegistry, who, phase string, allowed, unreadable bool) {
				t.Helper()
				orderID := id + "-" + phase
				g := comp.NewPreTradeGate(comp.NewEngine(nil), comp.MapBookSource{portfolio: {PortfolioID: portfolio, BaseCurrency: "USD"}}, reg, nil, compliancebus.NewBusRecorder(producer, logger), logger, comp.WithRequireMandate(true))
				d, err := g.Evaluate(bus.WithTenantID(ctx, who), comp.OrderDelta{TenantID: who, PortfolioID: portfolio, InstrumentID: "A", SignedQuantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 10}, Currency: "USD", OrderID: orderID, Issuer: "operator:metadata-test", AsOf: time.Now().UTC()})
				if err != nil || d.Allowed != allowed || d.Unreadable != unreadable || d.Ungoverned {
					t.Fatalf("gate %s: %+v %v", phase, d, err)
				}
				for {
					msg, err := reader.Next(jetstream.FetchMaxWait(5 * time.Second))
					if err != nil {
						t.Fatal(err)
					}
					env, payload, err := bus.Unframe(msg.Data())
					var decision observationpb.DecisionLog
					if err != nil || proto.Unmarshal(payload, &decision) != nil || bus.Validate(env) != nil {
						t.Fatal("decision FACT was not canonical")
					}
					if decision.GetAttributes()["order_id"] != orderID {
						if err := msg.Ack(); err != nil {
							t.Fatal(err)
						}
						continue
					}
					meta, err := msg.Metadata()
					if err != nil {
						t.Fatal(err)
					}
					decisionSequences = append(decisionSequences, meta.Sequence.Stream)
					if env.GetTenantId() != who || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || decision.GetAttributes()["allowed"] != fmt.Sprint(allowed) || unreadable && decision.GetAttributes()["not_evaluated"] != comp.CodeMandateUnreadable {
						t.Fatalf("decision FACT lost refusal/identity: %v %v", env, &decision)
					}
					if err := msg.Ack(); err != nil {
						t.Fatal(err)
					}
					waitACK(reader, meta.Sequence.Consumer)
					return
				}
			}
			now := time.Now().UTC()
			current := mandateVersion(tenant, portfolio, 1, time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC))
			scheduled := mandateVersion(tenant, portfolio, 20, now.Add(48*time.Hour))
			victim := mandateVersion(victimTenant, portfolio, 1, now.Add(-time.Hour))
			victim.Rules = []*compliancepb.Rule{{RuleId: "deny-A", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION, Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT, Mode: compliancepb.RestrictionMode_RESTRICTION_MODE_DENY, Values: []string{"A"}}}}}
			approved(current)
			approved(scheduled)
			approved(victim)
			live := start()
			waitACK(live.c, 2)
			gate(live.reg, tenant, "before-poison", true, false)
			bad := metadataIncomplete(mandateVersion(tenant, portfolio, 9, now.Add(24*time.Hour)), field)
			value := metadataRetainedValue(t, "bad_last", current, bad, scheduled)
			if err := producer.Publish(ctx, bus.Event{Subject: topic, EventType: comp.EventTypeMandateChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: comp.Domain, EventTime: now, PartitionKey: comp.MandateConfigKey(tenant, portfolio), TenantID: tenant, PayloadSchemaRef: "lifecycle.v1.ConfigChanged:1", Payload: &lifecyclepb.ConfigChanged{ConfigKey: comp.MandateConfigKey(tenant, portfolio), NewValue: value}}); err != nil {
				t.Fatal(err)
			}
			waitACK(live.c, 3)
			if !live.reg.Armed() || live.reg.Complete() {
				t.Fatal("ACK/armed registry claimed poisoned metadata was complete")
			}
			gate(live.reg, tenant, "warm-poison", false, true)
			gate(live.reg, tenant, "repeat-poison", false, true)
			gate(live.reg, victimTenant, "warm-victim", false, false)
			live.stop()
			cold := start()
			waitACK(cold.c, 2)
			gate(cold.reg, tenant, "cold-poison", false, true)
			gate(cold.reg, victimTenant, "cold-victim", false, false)
			_, _, poisonSequence, err := client.LastOnSubject(ctx, topic)
			if err != nil {
				t.Fatal(err)
			}
			repair := mandateVersion(tenant, portfolio, 10, now.Add(36*time.Hour))
			approved(repair)
			waitACK(cold.c, 3)
			retained, err := mandates.GetLastMsgForSubject(ctx, topic)
			if err != nil {
				t.Fatal(err)
			}
			_, payload, err := bus.Unframe(retained.Data)
			var changed lifecyclepb.ConfigChanged
			if err != nil || proto.Unmarshal(payload, &changed) != nil || changed.PreviousValue != value || changed.ChangedBy == "" || changed.ApprovedBy == "" || changed.ChangedBy == changed.ApprovedBy || retained.Header.Get("Nats-Expected-Last-Subject-Sequence") != strconv.FormatUint(poisonSequence, 10) {
				t.Fatalf("repair lost approved prior bytes/CAS evidence: %v", err)
			}
			members, err := comp.DecodeMandateValue(changed.NewValue)
			if err != nil || len(members) != 3 || !proto.Equal(members[0], current) || !proto.Equal(members[1], repair) || !proto.Equal(members[2], scheduled) {
				t.Fatalf("retained repair changed valid current/future members: %v %v", members, err)
			}
			assertSchedule := func(reg *comp.MandateRegistry) {
				t.Helper()
				for _, m := range []*compliancepb.Mandate{current, repair, scheduled} {
					got, gov, err := reg.Mandate(ctx, tenant, portfolio, m.EffectiveAt.AsTime())
					if err != nil || gov != comp.Governed || !proto.Equal(got, m) {
						t.Fatalf("retained repaired schedule differs: %v %v", got, err)
					}
				}
				if !reg.Complete() || len(reg.Rejections()) != 0 {
					t.Fatal("valid repair left unreadable metadata")
				}
			}
			assertSchedule(cold.reg)
			gate(cold.reg, tenant, "repair", true, false)
			cold.stop()
			repaired := start()
			waitACK(repaired.c, 2)
			assertSchedule(repaired.reg)
			gate(repaired.reg, tenant, "repair-restart", true, false)
			gate(repaired.reg, victimTenant, "repair-restart-victim", false, false)
			repaired.stop()
		})
	}
}
