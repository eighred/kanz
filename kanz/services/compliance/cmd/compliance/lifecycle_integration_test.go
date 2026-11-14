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

type lifecycleGate struct {
	comp.MandateSource
	armed, used      atomic.Bool
	entered, release chan struct{}
}

func (g *lifecycleGate) Mandate(ctx context.Context, tenant, portfolio string, at time.Time) (*compliancepb.Mandate, comp.Governance, error) {
	if g.armed.Load() && g.used.CompareAndSwap(false, true) {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, comp.NeverMandated, ctx.Err()
		}
	}
	return g.MandateSource.Mandate(ctx, tenant, portfolio, at)
}

type lifecyclePublisher struct {
	*bus.Producer
	failHistorical bool
	failed         atomic.Bool
	accepted       atomic.Int64
}

func (p *lifecyclePublisher) Publish(ctx context.Context, e bus.Event) error {
	if br, ok := e.Payload.(*compliancepb.ComplianceBreach); ok && p.failHistorical && br.GetTrigger() == compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE && p.failed.CompareAndSwap(false, true) {
		return errors.New("fixture: historical breach publication failed")
	}
	if err := p.Producer.Publish(ctx, e); err != nil {
		return err
	}
	p.accepted.Add(1)
	return nil
}

// Production has one POSITION broadcast, plus independent cash and sweep
// goroutines. Pause the latter two; the real single live subscriber supplies
// flat/reopening state. No second synthetic live subscriber forces the race.
func TestLifecycleCanonicalCashScheduledReopenAndRestart(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL canonical POSITION ACCOUNTING MARKET PLATFORM COMPLIANCE")
	}
	for _, path := range []string{"cash", "scheduled"} {
		for _, reopen := range []string{"healthy", "breach"} {
			for _, outcome := range []string{"success", "failure"} {
				t.Run(path+"-"+reopen+"-"+outcome, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
					defer cancel()
					id := "lifecycle-" + uuid.NewString()
					tenant, portfolio, instrument := "tenant-"+id, "PF-"+id, "INST-"+id
					positionTopic := subject.PositionFor(tenant, portfolio, instrument)
					nc, err := nats.Connect(url)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(nc.Close)
					js, err := jetstream.New(nc)
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
					positions := streamFor(positionTopic)
					info, err := positions.Info(ctx)
					if err != nil || info.Config.Name != "POSITION" || info.Config.MaxMsgsPerSubject != 1 || info.Config.MaxAge != 0 {
						t.Fatal("requires actual never-ageing compacted POSITION", err)
					}
					client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = client.Close() })
					t.Cleanup(func() {
						cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
						defer stop()
						if err := positions.Purge(cleanup, jetstream.WithPurgeSubject(positionTopic)); err != nil {
							t.Error(err)
						}
					})
					input, err := bus.NewProducer(client, bus.ProducerConfig{Source: "compliance/lifecycle-input", ProducerVersion: "test", Tenant: tenant})
					if err != nil {
						t.Fatal(err)
					}
					producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "compliance/lifecycle-output", ProducerVersion: "test"})
					if err != nil {
						t.Fatal(err)
					}
					pub := &lifecyclePublisher{Producer: producer, failHistorical: outcome == "failure"}
					logger := slog.New(slog.NewTextHandler(io.Discard, nil))
					registry := comp.NewMandateRegistry()
					if err := registry.Put(&compliancepb.Mandate{MandateId: "M-" + id, TenantId: tenant, PortfolioId: portfolio, Version: 7, Rules: []*compliancepb.Rule{{RuleId: "leverage", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE, Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{MaxGrossLeverage: dec(25, -1)}}}}}); err != nil {
						t.Fatal(err)
					}
					gate := &lifecycleGate{MandateSource: registry, entered: make(chan struct{}), release: make(chan struct{})}
					newMonitor := func(source comp.MandateSource) (postTradeValuation, *monitor.Monitor) {
						v := buildPostTradeValuation(testConfig(t, time.Hour), prometheus.NewRegistry(), logger)
						m := monitor.NewMonitor(nil, source, nil, monitor.NewEmitter(pub), compliancebus.NewBusRecorder(pub, logger), logger, monitor.WithCashSource(v.Cash), monitor.WithMarkSource(v.Marks))
						return v, m
					}
					type delivered struct {
						id  string
						err error
					}
					type subscription struct {
						consumer jetstream.Consumer
						seen     <-chan delivered
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
						seen, done := make(chan delivered, 64), make(chan error, 1)
						go func() {
							defer close(done)
							done <- consumer.SubscribeBroadcast(subCtx, topic, func(c context.Context, env *envelopepb.Envelope, payload []byte) error {
								if env.GetTenantId() != tenant {
									return nil
								}
								err := h(c, env, payload)
								select {
								case seen <- delivered{env.GetEventId(), err}:
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
									t.Error("lifecycle broadcast did not join")
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
							t.Fatal("broadcast did not create consumer", err)
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
						actual, err := js.Consumer(ctx, handle[0], handle[1])
						if err != nil {
							t.Fatal(err)
						}
						return subscription{actual, seen, finish}
					}
					awaitACK := func(s subscription, eventID string, wantFailure bool) {
						t.Helper()
						failed := false
						for {
							select {
							case d := <-s.seen:
								if d.id != eventID {
									continue
								}
								if d.err != nil {
									if !wantFailure {
										t.Fatal("valid lifecycle input failed", d.err)
									}
									failed = true
									continue
								}
								if wantFailure && !failed {
									t.Fatal("failed historical publish did not surface before source redelivery/ACK")
								}
								goto acknowledged
							case <-ctx.Done():
								t.Fatal("canonical lifecycle event never completed", ctx.Err())
							}
						}
					acknowledged:
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
								t.Fatal("canonical lifecycle event not ACKed", ctx.Err())
							}
						}
					}
					publish := func(topic, typ, domain, schema, key string, payload proto.Message, at time.Time) string {
						t.Helper()
						eventID, err := uuid.NewV7()
						if err != nil {
							t.Fatal(err)
						}
						if err := input.Publish(ctx, bus.Event{EventID: eventID.String(), Subject: topic, EventType: typ, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: domain, EventTime: at, PartitionKey: key, PayloadSchemaRef: schema, Payload: payload}); err != nil {
							t.Fatal(err)
						}
						return eventID.String()
					}
					position := func(quantity int64) (string, time.Time) {
						at := time.Now().UTC()
						return publish(positionTopic, subject.PositionChanged, "risk", "domain.v1.PositionState:1", positionTopic, &domainpb.PositionState{PortfolioId: portfolio, InstrumentId: instrument, Quantity: dec(quantity, 0), MarketValue: &commonpb.Money{Amount: dec(quantity*90, 0), CurrencyCode: "USD"}, AsOf: timestamppb.New(at)}, at), at
					}
					cash := func(total int64) string {
						at := time.Now().UTC()
						return publish(cashview.Subject, cashview.Subject, "accounting", "accounting.v1.PortfolioCashBalance:1", portfolio, &accountingpb.PortfolioCashBalance{PortfolioId: portfolio, BaseCurrency: "USD", Total: dec(total, 0), AsOf: timestamppb.New(at), KnowledgeTime: timestamppb.New(at), CurrencyCoverage: &domainpb.InputCoverage{Contributed: 1}}, at)
					}
					mark := func(price int64) string {
						at := time.Now().UTC()
						return publish("market.equity.trade", "market.equity.trade", "market", "market.v1.MarketDataEvent:1", instrument, &marketpb.MarketDataEvent{InstrumentId: instrument, EventTime: timestamppb.New(at), Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: dec(price, 0)}}}, at)
					}
					readers := map[string]jetstream.Consumer{}
					for _, topic := range []string{comp.SubjectBreach, compliancebus.SubjectDecision} {
						s := streamFor(topic)
						name := id + "-" + strings.ReplaceAll(topic, ".", "_")
						r, err := s.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: name, FilterSubject: topic, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
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
						readers[topic] = r
					}
					runSweep := func(m *monitor.Monitor) (<-chan error, func()) {
						observed := reevaluationObservedSweep{m, make(chan error, 8)}
						sweepCtx, stop := context.WithCancel(ctx)
						done := make(chan struct{})
						go func() {
							defer close(done)
							reevaluateBooks(sweepCtx, observed, 10*time.Millisecond, logger, newSweepMetrics(10*time.Millisecond))
						}()
						var once sync.Once
						join := func() {
							once.Do(func() {
								stop()
								select {
								case <-done:
								case <-time.After(5 * time.Second):
									t.Error("actual lifecycle sweep did not join")
								}
							})
						}
						t.Cleanup(join)
						return observed.done, join
					}
					awaitSweep := func(done <-chan error, wantFailure bool) {
						select {
						case err := <-done:
							if (err != nil) != wantFailure {
								t.Fatal("scheduled lifecycle outcome", err, wantFailure)
							}
						case <-ctx.Done():
							t.Fatal("actual lifecycle sweep did not complete", ctx.Err())
						}
					}
					v, m := newMonitor(gate)
					marks, balances := start("market.equity.trade", v.Marks.Handle), start(cashview.Subject, v.CashHandler(m))
					awaitACK(marks, mark(100), false)
					awaitACK(balances, cash(-50_000), false)
					active := start(positionTopic, m.Handle)
					seed, _ := position(1_000)
					awaitACK(active, seed, false)
					if pub.accepted.Load() != 0 {
						t.Fatal("healthy seed emitted a breach")
					}
					var releaseOnce sync.Once
					unblock := func() { releaseOnce.Do(func() { close(gate.release) }) }
					t.Cleanup(unblock)
					gate.armed.Store(true)
					var oldInput string
					var sweepDone <-chan error
					var stopSweep func()
					cashTotal, price, ratio := int64(-70_000), int64(100), "3.333333"
					if path == "cash" {
						oldInput = cash(cashTotal)
					} else {
						cashTotal, price, ratio = -50_000, 80, "2.666667"
						awaitACK(marks, mark(price), false)
						sweepDone, stopSweep = runSweep(m)
					}
					select {
					case <-gate.entered:
					case <-ctx.Done():
						t.Fatal("actual held evaluation never reached barrier", ctx.Err())
					}
					flat, flatAt := position(0)
					awaitACK(active, flat, false)
					quantity := int64(1_000)
					if reopen == "healthy" {
						quantity = 2_000
					}
					reopened, reopenAt := position(quantity)
					awaitACK(active, reopened, false)
					unblock()
					if path == "cash" {
						awaitACK(balances, oldInput, outcome == "failure")
					} else {
						awaitSweep(sweepDone, outcome == "failure")
						if outcome == "failure" {
							awaitSweep(sweepDone, false)
						}
						stopSweep()
					}
					if reopen == "healthy" {
						reopened, reopenAt = position(1_000)
						awaitACK(active, reopened, false)
					}
					// Old failures must not clear the current reopening verdict, and an old
					// historical breach must not suppress its distinct POSITION_CHANGE.
					awaitACK(balances, cash(cashTotal), false)
					done, stop := runSweep(m)
					awaitSweep(done, false)
					stop()
					wantAccepted := int64(4)
					if outcome == "failure" {
						wantAccepted = 3
					}
					if pub.accepted.Load() != wantAccepted {
						t.Fatalf("historical/reopening outputs=%d want%d", pub.accepted.Load(), wantAccepted)
					}
					// Compact to a real flat, stop all folds, then rebuild from the retained
					// level. Local status/capture/reservation identities must not survive it.
					lastFlat, _ := position(0)
					awaitACK(active, lastFlat, false)
					active.stop()
					marks.stop()
					balances.stop()
					retained, err := positions.GetLastMsgForSubject(ctx, positionTopic)
					if err != nil {
						t.Fatal(err)
					}
					env, payload, err := bus.Unframe(retained.Data)
					var state domainpb.PositionState
					if err != nil || bus.Validate(env) != nil || env.GetEventId() != lastFlat || proto.Unmarshal(payload, &state) != nil || state.GetQuantity() == nil || state.GetQuantity().GetCoefficient() != 0 {
						t.Fatal("retained POSITION is not the accepted flat")
					}
					freshV, fresh := newMonitor(registry)
					freshMarks, freshCash := start("market.equity.trade", freshV.Marks.Handle), start(cashview.Subject, freshV.CashHandler(fresh))
					awaitACK(freshMarks, mark(price), false)
					awaitACK(freshCash, cash(cashTotal), false)
					restarted := start(positionTopic, fresh.Handle)
					awaitACK(restarted, lastFlat, false)
					awaitACK(freshCash, cash(cashTotal), false)
					done, stop = runSweep(fresh)
					awaitSweep(done, false)
					stop()
					if pub.accepted.Load() != wantAccepted {
						t.Fatal("retained flat rebuilt a phantom decision/FACT")
					}
					freshPosition, freshAt := position(1_000)
					awaitACK(restarted, freshPosition, false)
					if pub.accepted.Load() != wantAccepted+2 {
						t.Fatal("cold rebuild suppressed the next genuinely held breach")
					}
					// Read and broker-ACK every accepted output, checking independent ratios
					// and causal/time attribution for historical and reopening observations.
					for _, topic := range []string{comp.SubjectBreach, compliancebus.SubjectDecision} {
						count := 3
						if topic == comp.SubjectBreach && outcome == "failure" {
							count = 2
						}
						seenHistorical, seenReopen, seenFresh := false, false, false
						for accepted := 0; accepted < count; {
							msg, err := readers[topic].Next(jetstream.FetchMaxWait(5 * time.Second))
							if err != nil {
								t.Fatal(err)
							}
							env, payload, err := bus.Unframe(msg.Data())
							if err != nil || bus.Validate(env) != nil {
								t.Fatal("lifecycle output failed canonical validation", err)
							}
							if err := msg.DoubleAck(ctx); err != nil {
								t.Fatal(err)
							}
							if env.GetTenantId() != tenant || env.GetPartitionKey() != portfolio {
								continue
							}
							accepted++
							trigger := ""
							at := env.GetEventTime().AsTime()
							if topic == comp.SubjectBreach {
								var br compliancepb.ComplianceBreach
								if err := proto.Unmarshal(payload, &br); err != nil {
									t.Fatal(err)
								}
								if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || br.GetPortfolioId() != portfolio || br.GetMandateVersion() != 7 || br.GetMandateId() != "M-"+id || br.GetResult().GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH || len(br.GetResult().GetViolations()) != 1 || br.GetResult().GetViolations()[0].GetEvidence()["observed"] != ratio || br.GetResult().GetViolations()[0].GetEvidence()["limit"] != "2.500000" || !br.GetDetectedAt().AsTime().Equal(at) || !br.GetResult().GetEvaluatedAt().AsTime().Equal(at) {
									t.Fatal("wrong independently derived lifecycle breach", &br)
								}
								trigger = strings.TrimPrefix(br.GetTrigger().String(), "BREACH_TRIGGER_")
							} else {
								var record observationpb.DecisionLog
								if err := proto.Unmarshal(payload, &record); err != nil {
									t.Fatal(err)
								}
								a := record.GetAttributes()
								if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_OBSERVATION || record.GetDecider() != compliancebus.Decider || a["phase"] != comp.PhasePostTrade || a["status"] != "BREACH" || a["portfolio_id"] != portfolio || a["mandate_id"] != "M-"+id || a["mandate_version"] != "7" || a["issuer"] != "compliance:monitor" {
									t.Fatal("decision lost lifecycle owner/policy", &record)
								}
								trigger = a["trigger"]
							}
							switch {
							case trigger == "MARKET_MOVE":
								if seenHistorical || !at.Before(flatAt) || (path == "cash" && env.GetCausationId() != oldInput) || (path == "scheduled" && env.GetCausationId() != "") {
									t.Fatal("historical decision lost its captured time/cause")
								}
								seenHistorical = true
							case at.Equal(reopenAt):
								if seenReopen || trigger != "POSITION_CHANGE" || env.GetCausationId() != reopened {
									t.Fatal("reopening decision lost attribution")
								}
								seenReopen = true
							case at.Equal(freshAt):
								if seenFresh || trigger != "POSITION_CHANGE" || env.GetCausationId() != freshPosition {
									t.Fatal("restarted reopening lost attribution")
								}
								seenFresh = true
							default:
								t.Fatal("unexpected lifecycle decision time/trigger", at, trigger)
							}
						}
						if !seenReopen || !seenFresh || seenHistorical != (topic == compliancebus.SubjectDecision || outcome == "success") {
							t.Fatal("missing historical/reopening/cold-rebuild evidence", seenHistorical, seenReopen, seenFresh)
						}
					}
				})
			}
		}
	}
}
