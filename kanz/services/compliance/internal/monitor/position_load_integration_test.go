package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/platform/subject"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
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

type loadPosition struct {
	portfolio string
	at        time.Time
	sent      time.Time
}

type loadDelivery struct {
	id     string
	at     time.Time
	start  time.Time
	finish time.Time
	retry  bool
	err    error
}

// TestPositionLoadJetStreamOrderedRecovery measures synthetic multi-book load
// through the actual serial broker consumer and rule engine. Each holding is
// published once during the initial backlog, so compaction cannot hide work.
func TestPositionLoadJetStreamOrderedRecovery(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires TEST_NATS_URL real JetStream")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	tenant := fmt.Sprintf("compliance-load-%d", time.Now().UnixNano())
	filter := subject.PositionChanged + "." + subject.Token(tenant) + ".>"
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	input := positionTimeStream(t, ctx, js, filter)
	conf, err := input.Info(ctx)
	if err != nil || conf.Config.MaxMsgsPerSubject != 1 || conf.Config.MaxAge != 0 {
		t.Fatalf("need the non-expiring compacted POSITION stream: %v", err)
	}
	for i, topic := range []string{comp.SubjectBreach, compliancebus.SubjectDecision} {
		bustest.EnsureSubjects(t, ctx, js, fmt.Sprintf("LOAD_%d_%s", i, tenant), []string{topic})
	}
	broker, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: tenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	publisher := func(source, scope string) *bus.Producer {
		t.Helper()
		p, err := bus.NewProducer(broker, bus.ProducerConfig{Source: source, Tenant: scope, ProducerVersion: "compliance-load-test"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	in, out := publisher("oms", tenant), publisher("compliance", "")
	outputs := make([]jetstream.Consumer, 0, 2)
	for index, topic := range []string{comp.SubjectBreach, compliancebus.SubjectDecision} {
		stream := positionTimeStream(t, ctx, js, topic)
		name := fmt.Sprintf("%s-output-%d", tenant, index)
		reader, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
			Name: name, FilterSubject: topic, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy,
			InactiveThreshold: 2 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, reader)
		t.Cleanup(func() { deleteLoadConsumer(t, stream, name) })
	}

	// Two 48-position books and four eight-position books, interleaved in one
	// stream. Each mandate caps the reviewed AAPL holding at exactly 60%.
	books := []string{"hot-0", "cold-0", "hot-1", "cold-1", "cold-2", "cold-3"}
	sizes := []int{48, 8, 48, 8, 8, 8}
	registry := comp.NewMandateRegistry()
	for _, book := range books {
		positionIdentityMandate(t, registry, tenant, book)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	makeMonitor := func() *Monitor {
		m := NewMonitor(nil, registry, nil, NewEmitter(out), compliancebus.NewBusRecorder(out, logger), logger)
		m.now = clockAt(t0.Add(24 * time.Hour))
		return m
	}
	seen := make(chan loadDelivery, 256)
	start := func(m *Monitor, failOnceAt time.Time) (jetstream.Consumer, func()) {
		t.Helper()
		subCtx, stop := context.WithCancel(ctx)
		transport := &positionTimeTransport{NATSClient: broker, created: make(chan [2]string, 1)}
		c, err := bus.NewConsumer(transport)
		if err != nil {
			stop()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			retry := !failOnceAt.IsZero()
			done <- c.SubscribeBroadcast(subCtx, filter, func(handlerCtx context.Context, env *envelopepb.Envelope, data []byte) error {
				d := loadDelivery{id: env.GetEventId(), at: env.GetEventTime().AsTime(), start: time.Now()}
				if retry && d.at.Equal(failOnceAt) {
					retry = false
					d.retry = true
					d.err = errors.New("synthetic transient delivery failure before rule evaluation")
				} else {
					d.err = m.Handle(handlerCtx, env, data)
				}
				d.finish = time.Now()
				select {
				case seen <- d:
				case <-subCtx.Done():
				}
				return d.err
			})
		}()
		var brokerName [2]string
		select {
		case brokerName = <-transport.created:
		case err := <-done:
			stop()
			t.Fatalf("position consumer exited before creation: %v", err)
		case <-ctx.Done():
			stop()
			t.Fatal(ctx.Err())
		}
		consumer, err := js.Consumer(ctx, brokerName[0], brokerName[1])
		if err != nil {
			stop()
			t.Fatal(err)
		}
		finish := func() {
			stop()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("position consumer did not stop")
			}
			deleteLoadConsumer(t, input, brokerName[1])
		}
		stopOnce := sync.OnceFunc(finish)
		t.Cleanup(stopOnce)
		return consumer, stopOnce
	}

	var history []loadPosition
	var topics []string
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, topic := range topics {
			if err := input.Purge(cleanup, jetstream.WithPurgeSubject(topic)); err != nil {
				t.Errorf("purge owned position %s: %v", topic, err)
			}
		}
	})
	nextTime := t0.Add(time.Hour)
	publish := func(book, instrument string, value int64, seed bool) loadPosition {
		t.Helper()
		nextTime = nextTime.Add(time.Millisecond)
		topic := subject.PositionFor(tenant, book, instrument)
		if seed {
			topics = append(topics, topic)
		}
		at := nextTime
		d := loadPosition{portfolio: book, at: at, sent: time.Now()}
		payload := &domainpb.PositionState{
			PortfolioId: book, InstrumentId: instrument, Quantity: decv(1, 0),
			MarketValue: &commonpb.Money{Amount: decv(value, 0), CurrencyCode: "USD"}, AsOf: timestamppb.New(at),
		}
		if err := in.Publish(ctx, bus.Event{Subject: topic, EventType: subject.PositionChanged,
			EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "risk", SchemaVersion: 1,
			EventTime: at, PartitionKey: topic, CorrelationID: tenant, PayloadSchemaRef: "domain.v1.PositionState:1", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		return d
	}
	for j := 1; j < 48; j++ {
		for i, book := range books {
			if j < sizes[i] {
				history = append(history, publish(book, fmt.Sprintf("BALLAST-%02d", j), 100, true))
			}
		}
	}
	for _, book := range books {
		history = append(history, publish(book, "AAPL", 10, true))
	}
	seeded := uint64(len(history))
	t.Logf("synthetic load: %s/%s CPUs=%d, %d books, %d queued holdings", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), len(books), seeded)

	// Arrival order is enforced per book. The transient failure is offered on
	// the final book of its phase and withholds the first attempt's ACK.
	drain := func(label string, want []loadPosition, retryAt time.Time) map[string]string {
		t.Helper()
		type lane struct {
			delay, service []time.Duration
			first, last    time.Time
		}
		lanes := map[string]*lane{"hot": {}, "cold": {}}
		byTime := make(map[time.Time]loadPosition, len(want))
		remaining := make(map[string][]time.Time)
		for _, p := range want {
			byTime[p.at] = p
			remaining[p.portfolio] = append(remaining[p.portfolio], p.at)
		}
		last := make(map[string]string, len(books))
		success := 0
		var retryID string
		for success < len(want) {
			select {
			case d := <-seen:
				p, ok := byTime[d.at]
				if !ok || len(remaining[p.portfolio]) == 0 || !remaining[p.portfolio][0].Equal(d.at) {
					t.Fatalf("%s: unexpected or out-of-order event at %s", label, d.at)
				}
				if d.retry && d.at.Equal(retryAt) && d.err != nil {
					if retryID != "" || d.id == "" {
						t.Fatal("expected exactly one identified failed delivery")
					}
					retryID = d.id
					continue
				}
				if d.err != nil {
					t.Fatalf("%s: delivery failed: %v", label, d.err)
				}
				if d.at.Equal(retryAt) && (retryID == "" || d.id != retryID) {
					t.Fatal("JetStream did not redeliver the same FACT after NAK")
				}
				remaining[p.portfolio] = remaining[p.portfolio][1:]
				last[p.portfolio] = d.id
				success++
				prefix := "cold"
				if strings.HasPrefix(p.portfolio, "hot-") {
					prefix = "hot"
				}
				x := lanes[prefix]
				x.delay = append(x.delay, d.start.Sub(p.sent))
				x.service = append(x.service, d.finish.Sub(d.start))
				if x.first.IsZero() || p.sent.Before(x.first) {
					x.first = p.sent
				}
				x.last = d.finish
			case <-ctx.Done():
				t.Fatalf("%s: processed %d/%d: %v", label, success, len(want), ctx.Err())
			}
		}
		if retryAt.IsZero() != (retryID == "") {
			t.Fatalf("%s: unexpected broker retry observation", label)
		}
		for _, prefix := range []string{"hot", "cold"} {
			x := lanes[prefix]
			t.Logf("%s %s: %d processed, %.1f/s, publish-to-handler p50/p95/p99=%s, handler p50/p95/p99=%s",
				label, prefix, len(x.delay), float64(len(x.delay))/x.last.Sub(x.first).Seconds(),
				loadQuantiles(x.delay), loadQuantiles(x.service))
		}
		return last
	}

	// Read both real broker outputs and correlate each new breach to its input,
	// mandate and portfolio. A repeated high exposure produces no transition.
	noPendingOutputs := func() {
		t.Helper()
		for _, reader := range outputs {
			info, err := reader.Info(ctx)
			if err != nil || info.NumPending != 0 || info.NumAckPending != 0 {
				t.Fatalf("unexpected compliance outputs: %+v %v", info, err)
			}
		}
	}
	checkBreaches := func(cause map[string]string) {
		t.Helper()
		for index, reader := range outputs {
			info, err := reader.Info(ctx)
			if err != nil || info.NumPending != uint64(len(cause)) || info.NumAckPending != 0 {
				t.Fatalf("output lane %d: expected %d new transitions: %+v %v", index, len(cause), info, err)
			}
			recorded := make(map[string]bool, len(cause))
			for range cause {
				msg, err := reader.Next(jetstream.FetchMaxWait(5 * time.Second))
				if err != nil {
					t.Fatalf("fetch output lane %d after %d results: %v", index, len(recorded), err)
				}
				env, data, err := bus.Unframe(msg.Data())
				if err != nil || bus.Validate(env) != nil || env.GetTenantId() != tenant || env.GetCorrelationId() != tenant {
					t.Fatalf("invalid broker evidence: %v", err)
				}
				var book string
				if index == 0 {
					var breach compliancepb.ComplianceBreach
					if proto.Unmarshal(data, &breach) != nil || breach.GetMandateVersion() != 7 ||
						breach.GetResult().GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
						t.Fatal("invalid concentration breach")
					}
					book = breach.GetPortfolioId()
				} else {
					var decision observationpb.DecisionLog
					if proto.Unmarshal(data, &decision) != nil || decision.Attributes["status"] != "BREACH" {
						t.Fatal("invalid compliance decision")
					}
					book = decision.Attributes["portfolio_id"]
				}
				if recorded[book] || cause[book] == "" || cause[book] != env.GetCausationId() {
					t.Fatalf("unexpected or misattributed transition for %s", book)
				}
				recorded[book] = true
				if err := msg.DoubleAck(ctx); err != nil {
					t.Fatalf("ack output lane %d for %s: %v", index, book, err)
				}
			}
		}
		noPendingOutputs()
	}
	batch := func(numerator int64) []loadPosition {
		t.Helper()
		result := make([]loadPosition, 0, len(books))
		for i, book := range books {
			ballast := int64(sizes[i]-1) * 100
			result = append(result, publish(book, "AAPL", ballast*numerator/2, false))
		}
		return result
	}
	retryAt := nextTime.Add(time.Duration(len(books)) * time.Millisecond)
	active, stop := start(makeMonitor(), retryAt)
	drain("initial backlog", history, time.Time{})
	positionTimeAwaitACK(t, ctx, active, seeded)
	noPendingOutputs()
	breach := batch(4)
	checkBreaches(drain("breach with broker retry", breach, retryAt))
	positionTimeAwaitACK(t, ctx, active, seeded+uint64(len(books))+1)
	drain("repeated breach", batch(6), time.Time{})
	positionTimeAwaitACK(t, ctx, active, seeded+uint64(2*len(books))+1)
	noPendingOutputs()
	equality := batch(3)
	drain("60 percent equality", equality, time.Time{})
	positionTimeAwaitACK(t, ctx, active, seeded+uint64(3*len(books))+1)
	noPendingOutputs()
	stop()

	// Reuse the true retained order: ballast followed by the latest six AAPL
	// readings. Replay latency begins at restart, not at the old publication.
	restartAt := time.Now()
	retained := append(history[:len(history)-len(books)], equality...)
	for i := range retained {
		retained[i].sent = restartAt
	}
	fresh, _ := start(makeMonitor(), time.Time{})
	drain("restart replay", retained, time.Time{})
	positionTimeAwaitACK(t, ctx, fresh, seeded)
	noPendingOutputs()
	checkBreaches(drain("breach after restart", batch(4), time.Time{}))
	positionTimeAwaitACK(t, ctx, fresh, seeded+uint64(len(books)))
	t.Logf("offered unique=%d, restarted replay=%d, transient retry=%d, breach FACTs=%d, decision FACTs=%d",
		seeded+uint64(4*len(books)), seeded, 1, 2*len(books), 2*len(books))
}

func deleteLoadConsumer(t *testing.T, stream jetstream.Stream, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stream.DeleteConsumer(ctx, name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
		t.Error(err)
	}
}

func loadQuantiles(samples []time.Duration) string {
	slices.Sort(samples)
	value := func(percent int) time.Duration {
		return samples[(len(samples)*percent+99)/100-1]
	}
	return fmt.Sprintf("%s/%s/%s", value(50), value(95), value(99))
}
