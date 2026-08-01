package natsrebuild_test

// #93 — A REBUILD RESTORES TENANT-PREFIXED TOPICS, AGAINST A RIG CARRYING MORE THAN ONE.
//
// #93 records the tenant dimension as ABSENT ("the scope boundary is documented;
// the capability is absent"). That is stale — NATS_REBUILD_TENANTS, ParseTenants,
// Qualify and per-tenant reporting all shipped. What had never happened is the
// thing the issue actually asks for: running it against a real Kafka holding more
// than one tenant prefix and watching the events come back onto the live spine.
//
// The property under test is the one that makes tenancy work here at all, and it
// is easy to state wrongly: KAFKA TOPICS ARE PREFIXED, NATS SUBJECTS ARE NOT.
// A tenant's log of record lives at "<tenant>.<base>" (topic.Qualify), but it
// republishes onto the SAME live subject as everyone else, carrying its identity
// in the envelope's tenant_id. So a correct rebuild is not "each tenant's events
// arrive on its own subject" — it is "every tenant's events arrive on the shared
// subject, still distinguishable". A rebuild that dropped tenant_id would look
// almost identical on the wire and would silently merge two funds' histories.
//
// Gated on a real Kafka AND a real NATS: kanz/test/backing/up.sh provides both.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/topic"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/tools/natsrebuild"
)

const (
	rebuildBaseTopic = "market.equity.trade"
	tenantA          = "tenant-acme"
	tenantB          = "tenant-beta"
)

func TestIntegration_RebuildRestoresEveryTenantsPrefixedTopic(t *testing.T) {
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	natsURL := os.Getenv("TEST_NATS_URL")
	if brokers == "" || natsURL == "" {
		t.Skip("set TEST_KAFKA_BROKERS and TEST_NATS_URL (kanz/test/backing/up.sh) to run the DR rebuild proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	broker := strings.Split(brokers, ",")[0]
	run := time.Now().UnixNano()

	// Two tenants, each with its own prefixed log-of-record topic. The system
	// tenant is deliberately NOT included: this test is about the prefixed case,
	// and including __system__ (whose Qualify is the bare name) would let an
	// implementation that ignores prefixes entirely still show traffic.
	tenants := []string{tenantA, tenantB}
	marker := map[string]string{
		tenantA: fmt.Sprintf("acme-trade-%d", run),
		tenantB: fmt.Sprintf("beta-trade-%d", run),
	}

	for _, ten := range tenants {
		qualified := topic.Qualify(ten, rebuildBaseTopic)
		if qualified == rebuildBaseTopic {
			t.Fatalf("Qualify(%q, %q) returned the bare topic — the test would not be exercising a "+
				"prefix at all", ten, rebuildBaseTopic)
		}
		createTopic(t, broker, qualified)
		writeFrame(t, ctx, broker, qualified, ten, marker[ten])
	}

	// Subscribe BEFORE the rebuild publishes. The MARKET stream retains, so a
	// later read would also work, but binding first keeps the assertion about
	// what this run republished rather than about stream history.
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: natsURL, Name: "dr-rebuild-it"})
	if err != nil {
		t.Fatalf("dial NATS: %v", err)
	}
	defer func() { _ = client.Close() }()

	seen := make(chan *envelopepb.Envelope, 64)
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.Subscribe(subCtx, rebuildBaseTopic, fmt.Sprintf("dr-rebuild-it-%d", run),
			func(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
				select {
				case seen <- env:
				default:
				}
				return nil
			})
	}()
	time.Sleep(500 * time.Millisecond)

	// The library the binary composes, driven the same way main.go drives it —
	// ParseTenants -> ResolveTargets -> Runner — so the tenancy logic under test
	// is the shipped path and not a shortcut assembled here.
	parsed, err := natsrebuild.ParseTenants(strings.Join(tenants, ","), true)
	if err != nil {
		t.Fatalf("ParseTenants: %v", err)
	}
	targets, err := natsrebuild.ResolveTargets(parsed, []string{rebuildBaseTopic}, nil)
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	if len(targets) != len(tenants) {
		t.Fatalf("resolved %d targets for %d tenants: %+v", len(targets), len(tenants), targets)
	}

	runner := &natsrebuild.Runner{
		Targets:   targets,
		Requested: parsed,
		Checker:   natsrebuild.KafkaTopicChecker{Brokers: []string{broker}},
		Open:      natsrebuild.KafkaOpen([]string{broker}),
		Publisher: client,
		Since:     time.Hour,
		Now:       time.Now(),
	}
	report, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("rebuild: %v (published %d)", err, report.Published())
	}
	if barren := report.BarrenTenants(); len(barren) > 0 {
		t.Fatalf("tenants replayed nothing at all: %v — a rebuild that restores no event for a "+
			"requested tenant is the silent success this tool exists to prevent", barren)
	}
	if report.Published() < uint64(len(tenants)) {
		t.Fatalf("published %d events for %d tenants, want at least one each", report.Published(), len(tenants))
	}

	// EVERY tenant's event must arrive, on the SHARED subject, with its tenant_id
	// intact. Collect until both markers are accounted for.
	want := map[string]string{marker[tenantA]: tenantA, marker[tenantB]: tenantB}
	got := map[string]string{} // marker -> tenant_id observed
	deadline := time.After(45 * time.Second)
	for len(got) < len(want) {
		select {
		case env := <-seen:
			id := env.GetEventId()
			if _, isOurs := want[id]; !isOurs {
				continue // other traffic on a shared subject; not this test's business
			}
			got[id] = env.GetTenantId()
		case <-deadline:
			t.Fatalf("only %d/%d tenants' events came back on %q: %+v\n\n"+
				"Kafka topics are PREFIXED per tenant but NATS subjects are not — every tenant "+
				"republishes here. A missing tenant means its prefixed topic was not resolved or "+
				"not drained, which is precisely the DR gap #93 describes.",
				len(got), len(want), rebuildBaseTopic, got)
		}
	}

	for id, wantTenant := range want {
		if got[id] != wantTenant {
			t.Errorf("event %s came back with tenant_id %q, want %q — the identity did not survive "+
				"the rebuild. Since every tenant lands on the same subject, a lost tenant_id merges "+
				"two funds' histories into one and nothing downstream can separate them again",
				id, got[id], wantTenant)
		}
	}
}

func createTopic(t *testing.T, broker, name string) {
	t.Helper()
	conn, err := kafka.Dial("tcp", broker)
	if err != nil {
		t.Fatalf("dial kafka: %v", err)
	}
	defer func() { _ = conn.Close() }()
	err = conn.CreateTopics(kafka.TopicConfig{Topic: name, NumPartitions: 1, ReplicationFactor: 1})
	if err != nil && !errors.Is(err, kafka.TopicAlreadyExists) {
		t.Fatalf("create topic %s: %v", name, err)
	}
	// CreateTopics RETURNS BEFORE THE TOPIC IS USABLE. The broker acknowledges the
	// request and propagates metadata asynchronously, so an immediate write races
	// it and fails with "Unknown Topic Or Partition" — which reads like the topic
	// was never created. Observed here on the second tenant only, which is exactly
	// how a race presents: the first create had the next create's round-trip as
	// accidental slack, the second had none. Auto-create is off on this broker
	// (KAFKA_AUTO_CREATE_TOPICS_ENABLE=false), so waiting is the only option.
	deadline := time.Now().Add(30 * time.Second)
	for {
		parts, perr := conn.ReadPartitions(name)
		if perr == nil && len(parts) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic %s did not become visible in broker metadata within 30s: %v", name, perr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// writeFrame puts one EventFrame on the tenant's prefixed topic — the shape the
// archiver writes and the shape the rebuild reads back.
func writeFrame(t *testing.T, ctx context.Context, broker, kafkaTopic, tenant, eventID string) {
	t.Helper()
	now := time.Now().UTC()
	frame := &envelopepb.EventFrame{
		Envelope: &envelopepb.Envelope{
			EventId: eventID,
			// A COMPLETE envelope, because the consumer validates and a partial one
			// never reaches a handler. The first version of this test omitted
			// envelope_version, publish_time, idempotency_key and
			// payload_schema_ref; Kafka accepted it, the rebuild republished it,
			// the stream stored it, and the subscription silently saw nothing —
			// the exact "fakeBus accepts what a real broker rejects" gap CLAUDE.md
			// warns about, reproduced by hand. FACT requires
			// idempotency_key == event_id (event-class-rules §1).
			EnvelopeVersion:  1,
			IdempotencyKey:   eventID,
			PayloadSchemaRef: "market.v1.Trade",
			PublishTime:      timestamppb.New(now),
			// The destination NATS subject IS the event_type (subject-taxonomy
			// §6) — it is not derived from the Kafka topic, which is why the
			// prefix does not leak onto the spine.
			EventType:       rebuildBaseTopic,
			EventClass:      envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion:   1,
			Domain:          "market",
			Source:          "dr-rebuild-it",
			ProducerVersion: "it",
			TenantId:        tenant,
			PartitionKey:    eventID,
			CorrelationId:   eventID,
			EventTime:       timestamppb.New(now),
			IngestionTime:   timestamppb.New(now),
		},
		Payload: []byte("dr-rebuild-payload"),
	}
	body, err := proto.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	w := &kafka.Writer{Addr: kafka.TCP(broker), Topic: kafkaTopic, BatchTimeout: 100 * time.Millisecond}
	defer func() { _ = w.Close() }()
	if err := w.WriteMessages(ctx, kafka.Message{Key: []byte(eventID), Value: body, Time: now}); err != nil {
		t.Fatalf("write to %s: %v", kafkaTopic, err)
	}
}
