package bus_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// THE PROOF FOR #283 IS A SCRAPE, NOT A UNIT TEST.
//
// The defect was never that a setter computed the wrong number. It was that the
// wiring between a real broker and a real registry did not exist: the gauges were
// registered, no code path reached them, and /metrics carried the metric families
// with zero series. A fake subscriber cannot show that gap closed, because the gap
// was in the part a fake stands in for. So these tests run a real bus.Consumer
// against a real broker, build a real backlog, and then GO AND SCRAPE an HTTP
// endpoint over a real prometheus.Registry — the same three moving parts the
// KEDA trigger reads through.

func TestBacklogPollerReportsNATSPending(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	streamName := "TEST_BACKLOG_" + suffix
	subject := "test.backlog." + suffix
	group := "backlog-" + suffix

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name: streamName, Subjects: []string{subject},
		Storage: jetstream.MemoryStorage, Retention: jetstream.LimitsPolicy,
	}); err != nil {
		setupConn.Close()
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() {
		_ = setupJS.DeleteStream(ctx, streamName)
		setupConn.Close()
	})

	reg := prometheus.NewRegistry()
	metrics := bus.NewBusMetrics(reg)
	scrape := startScrapeEndpoint(t, reg)

	// A SEPARATE publisher client. The broker-outage phase below kills the
	// CONSUMER's connection, and the publisher must not go with it.
	pub, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "backlog-pub"})
	if err != nil {
		t.Fatalf("dial publisher: %v", err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	publishProbes(t, ctx, pub, subject, 5)

	// MaxAckPending 1, because NumPending counts messages NOT YET DELIVERED. On
	// the default work tuning (32) all five probes would be handed to the blocked
	// handler at once and NumPending would legitimately read 0 — a backlog that
	// exists only as un-acked in-flight work, which is not what this gauge
	// measures and not what KEDA scales on.
	cons, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: url, Name: "backlog-cons",
		ConsumerTuning: &bus.ConsumerTuning{AckWait: 30 * time.Second, MaxDeliver: 5, MaxAckPending: 1},
	})
	if err != nil {
		t.Fatalf("dial consumer: %v", err)
	}
	consumer, err := bus.NewConsumer(cons, bus.WithBusMetrics(metrics), bus.WithDLQ(pub))
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	block := make(chan struct{})
	defer close(block)
	go func() {
		_ = consumer.Subscribe(subCtx, subject, group, func(hctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
			select {
			case <-block:
			case <-hctx.Done():
			}
			return nil
		})
	}()

	want := map[string]string{"subject": subject, "group": group}
	pending := waitForSeries(t, scrape, "kanz_bus_pending_messages", want, 30*time.Second,
		func(v float64) bool { return v > 0 })
	t.Logf("scraped kanz_bus_pending_messages{subject=%q,group=%q} = %v", subject, group, pending)
	if pending < 3 {
		t.Errorf("pending = %v, want >= 3 (5 published, MaxAckPending 1) — the poller is reporting a "+
			"backlog it is not measuring", pending)
	}
	// The poller's own health must be reporting success while it is working, or
	// the staleness signal cannot be trusted during the outage below.
	if _, ok := findSeries(scrape(t), "kanz_bus_backlog_poll_last_success_timestamp_seconds",
		map[string]string{"subject": subject, "group": group, "transport": "nats"}); !ok {
		t.Fatal("no kanz_bus_backlog_poll_last_success_timestamp_seconds while the poller is succeeding")
	}

	// ---- THE BROKER GOES AWAY ----
	//
	// The whole point of #283's second question. A poller that publishes 0 here
	// tells the KEDA trigger the backlog cleared, and the trigger scales the
	// service IN during a broker incident. Assert the OPPOSITE of a zero: the
	// series must be GONE, and the poller's health series must still be there
	// saying why.
	_ = cons.Close()

	deadline := time.Now().Add(40 * time.Second)
	var lastSeen bool
	for time.Now().Before(deadline) {
		body := scrape(t)
		if v, ok := findSeries(body, "kanz_bus_pending_messages", want); ok {
			lastSeen = true
			if v == 0 {
				t.Fatalf("kanz_bus_pending_messages is 0 with the broker unreachable — 'no answer' and "+
					"'no backlog' have become the same reading again, which is #283 on a %s timescale",
					15*time.Second)
			}
		} else {
			lastSeen = false
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastSeen {
		t.Fatal("kanz_bus_pending_messages is still exported 40s after the broker became unreachable — " +
			"the failure path did not delete it, so KEDA is scaling on a number nothing is updating")
	}

	body := scrape(t)
	health := map[string]string{"subject": subject, "group": group, "transport": "nats"}
	failures, ok := findSeries(body, "kanz_bus_backlog_poll_failures_total", health)
	if !ok || failures < 1 {
		t.Errorf("kanz_bus_backlog_poll_failures_total = %v (present=%v), want >= 1 — with the backlog "+
			"gauge deleted this is the only series saying the poller is trying and failing", failures, ok)
	}
	lastOK, ok := findSeries(body, "kanz_bus_backlog_poll_last_success_timestamp_seconds", health)
	if !ok || lastOK <= 0 {
		t.Errorf("kanz_bus_backlog_poll_last_success_timestamp_seconds = %v (present=%v), want the "+
			"timestamp of the last real answer — without it an absent backlog gauge is indistinguishable "+
			"from a subject nobody consumes", lastOK, ok)
	}
	t.Logf("broker unreachable: pending series ABSENT, failures=%v, last_success=%v (age %.1fs)",
		failures, lastOK, float64(time.Now().Unix())-lastOK)
}

func TestBacklogPollerReportsKafkaLag(t *testing.T) {
	raw := os.Getenv("TEST_KAFKA_BROKERS")
	if raw == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	brokers := strings.Split(raw, ",")
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	topic := "test.backlog." + suffix
	group := "backlog-" + suffix

	setupConn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("setup dial: %v", err)
	}
	if err := setupConn.CreateTopics(kafka.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}); err != nil {
		setupConn.Close()
		t.Fatalf("create topic: %v", err)
	}
	setupConn.Close()
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.DeleteTopics(topic)
	})

	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := bus.NewBusMetrics(reg)
	scrape := startScrapeEndpoint(t, reg)

	client, err := bus.DialKafka(bus.KafkaConfig{Brokers: brokers, ClientID: "backlog-test", Metrics: metrics})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	publishProbes(t, ctx, client, topic, 5)

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(metrics), bus.WithDLQ(client))
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	block := make(chan struct{})
	defer close(block)
	go func() {
		_ = consumer.Subscribe(subCtx, topic, group, func(hctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
			select {
			case <-block:
			case <-hctx.Done():
			}
			return nil
		})
	}()

	// partition="0": a single-partition topic, and the label matters — the Kafka
	// gauge is PER PARTITION, which is the shape kafka-go's own Reader.Stats().Lag
	// cannot produce at all (it is one last-writer-wins value across every
	// partition a group reader owns). See BusMetrics.SetConsumerLag.
	want := map[string]string{"subject": topic, "group": group, "partition": "0"}
	lag := waitForSeries(t, scrape, "kanz_bus_consumer_lag", want, 30*time.Second,
		func(v float64) bool { return v > 0 })
	t.Logf("scraped kanz_bus_consumer_lag{subject=%q,group=%q,partition=\"0\"} = %v", topic, group, lag)
	if lag != 5 {
		t.Errorf("lag = %v, want 5 — five probes published, the group has committed nothing, and the "+
			"handler is blocked, so the whole retained partition is still owed to it", lag)
	}
}

// TestKafkaBacklogRefusesToReportZeroForAnUnreachableBroker is the Kafka half of
// the failure policy.
//
// The NATS test above kills a live consumer's connection and watches the series
// disappear. The equivalent for Kafka cannot be done in-process: closing the
// KafkaClient also ends Subscribe, so the series would vanish for the ordinary
// end-of-subscription reason and prove nothing about the outage path. What is
// transport-specific is only the question BacklogSource is asked, so that is what
// is tested here — the poller loop it feeds is shared and is covered above.
//
// The contract being pinned: an unreachable broker produces an ERROR, never
// `[]PartitionBacklog{{Messages: 0}}`. A zero here would travel all the way to
// the KEDA trigger as "backlog cleared".
func TestKafkaBacklogRefusesToReportZeroForAnUnreachableBroker(t *testing.T) {
	// Port 1 is reserved and nothing listens on it; no environment gate, because
	// there is no broker to gate on.
	client, err := bus.DialKafka(bus.KafkaConfig{Brokers: []string{"127.0.0.1:1"}, ClientID: "backlog-down"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	readings, err := client.Backlog(ctx, "test.backlog.unreachable", "nobody")
	if err == nil {
		t.Fatalf("Backlog returned %v and no error against an unreachable broker — a caller cannot tell "+
			"'no backlog' from 'no answer', which is the whole of #283", readings)
	}
	if readings != nil {
		t.Errorf("Backlog returned readings %v alongside an error; the poller writes whatever it gets back, "+
			"so a non-nil slice here is a number that would reach the gauge", readings)
	}
}

// publishProbes puts n valid, distinct envelope-framed COMMANDs on subject.
// COMMAND rather than FACT because it carries an idempotency key, and distinct
// keys are what stop the consumer's dedup window from collapsing the backlog to
// one message.
func publishProbes(t *testing.T, ctx context.Context, client bus.Client, subject string, n int) {
	t.Helper()
	prod, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "backlog-test", ProducerVersion: "test", Tenant: "kanztest",
	})
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	et := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		if err := prod.Publish(ctx, bus.Event{
			Subject: subject, EventType: subject,
			EventClass: envelopepb.EventClass_EVENT_CLASS_COMMAND, SchemaVersion: 1,
			Domain: "kanztest", EventTime: et, PartitionKey: "probe",
			IdempotencyKey:   fmt.Sprintf("backlog-probe-%s-%d", subject, i),
			PayloadSchemaRef: "kanztest.backlogprobe.v1:1", Payload: timestamppb.New(et),
		}); err != nil {
			t.Fatalf("publish probe %d: %v", i, err)
		}
	}
}

// startScrapeEndpoint serves reg over HTTP and returns a function that fetches
// /metrics. A real HTTP scrape of a real registry, because "the family is
// exported with no series" is a property of the exposition output and of nothing
// else — testutil.ToFloat64 on a collector would have been just as green while
// #283 was live.
func startScrapeEndpoint(t *testing.T, reg *prometheus.Registry) func(*testing.T) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	t.Cleanup(srv.Close)
	return func(t *testing.T) string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatalf("scrape: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read scrape: %v", err)
		}
		return string(body)
	}
}

// findSeries looks up one sample in a Prometheus exposition body by metric name
// and a subset of its labels.
func findSeries(body, name string, want map[string]string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		labels := map[string]string{}
		if strings.HasPrefix(rest, "{") {
			end := strings.Index(rest, "}")
			if end < 0 {
				continue
			}
			for _, pair := range splitLabels(rest[1:end]) {
				k, v, found := strings.Cut(pair, "=")
				if !found {
					continue
				}
				labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
			}
			rest = rest[end+1:]
		} else if !strings.HasPrefix(rest, " ") {
			continue // a longer metric name that merely shares this prefix
		}
		match := true
		for k, v := range want {
			if labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			continue
		}
		return f, true
	}
	return 0, false
}

// splitLabels splits a label set on commas that are not inside a quoted value.
func splitLabels(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"' && (i == 0 || s[i-1] != '\\'):
			inQuote = !inQuote
			cur.WriteByte(s[i])
		case s[i] == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func waitForSeries(t *testing.T, scrape func(*testing.T) string, name string, want map[string]string,
	timeout time.Duration, ok func(float64) bool) float64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = scrape(t)
		if v, found := findSeries(last, name, want); found && ok(v) {
			return v
		}
		time.Sleep(250 * time.Millisecond)
	}
	var families []string
	for _, line := range strings.Split(last, "\n") {
		if strings.HasPrefix(line, name) {
			families = append(families, line)
		}
	}
	t.Fatalf("%s%v never appeared with an acceptable value within %s.\nlines carrying that name: %v\n\n"+
		"If the metric family is present with no sample line, that is exactly #283: registered, exported, "+
		"and never written.", name, want, timeout, families)
	return 0
}
