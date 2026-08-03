// THE DRAIN, AGAINST A REAL SPINE.
//
// redrive_test.go proves the decision logic against fakes, which is enough for
// the routing, the loop bound and the age gate and nothing at all for the
// question that matters most: whether a message parked by a real consumer, on a
// real stream, can actually be read back out and land where the service is
// listening. Every step of that is broker behaviour a fake cannot refuse —
// StreamNameBySubject on a `dlq.` subject, the durable cursor that stops a
// message being redriven twice, and the destination stream's dedup window,
// which answers a duplicate Nats-Msg-Id with a SUCCESSFUL publish and silently
// discards the message.
//
//	docker run -d --name kanz-nats -p 4222:4222 nats:2 -js
//	TEST_NATS_URL=nats://localhost:4222 go test -run TestIntegration_Redrive -p 1 ./pkg/bus/
package bus_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/pkg/bus"
)

// TestIntegration_RedriveReturnsAParkedCommandToItsOriginalSubject is the
// end-to-end proof for #220.
//
// NOTE ON THE ISSUE'S ORIGINAL "Verified when". It asked for
// TestTransientHandlerErrorIsRetriedNotDLQd — a consumer whose handler fails
// once then succeeds, asserting the handler is invoked TWICE. That test must
// not be written: it asserts in-handler retry, which is banned estate-wide by
// test/arch/bus_dlq_test.go's retryCertifiedConsumers, and it would directly
// contradict dlq_integration_test.go:216, which asserts Kanz-DLQ-Attempts == "1"
// against this same broker and says a different value "means retry was wired
// somewhere". The issue was retitled on 2026-08-02 for exactly this reason: one
// failed attempt then parking is CORRECT, and the defect is that the parked
// message was unreachable. This is the same scenario with the correct assertion
// — the handler fails once, the command parks, and the drain gets it back.
func TestIntegration_RedriveReturnsAParkedCommandToItsOriginalSubject(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the DLQ drain over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const liveSubj = "kanztest.redriveprobe"
	const dlqSubj = "dlq." + liveSubj
	const group = "redrive-probe-group"

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	t.Cleanup(setupConn.Close)
	js, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}

	// One subject per call — see dlq_integration_test.go for why: on a
	// bootstrapped spine the DLQ half binds to the real `dlq.>` stream while the
	// live half gets a scratch stream, and passing both to one call trips
	// EnsureSubjects' half-bound refusal.
	bustest.EnsureSubjects(t, ctx, js, "KANZTEST_REDRIVEPROBE", []string{liveSubj})
	bustest.EnsureSubjects(t, ctx, js, "KANZTEST_REDRIVEPROBE_DLQ", []string{dlqSubj})

	// Purge BY SUBJECT via StreamNameBySubject. The subjects are constants and
	// the streams outlive the run, so a message parked by a previous execution
	// would satisfy the drain before this run's handler had failed even once, and
	// the non-vacuity checks below would fire on a fixture problem rather than a
	// real one.
	purgeSubject(t, ctx, js, liveSubj)
	purgeSubject(t, ctx, js, dlqSubj)

	// The redrive durable persists across runs — that is its job — so its cursor
	// would still be past this run's message and the drain would correctly find
	// nothing. Delete it so each execution starts clean. The name is
	// bus.durableName("kanz-redrive", dlqSubj): the group, then the subject with
	// dots replaced by underscores.
	if name, err := js.StreamNameBySubject(ctx, dlqSubj); err == nil {
		_ = js.DeleteConsumer(ctx, name, "kanz-redrive-dlq_kanztest_redriveprobe")
	}

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "redrive-probe"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	prod, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "test/redrive-probe", ProducerVersion: "v1", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}

	// THE PRODUCTION WIRING, exactly: NewConsumer(nats, WithDLQ(nats)). No
	// WithRetry — that is the point.
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	// failing is flipped off once the command has parked, so the SECOND
	// dispatch — the redriven one — succeeds. That models the real scenario: a
	// transient dependency failure that has since recovered.
	var failing atomic.Bool
	failing.Store(true)
	var dispatches atomic.Int32

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	go func() {
		_ = consumer.Subscribe(subCtx, liveSubj, group,
			func(context.Context, *envelopepb.Envelope, []byte) error {
				dispatches.Add(1)
				if failing.Load() {
					return errors.New("store load: connection refused")
				}
				return nil
			})
	}()

	et := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	if err := prod.Publish(ctx, bus.Event{
		Subject:          liveSubj,
		EventType:        liveSubj,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:    1,
		Domain:           "kanztest",
		EventTime:        et,
		PartitionKey:     "probe",
		PayloadSchemaRef: "kanztest.redriveprobe.v1:1",
		Payload:          timestamppb.New(et),
	}); err != nil {
		t.Fatalf("publish probe: %v", err)
	}

	// Wait for the parked message to be READABLE on the DLQ subject, not merely
	// for the handler to have failed: the drain's input is the stream, and a
	// publish that has not landed yet would make the drain report an empty queue.
	waitForDLQ(t, ctx, js, dlqSubj)

	if n := dispatches.Load(); n != 1 {
		t.Fatalf("handler ran %d times before the redrive, want exactly 1 — MaxAttempts is "+
			"deliberately 1 estate-wide (test/arch/bus_dlq_test.go), so a second dispatch here "+
			"means in-handler retry was wired somewhere", n)
	}

	// The dependency has recovered.
	failing.Store(false)

	// THE DRAIN. MinAge 0 because this message is seconds old by construction;
	// the age gate is unit-tested and would otherwise make this test sleep for
	// five minutes.
	r := &bus.Redriver{
		Source:      client,
		Dest:        client,
		Options:     bus.RedriveOptions{MinAge: 0},
		IdleTimeout: 3 * time.Second,
	}
	stats, err := r.Run(ctx, dlqSubj, "kanz-redrive")
	if err != nil {
		t.Fatalf("redrive run: %v", err)
	}
	if stats.Redriven != 1 {
		t.Fatalf("redriven = %d, want 1 — the parked command was not returned to %s, which is "+
			"the whole of #220: it is still sitting on a write-only stream nothing reads",
			stats.Redriven, liveSubj)
	}

	// THE ASSERTION THAT MATTERS: the handler runs AGAIN, from the live subject,
	// having been recovered out of the DLQ. Before #220 this was unreachable —
	// the message was acked, parked, and no tool would republish it.
	redelivered := func() bool { return dispatches.Load() >= 2 }
	for deadline := time.Now().Add(30 * time.Second); !redelivered() && time.Now().Before(deadline); {
		time.Sleep(25 * time.Millisecond)
	}
	if !redelivered() {
		t.Fatalf("the redriven command never reached the handler (dispatches=%d). It was "+
			"published to %s, so either it did not land on the stream or the destination's "+
			"dedup window swallowed it — the silent no-op DefaultMinAge and redriveMsgID exist "+
			"to prevent", dispatches.Load(), liveSubj)
	}

	// AND IT IS NOT RE-PARKED. The second dispatch succeeded, so nothing new
	// should reach the DLQ.
	if n := countOnSubject(t, ctx, js, dlqSubj); n != 1 {
		t.Errorf("%d messages on %s, want the 1 original — a successfully redriven command was "+
			"parked again", n, dlqSubj)
	}

	// AND A SECOND RUN REDRIVES NOTHING. The durable cursor is what stops an
	// operator re-running the tool from re-submitting every order it already
	// recovered.
	second, err := r.Run(ctx, dlqSubj, "kanz-redrive")
	if err != nil {
		t.Fatalf("second redrive run: %v", err)
	}
	if second.Redriven != 0 {
		t.Errorf("a second run redrove %d message(s), want 0 — the durable cursor is not "+
			"holding, so every re-run of the drain re-submits commands it already recovered",
			second.Redriven)
	}
}

// TestIntegration_RedriveRefusesAPoisonMessageRatherThanLoopingForever proves
// the loop bound over the wire: the count survives the round trip through a
// REAL park, so a message that keeps failing eventually stops being sent.
func TestIntegration_RedriveRefusesAPoisonMessageRatherThanLoopingForever(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the DLQ drain over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const liveSubj = "kanztest.poisonprobe"
	const dlqSubj = "dlq." + liveSubj

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	t.Cleanup(setupConn.Close)
	js, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	bustest.EnsureSubjects(t, ctx, js, "KANZTEST_POISONPROBE", []string{liveSubj})
	bustest.EnsureSubjects(t, ctx, js, "KANZTEST_POISONPROBE_DLQ", []string{dlqSubj})

	// Same reset as the test above, and it matters more here: this test COUNTS
	// parked messages to prove the loop bound, so a message left by a previous
	// execution would shift every count and fail the run for the wrong reason.
	purgeSubject(t, ctx, js, liveSubj)
	purgeSubject(t, ctx, js, dlqSubj)
	if name, err := js.StreamNameBySubject(ctx, dlqSubj); err == nil {
		_ = js.DeleteConsumer(ctx, name, "kanz-redrive-poison-dlq_kanztest_poisonprobe")
	}

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "poison-probe"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	go func() {
		_ = consumer.Subscribe(subCtx, liveSubj, "poison-probe-group",
			func(context.Context, *envelopepb.Envelope, []byte) error {
				return errors.New("permanently broken dependency")
			})
	}()

	prod, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "test/poison-probe", ProducerVersion: "v1", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	et := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	if err := prod.Publish(ctx, bus.Event{
		Subject: liveSubj, EventType: liveSubj,
		EventClass: envelopepb.EventClass_EVENT_CLASS_COMMAND, SchemaVersion: 1,
		Domain: "kanztest", EventTime: et, PartitionKey: "probe",
		PayloadSchemaRef: "kanztest.poisonprobe.v1:1", Payload: timestamppb.New(et),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForDLQ(t, ctx, js, dlqSubj)

	r := &bus.Redriver{
		Source: client, Dest: client,
		Options:     bus.RedriveOptions{MinAge: 0, MaxRedrives: 2},
		IdleTimeout: 3 * time.Second,
	}

	// Two runs are allowed; each redrives the message, the handler fails again,
	// and it parks carrying a higher Kanz-DLQ-Redrives.
	for i := 1; i <= 2; i++ {
		stats, err := r.Run(ctx, dlqSubj, "kanz-redrive-poison")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if stats.Redriven != 1 {
			t.Fatalf("run %d redrove %d, want 1", i, stats.Redriven)
		}
		waitForDLQCount(t, ctx, js, dlqSubj, i+1)
	}

	// The third must be REFUSED. Without the bound this loops for as long as an
	// operator keeps running the tool, burning a real dispatch on a capital-path
	// subject each time.
	_, err = r.Run(ctx, dlqSubj, "kanz-redrive-poison")
	if err == nil {
		t.Fatal("the third run redrove a message that has already failed twice — the loop bound " +
			"did not survive the round trip through a real park, so DLQ→live→DLQ cycles forever")
	}
	if !errors.Is(err, bus.ErrRedriveRefused) {
		t.Errorf("error %v is not a refusal; an operator cannot tell a holding safety limit from "+
			"a broken drain", err)
	}
}

// purgeSubject clears one subject, resolving its stream rather than assuming a
// name: on a bootstrapped spine a `dlq.` subject is carried by the REAL `dlq.>`
// stream, and purging that whole stream would delete every other service's
// parked messages.
func purgeSubject(t *testing.T, ctx context.Context, js jetstream.JetStream, subj string) {
	t.Helper()
	name, err := js.StreamNameBySubject(ctx, subj)
	if err != nil {
		t.Fatalf("no stream carries %q: %v", subj, err)
	}
	stream, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatalf("open stream %s: %v", name, err)
	}
	if err := stream.Purge(ctx, jetstream.WithPurgeSubject(subj)); err != nil {
		t.Fatalf("purge %q from stream %s: %v", subj, name, err)
	}
}

func waitForDLQ(t *testing.T, ctx context.Context, js jetstream.JetStream, subj string) {
	t.Helper()
	waitForDLQCount(t, ctx, js, subj, 1)
}

func waitForDLQCount(t *testing.T, ctx context.Context, js jetstream.JetStream, subj string, want int) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if countOnSubject(t, ctx, js, subj) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("only %d message(s) on %s after 30s, want %d — nothing was parked, so there is "+
		"nothing for the drain to prove anything about",
		countOnSubject(t, ctx, js, subj), subj, want)
}

// countOnSubject reads the stream's per-subject message count from
// StreamInfo rather than consuming, so it never disturbs the durable cursor the
// test is also asserting on.
func countOnSubject(t *testing.T, ctx context.Context, js jetstream.JetStream, subj string) int {
	t.Helper()
	name, err := js.StreamNameBySubject(ctx, subj)
	if err != nil {
		t.Fatalf("no stream carries %q: %v", subj, err)
	}
	stream, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatalf("open stream %s: %v", name, err)
	}
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(subj))
	if err != nil {
		t.Fatalf("stream info %s: %v", name, err)
	}
	total := 0
	for _, n := range info.State.Subjects {
		total += int(n)
	}
	return total
}
