package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/cashview"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/platform/subject"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/compliance/internal/monitor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type reevaluationTransport struct {
	*bus.NATSClient
	created chan [2]string
}

func (c *reevaluationTransport) SubscribeBroadcastObserved(ctx context.Context, filter string, h bus.Handler, ready func(), created bus.ConsumerCreated) error {
	return c.NATSClient.SubscribeBroadcastObserved(ctx, filter, h, ready, func(stream, consumer string) {
		if created != nil {
			created(stream, consumer)
		}
		c.created <- [2]string{stream, consumer}
	})
}

type reevaluationPublisher struct {
	*bus.Producer
	calls atomic.Int64
}

func (p *reevaluationPublisher) Publish(ctx context.Context, e bus.Event) error {
	p.calls.Add(1)
	return p.Producer.Publish(ctx, e)
}

type reevaluationObservedSweep struct {
	*monitor.Monitor
	done chan error
}

func (s reevaluationObservedSweep) ReevaluateAll(ctx context.Context) error {
	err := s.Monitor.ReevaluateAll(ctx)
	select {
	case s.done <- err:
	default:
	}
	return err
}

// These are the production plain broadcasts and composition-root handlers.
// Eligibility's deterministic interleaving is pinned in the monitor package;
// this fixture proves the real routes, ACKs, retained flats and output contracts.
func TestReevaluationJetStreamLiveCashScheduledFlatAndRestart(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL real canonical JetStream")
	}
	for _, path := range []string{"live", "cash", "scheduled"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			id := "reevaluation-" + uuid.NewString()
			tenant, portfolio, instrument := "tenant-"+id, "PF-"+id, "INST-"+id
			positionTopic := subject.PositionFor(tenant, portfolio, instrument)
			markTopic := "market.equity.trade"
			admin, err := nats.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(admin.Close)
			js, err := jetstream.New(admin)
			if err != nil {
				t.Fatal(err)
			}
			stream := func(topic string) jetstream.Stream {
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
			positions := stream(positionTopic)
			info, err := positions.Info(ctx)
			if err != nil || info.Config.Name != "POSITION" || info.Config.MaxMsgsPerSubject != 1 || info.Config.MaxAge != 0 {
				t.Fatalf("requires canonical never-ageing POSITION compaction: %+v %v", info, err)
			}
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			// Only this unique entity subject is reclaimed. Global cash and output
			// history belongs to the canonical streams and must never be purged.
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := positions.Purge(cleanup, jetstream.WithPurgeSubject(positionTopic)); err != nil {
					t.Error(err)
				}
			})
			input, err := bus.NewProducer(client, bus.ProducerConfig{Source: "compliance/reevaluation-input", ProducerVersion: "test", Tenant: tenant})
			if err != nil {
				t.Fatal(err)
			}
			out, err := bus.NewProducer(client, bus.ProducerConfig{Source: "compliance/reevaluation-output", ProducerVersion: "test"})
			if err != nil {
				t.Fatal(err)
			}
			pub := &reevaluationPublisher{Producer: out}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			reg := comp.NewMandateRegistry()
			if err := reg.Put(&compliancepb.Mandate{MandateId: "M-" + id, TenantId: tenant, PortfolioId: portfolio, Version: 7,
				Rules: []*compliancepb.Rule{{RuleId: "leverage", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
					Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{MaxGrossLeverage: dec(25, -1)}}}}}); err != nil {
				t.Fatal(err)
			}
			makeMonitor := func() (postTradeValuation, *monitor.Monitor) {
				v := buildPostTradeValuation(testConfig(t, time.Hour), prometheus.NewRegistry(), logger)
				m := monitor.NewMonitor(nil, reg, nil, monitor.NewEmitter(pub), compliancebus.NewBusRecorder(pub, logger), logger,
					monitor.WithCashSource(v.Cash), monitor.WithMarkSource(v.Marks))
				return v, m
			}
			type delivery struct {
				id  string
				err error
			}
			type subscription struct {
				consumer jetstream.Consumer
				seen     <-chan delivery
				stop     func()
			}
			start := func(topic string, h bus.EventHandler) subscription {
				t.Helper()
				transport := &reevaluationTransport{NATSClient: client, created: make(chan [2]string, 1)}
				consumer, err := bus.NewConsumer(transport)
				if err != nil {
					t.Fatal(err)
				}
				subCtx, stop := context.WithCancel(ctx)
				seen, done := make(chan delivery, 128), make(chan error, 1)
				go func() {
					done <- consumer.SubscribeBroadcast(subCtx, topic, func(c context.Context, env *envelopepb.Envelope, payload []byte) error {
						// Cash and marks have shared subjects; only this tenant is ours.
						if env.GetTenantId() != tenant {
							return nil
						}
						err := h(c, env, payload)
						select {
						case seen <- delivery{env.GetEventId(), err}:
						case <-subCtx.Done():
						}
						return err
					})
				}()
				var handle [2]string
				var once sync.Once
				finish := func() {
					once.Do(func() {
						stop()
						select {
						case err := <-done:
							if err != nil && !errors.Is(err, context.Canceled) {
								t.Error(err)
							}
						case <-time.After(5 * time.Second):
							t.Error("reevaluation broadcast did not stop")
						}
						if handle[0] != "" {
							cleanup, end := context.WithTimeout(context.Background(), 5*time.Second)
							defer end()
							if err := js.DeleteConsumer(cleanup, handle[0], handle[1]); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
								t.Error(err)
							}
						}
					})
				}
				t.Cleanup(finish)
				select {
				case handle = <-transport.created:
				case err := <-done:
					t.Fatalf("broadcast did not create consumer: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				owned, err := js.Consumer(ctx, handle[0], handle[1])
				if err != nil {
					t.Fatal(err)
				}
				return subscription{owned, seen, finish}
			}
			awaitACK := func(s subscription, eventID string) {
				t.Helper()
				for {
					select {
					case d := <-s.seen:
						if d.id != eventID {
							continue
						}
						if d.err != nil {
							t.Fatalf("production handler refused valid state: %v", d.err)
						}
					case <-ctx.Done():
						t.Fatal("canonical event did not reach handler")
					}
					break
				}
				for {
					i, err := s.consumer.Info(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if i.NumPending == 0 && i.NumAckPending == 0 && i.AckFloor.Consumer > 0 {
						return
					}
					select {
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal("canonical event was not broker-ACKed")
					}
				}
			}
			publish := func(topic, typ, domain, schema, key string, payload proto.Message, at time.Time) string {
				t.Helper()
				eventID, err := uuid.NewV7()
				if err != nil {
					t.Fatal(err)
				}
				if err := input.Publish(ctx, bus.Event{EventID: eventID.String(), Subject: topic, EventType: typ, Domain: domain,
					EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, EventTime: at,
					PartitionKey: key, PayloadSchemaRef: schema, Payload: payload}); err != nil {
					t.Fatal(err)
				}
				return eventID.String()
			}
			position := func(qty int64) (string, time.Time) {
				at := time.Now().UTC()
				return publish(positionTopic, subject.PositionChanged, "risk", "domain.v1.PositionState:1", positionTopic,
					&domainpb.PositionState{PortfolioId: portfolio, InstrumentId: instrument, Quantity: dec(qty, 0),
						MarketValue: &commonpb.Money{Amount: dec(qty*90, 0), CurrencyCode: "USD"}, AsOf: timestamppb.New(at)}, at), at
			}
			cash := func(total int64) string {
				at := time.Now().UTC()
				return publish(cashview.Subject, cashview.Subject, "accounting", "accounting.v1.PortfolioCashBalance:1", portfolio,
					&accountingpb.PortfolioCashBalance{PortfolioId: portfolio, BaseCurrency: "USD", Total: dec(total, 0),
						AsOf: timestamppb.New(at), KnowledgeTime: timestamppb.New(at), CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1}}, at)
			}
			mark := func(price int64) string {
				at := time.Now().UTC()
				return publish(markTopic, markTopic, "market", "market.v1.MarketDataEvent:1", instrument,
					&marketpb.MarketDataEvent{InstrumentId: instrument, EventTime: timestamppb.New(at),
						Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: dec(price, 0)}}}, at)
			}
			sweep := func(m *monitor.Monitor) {
				t.Helper()
				sweepCtx, stop := context.WithCancel(ctx)
				observed := reevaluationObservedSweep{m, make(chan error, 1)}
				done := make(chan struct{})
				go func() {
					defer close(done)
					reevaluateBooks(sweepCtx, observed, 10*time.Millisecond, logger, newSweepMetrics(10*time.Millisecond))
				}()
				defer func() {
					stop()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("actual scheduled loop did not stop")
					}
				}()
				select {
				case err := <-observed.done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("actual scheduled loop never completed a real monitor sweep")
				}
			}
			readers := make(map[string]jetstream.Consumer)
			for _, topic := range []string{comp.SubjectBreach, compliancebus.SubjectDecision} {
				s := stream(topic)
				name := id + "-" + strings.ReplaceAll(topic, ".", "_")
				c, err := s.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: name, FilterSubject: topic,
					DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if err := s.DeleteConsumer(cleanup, name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
						t.Error(err)
					}
				})
				readers[topic] = c
			}
			assertBreach := func(trigger compliancepb.BreachTrigger, ratio string, exact time.Time) {
				t.Helper()
				for _, topic := range []string{comp.SubjectBreach, compliancebus.SubjectDecision} {
					for {
						msg, err := readers[topic].Next(jetstream.FetchMaxWait(5 * time.Second))
						if err != nil {
							t.Fatal(err)
						}
						env, payload, err := bus.Unframe(msg.Data())
						if err != nil || bus.Validate(env) != nil {
							t.Fatal("output failed canonical framing/validation")
						}
						if err := msg.Ack(); err != nil {
							t.Fatal(err)
						}
						if env.GetTenantId() != tenant || env.GetPartitionKey() != portfolio {
							continue
						}
						if topic == comp.SubjectBreach {
							var br compliancepb.ComplianceBreach
							if err := proto.Unmarshal(payload, &br); err != nil {
								t.Fatal(err)
							}
							res := br.GetResult()
							if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || br.GetTrigger() != trigger ||
								br.GetPortfolioId() != portfolio || res.GetMandateId() != "M-"+id || res.GetMandateVersion() != 7 ||
								res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH || len(res.GetViolations()) != 1 ||
								res.GetViolations()[0].GetEvidence()["observed"] != ratio || res.GetViolations()[0].GetEvidence()["limit"] != "2.500000" ||
								!env.GetEventTime().AsTime().Equal(res.GetEvaluatedAt().AsTime()) || !br.GetDetectedAt().AsTime().Equal(res.GetEvaluatedAt().AsTime()) ||
								(!exact.IsZero() && !exact.Equal(res.GetEvaluatedAt().AsTime())) {
								t.Fatalf("wrong held-book breach attribution or independently derived ratio: %v", &br)
							}
						} else {
							var d observationpb.DecisionLog
							if err := proto.Unmarshal(payload, &d); err != nil {
								t.Fatal(err)
							}
							a := d.GetAttributes()
							if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_OBSERVATION || d.GetDecider() != compliancebus.Decider ||
								a["phase"] != comp.PhasePostTrade || a["status"] != "BREACH" || a["portfolio_id"] != portfolio ||
								a["mandate_id"] != "M-"+id || a["mandate_version"] != "7" || a["issuer"] != "compliance:monitor" ||
								a["trigger"] != strings.TrimPrefix(trigger.String(), "BREACH_TRIGGER_") ||
								(!exact.IsZero() && !exact.Equal(env.GetEventTime().AsTime())) {
								t.Fatalf("wrong actual decision observation: %v", &d)
							}
						}
						break
					}
				}
			}
			v, mon := makeMonitor()
			marks, balances := start(markTopic, v.Marks.Handle), start(cashview.Subject, v.CashHandler(mon))
			awaitACK(marks, mark(100))
			awaitACK(balances, cash(-50_000))
			active := start(positionTopic, mon.Handle)
			seed, _ := position(1_000) // market gross 100k / equity 50k = 2, despite cost 90k
			awaitACK(active, seed)
			if pub.calls.Load() != 0 {
				t.Fatal("healthy held book or unknown portfolio cash announcement emitted a result")
			}
			trigger, ratio, exact := compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE, "3.333333", time.Time{}
			switch path {
			case "live":
				var event string
				event, exact = position(600) // market gross 60k / equity 10k = 6
				awaitACK(active, event)
				trigger, ratio = compliancepb.BreachTrigger_BREACH_TRIGGER_POSITION_CHANGE, "6.000000"
			case "cash":
				awaitACK(balances, cash(-70_000)) // gross 100k / equity 30k = 10/3
			case "scheduled":
				awaitACK(marks, mark(80)) // gross 80k / equity 30k = 8/3; no position or cash change
				sweep(mon)
				ratio = "2.666667"
			}
			assertBreach(trigger, ratio, exact)
			sweep(mon) // the held breach is already recorded, so no duplicate
			if pub.calls.Load() != 2 {
				t.Fatal("expected one real decision and breach for the held-book transition")
			}
			flat, _ := position(0)
			awaitACK(active, flat)
			awaitACK(balances, cash(-70_000))
			sweep(mon)
			if pub.calls.Load() != 2 {
				t.Fatal("valid last-flat, subsequent cash announcement or scheduled sweep emitted a phantom result")
			}
			active.stop()
			marks.stop()
			balances.stop()
			retained, err := positions.GetLastMsgForSubject(ctx, positionTopic)
			if err != nil {
				t.Fatal(err)
			}
			env, payload, err := bus.Unframe(retained.Data)
			var ps domainpb.PositionState
			if err != nil || bus.Validate(env) != nil || env.GetEventId() != flat || proto.Unmarshal(payload, &ps) != nil ||
				ps.GetQuantity() == nil || ps.GetQuantity().GetCoefficient() != 0 || ps.GetInstrumentId() != instrument {
				t.Fatal("POSITION did not retain the canonical explicit flat")
			}
			freshValuation, fresh := makeMonitor()
			freshMarks := start(markTopic, freshValuation.Marks.Handle)
			freshCash := start(cashview.Subject, freshValuation.CashHandler(fresh))
			awaitACK(freshMarks, mark(100))
			awaitACK(freshCash, cash(-50_000))
			restarted := start(positionTopic, fresh.Handle)
			awaitACK(restarted, flat) // real last-per-subject replay, no manual fold
			awaitACK(freshCash, cash(-50_000))
			sweep(fresh)
			if pub.calls.Load() != 2 {
				t.Fatal("fresh retained-flat rebuild fabricated a decision or breach")
			}
			reopen, _ := position(1_000)
			awaitACK(restarted, reopen)
			rebreach, at := position(600)
			awaitACK(restarted, rebreach)
			assertBreach(compliancepb.BreachTrigger_BREACH_TRIGGER_POSITION_CHANGE, "6.000000", at)
			if pub.calls.Load() != 4 {
				t.Fatal("flat/restart suppressed the next genuine breach or duplicated its decision")
			}
		})
	}
}
