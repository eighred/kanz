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
	"sync"
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

	// THE PRODUCTION WIRING, exactly: NewConsumer(nats, WithDLQ(nats)), default
	// dedup window, no WithRetry — that last one is the point.
	//
	// This first instance is the one that FAILS the command and parks it.
	parking := startProbeConsumer(t, ctx, client, liveSubj, group, func() error {
		return errors.New("store load: connection refused")
	})

	et := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	if err := prod.Publish(ctx, bus.Event{
		Subject:       liveSubj,
		EventType:     liveSubj,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion: 1,
		Domain:        "kanztest",
		EventTime:     et,
		PartitionKey:  "probe",
		// Required for COMMAND, and load-bearing here rather than boilerplate:
		// Publish maps it onto Nats-Msg-Id, which is the header the redrive
		// deliberately re-stamps per attempt so the destination stream's dupe
		// window cannot swallow a replay with a successful PubAck. A probe with
		// no key would not exercise that path at all.
		IdempotencyKey:   probeKey("redrive-probe"),
		PayloadSchemaRef: "kanztest.redriveprobe.v1:1",
		Payload:          timestamppb.New(et),
	}); err != nil {
		t.Fatalf("publish probe: %v", err)
	}

	// Wait for the parked message to be READABLE on the DLQ subject, not merely
	// for the handler to have failed: the drain's input is the stream, and a
	// publish that has not landed yet would make the drain report an empty queue.
	waitForDLQ(t, ctx, js, dlqSubj)

	if n := parking.dispatches.Load(); n != 1 {
		t.Fatalf("handler ran %d times before the redrive, want exactly 1 — MaxAttempts is "+
			"deliberately 1 estate-wide (test/arch/bus_dlq_test.go), so a second dispatch here "+
			"means in-handler retry was wired somewhere", n)
	}

	// THE DEPENDENCY HAS RECOVERED, AND THE PARKING INSTANCE IS GONE.
	//
	// Stopping it before starting the recovered one is what models the dedup TTL
	// having elapsed — see startProbeConsumer for why that is faithful rather
	// than a weakened assertion. It is also what makes the test deterministic:
	// two instances on one durable would race for the redriven message, and the
	// old one still holds the suppressing entry.
	parking.stop()
	recovered := startProbeConsumer(t, ctx, client, liveSubj, group, func() error { return nil })

	// THE DRAIN. MinAge 0 because the message is seconds old by construction and
	// the alternative is a five-minute sleep; the age gate itself is proven by
	// TestPlanRedriveRefusesAMessageTooYoungToSurviveTheDedupWindows and its
	// ordering against the dedup TTL by
	// TestDefaultMinAgeOutlastsTheConsumerDedupWindow plus a compile-time assertion.
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

	// MinAge 0 on a seconds-old message is exactly the case the drain must not
	// report as a clean success, because it cannot see the consumer's dedup
	// window from here.
	if stats.SuppressionRisk != 1 {
		t.Errorf("SuppressionRisk = %d, want 1 — redriving inside the dedup window has to be "+
			"reported, or 'redriven 1' means 'and possibly nothing happened'", stats.SuppressionRisk)
	}

	// THE ASSERTION THAT MATTERS: the handler runs AGAIN, on a live consumer,
	// having been recovered out of the DLQ. Before #220 this was unreachable —
	// the message was acked, parked, and no supplied tool would republish it.
	redelivered := func() bool { return recovered.dispatches.Load() >= 1 }
	for deadline := time.Now().Add(30 * time.Second); !redelivered() && time.Now().Before(deadline); {
		time.Sleep(25 * time.Millisecond)
	}
	if !redelivered() {
		t.Fatalf("the redriven command never reached a handler. It was published to %s, so "+
			"either it did not land on the stream or a dedup window swallowed it — the silent "+
			"no-op that DefaultMinAge, redriveMsgID and SuppressionRisk exist to prevent",
			liveSubj)
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
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
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

	const group = "poison-probe-group"
	alwaysFails := func() error { return errors.New("permanently broken dependency") }

	// Cycle 0: the instance that parks the message the first time.
	cur := startProbeConsumer(t, ctx, client, liveSubj, group, alwaysFails)

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
		// Required for COMMAND; see the note in the sibling test. Distinct from
		// it so the two probes cannot collide on Nats-Msg-Id if a stream outlives
		// one of them.
		IdempotencyKey:   probeKey("poison-probe"),
		PayloadSchemaRef: "kanztest.poisonprobe.v1:1", Payload: timestamppb.New(et),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForDLQ(t, ctx, js, dlqSubj)

	// A FRESH CONSUMER PER CYCLE, for the reason startProbeConsumer documents:
	// the instance that just parked this message is suppressing its
	// idempotency_key for the dedup TTL, so the redriven copy would be skipped
	// and acked and would never park again. Cycling the instance reproduces the
	// state DefaultMinAge guarantees in production without waiting 2m per cycle.
	cur.stop()
	cur = startProbeConsumer(t, ctx, client, liveSubj, group, alwaysFails)

	// CYCLE 1 — and this run also proves ONE ATTEMPT PER MESSAGE PER RUN, the
	// rule that only a real broker could have shown was missing.
	//
	// The drain stays subscribed while it works, so the message it just redrove
	// fails, re-parks and is re-offered to this same run within milliseconds.
	// Before errRedrivenThisRun existed this run walked the message from 0 to 2
	// redrives in under 200ms — the entire loop budget spent automatically, in
	// one operator action, against a dependency still down. No Limit here, so a
	// regression would show up as Redriven > 1.
	cycle1 := &bus.Redriver{
		Source: client, Dest: client,
		Options:     bus.RedriveOptions{MinAge: 0, MaxRedrives: 2},
		IdleTimeout: 5 * time.Second,
	}
	stats, err := cycle1.Run(ctx, dlqSubj, "kanz-redrive-poison")
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if stats.Redriven != 1 {
		t.Fatalf("cycle 1 redrove %d, want exactly 1. A single run must not spend more than one "+
			"of a message's redrive budget: the bound is a sequence of operator decisions, and a "+
			"run that consumes it by itself makes MaxRedrives meaningless as a checkpoint",
			stats.Redriven)
	}
	if stats.ReturnedThisRun != 1 {
		t.Errorf("ReturnedThisRun = %d, want 1 — the redriven copy failed and came back inside "+
			"this run, and the drain has to report that rather than silently re-sending it",
			stats.ReturnedThisRun)
	}
	waitForDLQCount(t, ctx, js, dlqSubj, 2)

	// CYCLE 2 takes the count to 2, which is the limit.
	//
	// Limit 1 so the run stops as soon as it has done its one redrive, and a
	// LONG idle window because the message it needs was NAK'd by cycle 1 (that is
	// what deferring it means) and the broker is holding it on nakDelay's
	// exponential backoff — 2s, 4s, 8s… by delivery count. A short window expires
	// before the redelivery and the run ends cleanly having seen nothing, which
	// is the flake this replaced.
	cycle2 := &bus.Redriver{
		Source: client, Dest: client,
		Options:     bus.RedriveOptions{MinAge: 0, MaxRedrives: 2},
		IdleTimeout: 45 * time.Second,
		Limit:       1,
	}
	cur.stop()
	cur = startProbeConsumer(t, ctx, client, liveSubj, group, alwaysFails)
	stats, err = cycle2.Run(ctx, dlqSubj, "kanz-redrive-poison")
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if stats.Redriven != 1 {
		t.Fatalf("cycle 2 redrove %d, want 1", stats.Redriven)
	}
	// It must come BACK — dispatched, failed, re-parked — carrying its
	// incremented count. That round trip is what the loop bound rides on.
	waitForDLQCount(t, ctx, js, dlqSubj, 3)

	// THE THIRD MUST BE REFUSED. Without the bound this loops for as long as an
	// operator keeps running the tool, burning a real dispatch on a capital-path
	// subject each time. Same long idle window, same reason as cycle 2.
	final := &bus.Redriver{
		Source: client, Dest: client,
		Options:     bus.RedriveOptions{MinAge: 0, MaxRedrives: 2},
		IdleTimeout: 45 * time.Second,
	}
	// A live consumer is still running for this last run, and not assigned to cur
	// because nothing reads it again — t.Cleanup stops it. It is here so that a
	// REGRESSION is loud rather than quiet: if the loop bound ever stopped
	// holding, the message would be redriven, dispatched, and re-parked, and the
	// DLQ count check below would catch it on top of the refusal assertion.
	cur.stop()
	startProbeConsumer(t, ctx, client, liveSubj, group, alwaysFails)
	stats, err = final.Run(ctx, dlqSubj, "kanz-redrive-poison")
	if err == nil {
		t.Fatalf("the third run did not refuse a message that has already failed twice "+
			"(inspected=%d, redriven=%d). The loop bound did not survive the round trip through "+
			"a real park, so DLQ→live→DLQ cycles for as long as an operator keeps running the tool",
			stats.Inspected, stats.Redriven)
	}
	if stats.Redriven != 0 {
		t.Errorf("the refusing run still redrove %d message(s), want 0", stats.Redriven)
	}
	if !errors.Is(err, bus.ErrRedriveRefused) {
		t.Errorf("error %v is not a refusal; an operator cannot tell a holding safety limit from "+
			"a broken drain", err)
	}
	// AND NOTHING NEW WAS PARKED. The refusal has to mean the message stayed put,
	// not that it went round again and the run happened to report an error. Three
	// is the original plus the two allowed cycles.
	if n := countOnSubject(t, ctx, js, dlqSubj); n != 3 {
		t.Errorf("%d messages on %s, want 3 — the refused message made another DLQ→live→DLQ "+
			"trip despite the bound", n, dlqSubj)
	}
}

// purgeSubject clears one subject, resolving its stream rather than assuming a
// name: on a bootstrapped spine a `dlq.` subject is carried by the REAL `dlq.>`
// stream, and purging that whole stream would delete every other service's
// parked messages.
// probeKey returns an idempotency key unique to this run.
//
// IT MUST BE UNIQUE, and a fixed literal is a trap that costs a confusing
// half-hour. Publish maps the key onto Nats-Msg-Id, and every stream in
// infra/nats/bootstrap-job.yaml carries `--dupe-window=2m`, so re-running one of
// these tests inside two minutes with a constant key makes the broker answer the
// probe publish with a successful PubAck and DISCARD it. Nothing is ever
// delivered, nothing parks, and the test fails at "nothing was parked" pointing
// at the drain — which is innocent. Purging the subject does not help: the dupe
// window is tracked separately from the messages.
func probeKey(prefix string) string {
	return prefix + "-" + time.Now().UTC().Format("20060102T150405.000000000")
}

// runningConsumer is one subscribed consumer instance plus the means to stop it
// and wait for it to be gone.
type runningConsumer struct {
	dispatches *atomic.Int32
	stop       func()
}

// startProbeConsumer subscribes a consumer with THE PRODUCTION WIRING —
// NewConsumer(nats, WithDLQ(nats)), default dedup window, no WithRetry — and
// returns a handle that stops it and blocks until Subscribe has returned.
//
// WHY THE TESTS CYCLE THE CONSUMER INSTEAD OF SLEEPING. Parking calls
// dedup.Commit(idempotency_key), which suppresses that key on THAT INSTANCE for
// defaultDedupTTL (2m). A redrive arriving inside the window is skipped and
// acked, so the handler never runs. In production DefaultMinAge (5m) puts the
// redrive well outside it — that ordering is now derived and compiler-asserted
// (redrive.go), and unit-tested by TestDefaultMinAgeOutlastsTheConsumerDedupWindow.
//
// Reproducing it here by waiting is not an option: two minutes per park, three
// parks in the poison test. So these tests reproduce the STATE instead of the
// duration. The window is per-instance and in-memory, so a fresh instance has no
// entry for the key — exactly the state the TTL expiring produces. Stopping the
// parking consumer and starting a new one is the same technique as the injected
// clock DedupWindow already exposes for unit tests, applied at instance
// granularity because the Consumer does not take a clock.
//
// This is a faithful model, not a weakened assertion: everything else stays the
// production wiring, and what is being proven — that a parked message can be
// read off the DLQ stream and land back on its live subject where a handler
// receives it — is unaffected by which instance holds the dedup map.
func startProbeConsumer(t *testing.T, ctx context.Context, client *bus.NATSClient,
	subject, group string, handler func() error,
) *runningConsumer {
	t.Helper()
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	var dispatches atomic.Int32
	subCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = consumer.Subscribe(subCtx, subject, group,
			func(context.Context, *envelopepb.Envelope, []byte) error {
				dispatches.Add(1)
				return handler()
			})
	}()
	rc := &runningConsumer{dispatches: &dispatches}
	var once sync.Once
	rc.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("Subscribe did not return within 15s of cancellation; the next " +
					"consumer would race this one for the durable")
			}
		})
	}
	t.Cleanup(rc.stop)
	return rc
}

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
