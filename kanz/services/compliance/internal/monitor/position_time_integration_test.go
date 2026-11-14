package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/platform/subject"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Observe the existing transport's broker-generated consumer identity. This
// delegates every delivery/ACK to the real Consumer and NATSClient unchanged.
type positionTimeTransport struct {
	*bus.NATSClient
	created chan [2]string
}

func (c *positionTimeTransport) SubscribeBroadcastObserved(ctx context.Context, filter string, h bus.Handler, ready func(), created bus.ConsumerCreated) error {
	return c.NATSClient.SubscribeBroadcastObserved(ctx, filter, h, ready, func(stream, consumer string) {
		if created != nil {
			created(stream, consumer)
		}
		c.created <- [2]string{stream, consumer}
	})
}

type positionTimeDelivery struct {
	eventID string
	changed bool
	err     error
}

func positionTimeStream(t *testing.T, ctx context.Context, js jetstream.JetStream, topic string) jetstream.Stream {
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

func positionTimeAwaitACK(t *testing.T, ctx context.Context, c jetstream.Consumer, count uint64) {
	t.Helper()
	for {
		info, err := c.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Consumer >= count {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("position delivery was not broker-acknowledged")
		}
	}
}

func TestPositionTimeJetStreamACKReplayAndDecisionRecovery(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL real JetStream")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id := fmt.Sprintf("position-time-%d", time.Now().UnixNano())
	tenant, portfolio := id, "PF-"+id
	topics := []string{subject.PositionFor(tenant, portfolio, "A"), subject.PositionFor(tenant, portfolio, "B")}
	// Derived from a canonical entity subject; narrow the fixture to its own
	// portfolio while using the same broadcast/last-per-subject subscription.
	filter := strings.TrimSuffix(topics[0], "."+subject.Token("A")) + ".>"
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "POSITION_TIME_"+strings.ReplaceAll(id, "-", "_"), []string{subject.PositionAll})
	bustest.EnsureSubjects(t, ctx, js, "POSITION_TIME_BREACH_"+strings.ReplaceAll(id, "-", "_"), []string{comp.SubjectBreach})
	bustest.EnsureSubjects(t, ctx, js, "POSITION_TIME_DECISION_"+strings.ReplaceAll(id, "-", "_"), []string{compliancebus.SubjectDecision})
	input := positionTimeStream(t, ctx, js, topics[0])
	breaches := positionTimeStream(t, ctx, js, comp.SubjectBreach)
	decisions := positionTimeStream(t, ctx, js, compliancebus.SubjectDecision)
	// Register before the first publish. Never purge a shared stream or a shared
	// breach/decision subject: only these two unique compacted input subjects.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, topic := range topics {
			if strings.ContainsAny(topic, "*>") || !strings.Contains(topic, subject.Token(portfolio)) {
				t.Error("unsafe fixture cleanup subject")
				continue
			}
			if err := input.Purge(cleanup, jetstream.WithPurgeSubject(topic)); err != nil {
				t.Error(err)
			}
		}
	})
	output := func(s jetstream.Stream, topic, name string) jetstream.Consumer {
		t.Helper()
		c, err := s.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: id + name, FilterSubject: topic, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := s.DeleteConsumer(cleanup, id+name); err != nil {
				t.Error(err)
			}
		})
		return c
	}
	breachReader, decisionReader := output(breaches, comp.SubjectBreach, "-breach"), output(decisions, compliancebus.SubjectDecision, "-decision")
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	inProducer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "position-time-test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	// Match production compliance: no producer-level tenant fallback can hide a
	// lost inbound tenant on either the recorder or the breach emitter.
	outProducer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "compliance", ProducerVersion: "position-time-test"})
	if err != nil {
		t.Fatal(err)
	}
	registry := comp.NewMandateRegistry()
	if err := registry.Put(&compliancepb.Mandate{MandateId: "M-" + id, TenantId: tenant, PortfolioId: portfolio, Version: 1, EffectiveAt: timestamppb.New(t0), Rules: []*compliancepb.Rule{{RuleId: "cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION, Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT, MaxWeight: decv(60, -2)}}}}}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	makeMonitor := func() *Monitor {
		m := NewMonitor(nil, registry, nil, NewEmitter(outProducer), compliancebus.NewBusRecorder(outProducer, logger), logger)
		m.now = clockAt(t0.Add(180 * 24 * time.Hour))
		return m
	}
	type subscription struct {
		c    jetstream.Consumer
		seen <-chan positionTimeDelivery
		stop func()
	}
	start := func(m *Monitor) subscription {
		t.Helper()
		transport := &positionTimeTransport{NATSClient: client, created: make(chan [2]string, 1)}
		consumer, err := bus.NewConsumer(transport)
		if err != nil {
			t.Fatal(err)
		}
		subCtx, stop := context.WithCancel(ctx)
		ready, done, seen := make(chan struct{}), make(chan error, 1), make(chan positionTimeDelivery, 32)
		go func() {
			done <- consumer.SubscribeBroadcastReady(subCtx, filter, func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				before, gc := positionTimeBooks(m), m.lastBookGC
				err := m.Handle(ctx, env, payload)
				changed := !reflect.DeepEqual(before, positionTimeBooks(m)) || !gc.Equal(m.lastBookGC)
				select {
				case seen <- positionTimeDelivery{env.GetEventId(), changed, err}:
				case <-subCtx.Done():
				}
				return err
			}, func() { close(ready) })
		}()
		var handle [2]string
		select {
		case handle = <-transport.created:
		case err := <-done:
			stop()
			t.Fatalf("broadcast did not create consumer: %v", err)
		case <-ctx.Done():
			stop()
			t.Fatal(ctx.Err())
		}
		c, err := js.Consumer(ctx, handle[0], handle[1])
		if err != nil {
			stop()
			t.Fatal(err)
		}
		stopped := false
		finish := func() {
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
			case <-time.After(5 * time.Second):
				t.Error("broadcast did not stop")
			}
			cleanup, end := context.WithTimeout(context.Background(), 5*time.Second)
			defer end()
			if err := js.DeleteConsumer(cleanup, handle[0], handle[1]); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
				t.Error(err)
			}
		}
		t.Cleanup(finish)
		select {
		case <-ready:
		case <-ctx.Done():
			finish()
			t.Fatal(ctx.Err())
		}
		return subscription{c, seen, finish}
	}
	await := func(s subscription, changed bool) positionTimeDelivery {
		t.Helper()
		select {
		case d := <-s.seen:
			if d.err != nil || d.changed != changed {
				t.Fatalf("unexpected handler disposition: %+v want changed=%v", d, changed)
			}
			return d
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return positionTimeDelivery{}
		}
	}
	observed := time.Unix(1700000000, 123456789).UTC()
	publish := func(instrument string, qty, value int64, stamp *timestamppb.Timestamp) {
		t.Helper()
		data := positionTimePayload(t, portfolio, instrument, qty, value, stamp)
		var payload domainpb.PositionState
		if err := proto.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		// Producer allocates a UUIDv7 and the matching FACT idempotency key.
		if err := inProducer.Publish(ctx, bus.Event{Subject: subject.PositionFor(tenant, portfolio, instrument), EventType: subject.PositionChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "risk", EventTime: observed, PartitionKey: portfolio, CorrelationID: id, TraceContext: "00-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 16) + "-01", PayloadSchemaRef: "domain.v1.PositionState:1", Payload: &payload}); err != nil {
			t.Fatal(err)
		}
	}
	next := func(c jetstream.Consumer, s jetstream.Stream) (*envelopepb.Envelope, []byte) {
		t.Helper()
		for {
			msg, err := c.Next(jetstream.FetchMaxWait(5 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			env, data, err := bus.Unframe(msg.Data())
			if err != nil || bus.Validate(env) != nil {
				t.Fatal("output failed canonical envelope validation")
			}
			if env.GetCorrelationId() != id {
				if err := msg.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if env.GetTenantId() != tenant {
				t.Fatal("compliance output lost inbound tenant")
			}
			meta, err := msg.Metadata()
			if err != nil {
				t.Fatal(err)
			}
			retained, err := s.GetMsg(ctx, meta.Sequence.Stream)
			if err != nil || !bytes.Equal(retained.Data, msg.Data()) {
				t.Fatal("output was not retained with original bytes")
			}
			if err := msg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
			return env, data
		}
	}
	pair := func(at time.Time, eventID string, trigger compliancepb.BreachTrigger) {
		t.Helper()
		env, data := next(decisionReader, decisions)
		var decision observationpb.DecisionLog
		if proto.Unmarshal(data, &decision) != nil || env.GetEventType() != compliancebus.DecisionEventType || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_OBSERVATION || !env.GetEventTime().AsTime().Equal(at) || env.GetCausationId() != eventID || decision.GetDecider() != compliancebus.Decider || decision.Attributes["portfolio_id"] != portfolio || decision.Attributes["phase"] != comp.PhasePostTrade || decision.Attributes["status"] != "BREACH" {
			t.Fatal("real decision recorder lost observation time, tenant, lineage or verdict")
		}
		env, data = next(breachReader, breaches)
		var breach compliancepb.ComplianceBreach
		if proto.Unmarshal(data, &breach) != nil || env.GetEventType() != comp.EventTypeBreach || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || !env.GetEventTime().AsTime().Equal(at) || env.GetCausationId() != eventID || breach.GetPortfolioId() != portfolio || breach.GetMandateId() != "M-"+id || breach.GetTrigger() != trigger || !breach.GetDetectedAt().AsTime().Equal(at) || !breach.GetResult().GetEvaluatedAt().AsTime().Equal(at) || breach.GetResult().GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
			t.Fatal("real breach emitter lost exact time, mandate, lineage or trigger")
		}
	}
	quiet := func() {
		t.Helper()
		for _, c := range []jetstream.Consumer{decisionReader, breachReader} {
			for {
				info, err := c.Info(ctx)
				if err != nil || info.NumAckPending != 0 {
					t.Fatal("output acknowledgement was not completed")
				}
				if info.NumPending == 0 {
					break
				}
				msg, err := c.Next(jetstream.FetchMaxWait(5 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				env, _, err := bus.Unframe(msg.Data())
				if err == nil && env.GetCorrelationId() == id {
					t.Fatal("malformed or non-transition update produced extra decision/breach output")
				}
				// Other fixtures may use the shared output subjects. Acknowledge
				// only this private consumer's copy, without deleting their records.
				if err := msg.DoubleAck(ctx); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	first := makeMonitor()
	live := start(first)
	publish("A", 1, 100, timestamppb.New(observed))
	a := await(live, true)
	positionTimeAwaitACK(t, ctx, live.c, 1)
	pair(observed, a.eventID, compliancepb.BreachTrigger_BREACH_TRIGGER_POSITION_CHANGE)
	publish("B", 1, 100, timestamppb.New(observed))
	seedB := await(live, true)
	positionTimeAwaitACK(t, ctx, live.c, 2)
	quiet()
	// Known book: 100/(100+100)=1/2 is below the 3/5 cap. A malformed
	// 300/(300+100)=3/4 update or a flat removal must not change that book.
	before := positionTimeBooks(first)
	delivered := uint64(2)
	for _, bad := range positionTimeMalformed() {
		for _, flat := range []bool{false, true} {
			value, qty := int64(300), int64(1)
			if flat {
				value, qty = 0, 0
			}
			publish("A", qty, value, bad.at)
			await(live, false)
			delivered++
			positionTimeAwaitACK(t, ctx, live.c, delivered)
			if !reflect.DeepEqual(before, positionTimeBooks(first)) {
				t.Fatal("malformed broker input changed known book/status")
			}
			quiet()
		}
	}
	retained, err := input.GetLastMsgForSubject(ctx, topics[0])
	if err != nil {
		t.Fatal(err)
	}
	retainedEnv, raw, err := bus.Unframe(retained.Data)
	var last domainpb.PositionState
	if err != nil || proto.Unmarshal(raw, &last) != nil || last.AsOf == nil || last.GetAsOf().CheckValid() == nil {
		t.Fatal("malformed position was not retained for restart proof")
	}
	live.stop()
	// The fresh broadcast replays only each subject's latest frame: malformed A
	// remains inert, while valid B is a real 100%-weight holding and breaches.
	restarted := makeMonitor()
	replay := start(restarted)
	var b positionTimeDelivery
	for range 2 {
		select {
		case d := <-replay.seen:
			if d.err != nil {
				t.Fatal(d.err)
			}
			if d.changed {
				if d.eventID != seedB.eventID {
					t.Fatal("restart changed the original valid B event identity")
				}
				b = d
			} else if d.eventID != retainedEnv.GetEventId() {
				t.Fatal("restart did not acknowledge the original retained malformed A event")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	positionTimeAwaitACK(t, ctx, replay.c, 2)
	stillRetained, err := input.GetMsg(ctx, retained.Sequence)
	if err != nil || !bytes.Equal(stillRetained.Data, retained.Data) {
		t.Fatal("restart changed or lost the original malformed retained frame")
	}
	state := positionTimeBooks(restarted)[bookKey{tenant, portfolio}]
	if len(state.positions) != 1 || state.positions["B"].Quantity == nil || state.positions["A"].Quantity != nil {
		t.Fatal("retained malformed A entered the restarted book")
	}
	pair(observed, b.eventID, compliancepb.BreachTrigger_BREACH_TRIGGER_POSITION_CHANGE)
	quiet()
	recoveryAt := observed.Add(time.Second)
	publish("A", 1, 100, timestamppb.New(recoveryAt))
	await(replay, true)
	positionTimeAwaitACK(t, ctx, replay.c, 3)
	quiet()
	state = positionTimeBooks(restarted)[bookKey{tenant, portfolio}]
	if len(state.positions) != 2 || state.lastStatus != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatal("valid recovery failed to restore the independent 50% clean book")
	}
	breachAt := observed.Add(2*time.Second + time.Nanosecond)
	publish("A", 1, 300, timestamppb.New(breachAt))
	d := await(replay, true)
	positionTimeAwaitACK(t, ctx, replay.c, 4)
	pair(breachAt, d.eventID, compliancepb.BreachTrigger_BREACH_TRIGGER_MARKET_MOVE)
	quiet()
	// Each accepted position advances its lifecycle fences, including a newer
	// observation of identical holdings. Every other book field and the standing
	// breach stay unchanged; malformed frames above still compare the full book.
	expected := positionTimeBooks(restarted)
	key := bookKey{tenant, portfolio}
	standing := expected[key]
	standing.revision++
	standing.nextCapture++
	standing.appliedCapture++
	expected[key] = standing
	restarted.mu.Lock()
	gcBeforeRepeat := restarted.lastBookGC
	restarted.mu.Unlock()
	publish("A", 1, 300, timestamppb.New(breachAt.Add(time.Second)))
	await(replay, true)
	positionTimeAwaitACK(t, ctx, replay.c, 5)
	restarted.mu.Lock()
	gcUnchanged := restarted.lastBookGC.Equal(gcBeforeRepeat)
	restarted.mu.Unlock()
	if !reflect.DeepEqual(expected, positionTimeBooks(restarted)) || !gcUnchanged {
		t.Fatal("valid repeated position must advance only its lifecycle fences")
	}
	quiet()
	t.Log("real canonical JetStream ACK/retained replay and valid recovery passed; real BusRecorder and Emitter preserved tenant, exact observation nanoseconds and lineage")
}
