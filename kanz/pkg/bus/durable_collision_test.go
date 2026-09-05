package bus

// THE DURABLE-NAME COLLISION IS REFUSED, AT BOTH DISTANCES (#1011).
//
// durableName maps `.`, `*`, `>` and ` ` all onto `_` because a JetStream
// consumer name may carry none of them. That map is MANY-TO-ONE, and
// CreateOrUpdateConsumer does not fail on a second claimant — it rewrites the
// first consumer's FilterSubject, after which one subscription receives nothing
// while its consumer still exists and reports Ready.
//
// These two tests are the compensating control the arch-guard exemption in
// test/arch/one_subject_sanitizer_test.go names, and they are deliberately
// split by DISTANCE rather than by layer:
//
//   - TestDurableNameCollisionIsRefused covers ONE PROCESS, needs no broker, and
//     is the arm that stays live on a laptop with no NATS running.
//   - TestDurableCollisionIsRefusedAcrossProcesses drives the REAL Subscribe path
//     against a REAL broker with TWO clients, because a unit test on claimDurable
//     proves nothing about whether Subscribe calls it — and because the reachable
//     case is cmd/kanz-redrive, where the two colliding subscribes are two
//     separate invocations and no in-process map can see both.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// collidingPairs are subject pairs that differ ONLY where durableName is lossy.
//
// The second pair is not hypothetical: cmd/kanz-redrive takes its subject from an
// operator's `--subject` flag whose own help text offers `dlq.order.>` as an
// example, and `dlq.order.*` is the same flag one keystroke away.
var collidingPairs = [][2]string{
	{"order.order.submit", "order.order_submit"},
	{"dlq.order.>", "dlq.order.*"},
}

func TestDurableNameCollisionIsRefused(t *testing.T) {
	const group = "oms"

	for _, pair := range collidingPairs {
		a, b := pair[0], pair[1]
		// The premise. If durableName ever stops being lossy for this pair the
		// refusal below is testing nothing, and this says so rather than passing.
		if durableName(group, a) != durableName(group, b) {
			t.Fatalf("durableName(%q,%q) = %q and durableName(%q,%q) = %q no longer collide — "+
				"if durableName became injective, delete this refusal and its arch-guard exemption "+
				"together (see durableName's doc comment for what that migration costs)",
				group, a, durableName(group, a), group, b, durableName(group, b))
		}
	}

	for _, pair := range collidingPairs {
		a, b := pair[0], pair[1]
		t.Run(a+" vs "+b, func(t *testing.T) {
			c := &NATSClient{}
			dur := durableName(group, a)

			if err := c.claimDurable("EXECUTION", dur, a); err != nil {
				t.Fatalf("first claim of %q for %q: %v", dur, a, err)
			}
			// Re-claiming for the SAME subject must stay silent: a service that
			// resubscribes after a context cancel is not a collision, and refusing
			// it would turn a restart into an outage.
			if err := c.claimDurable("EXECUTION", dur, a); err != nil {
				t.Fatalf("re-claim of %q for the same subject %q was refused: %v", dur, a, err)
			}

			err := c.claimDurable("EXECUTION", durableName(group, b), b)
			if err == nil {
				t.Fatalf("claimDurable accepted %q for durable %q, which is already bound to %q — "+
					"CreateOrUpdateConsumer would now rewrite the first subscription's filter and that "+
					"feed would go dark while its consumer stayed Ready", b, dur, a)
			}
			// The message has to NAME both subjects, or an operator reading it at
			// 3am cannot tell which two feeds are in conflict.
			for _, want := range []string{dur, a, b, "#1011"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// TestDurableClaimIsNotWholesaleRefusal is the non-vacuity arm. Without it the
// test above passes on a claimDurable that refuses EVERYTHING, which would be a
// bus that cannot subscribe to two subjects at once — the very thing #237 fixed.
func TestDurableClaimIsNotWholesaleRefusal(t *testing.T) {
	c := &NATSClient{}
	for _, subj := range []string{"order.order.submit", "order.order.amend", "execution.fill.recorded"} {
		if err := c.claimDurable("EXECUTION", durableName("oms", subj), subj); err != nil {
			t.Fatalf("claimDurable refused %q, which collides with nothing: %v", subj, err)
		}
	}
}

// TestDurableCollisionIsRefusedAcrossProcesses drives Subscribe itself, with two
// clients standing in for two processes, against a real broker.
//
// TWO CLIENTS IS THE POINT. One client would be stopped by the in-process claim
// map and the broker read would never run — so a single-client test would pass
// with inspectExistingDurable deleted. Each NATSClient carries its own claims
// map, which is exactly what two `kanz-redrive` invocations look like.
func TestDurableCollisionIsRefusedAcrossProcesses(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set — this is the half of #1011 that needs a real CreateOrUpdateConsumer")
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	streamName := "TEST_DUR1011_" + suffix
	// a.b and a_b differ only where durableName is lossy; a.c does not collide
	// with either and is the control.
	base := "dur1011." + suffix
	subjA := base + ".a.b"
	subjB := base + ".a_b"
	subjC := base + ".a.c"
	group := "g" + suffix

	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	t.Cleanup(setupConn.Close)
	setupJS, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	if _, err := setupJS.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{base + ".>"},
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	// Deleting the stream takes its consumers with it, so this run leaves no
	// durable behind on a shared broker for a later test to bind by accident.
	t.Cleanup(func() { _ = setupJS.DeleteStream(context.Background(), streamName) })

	if durableName(group, subjA) != durableName(group, subjB) {
		t.Fatalf("premise gone: %q and %q no longer share a durable name", subjA, subjB)
	}

	dial := func(name string) *NATSClient {
		c, err := DialNATS(ctx, NATSConfig{URL: url, Name: name})
		if err != nil {
			t.Fatalf("dial %s: %v", name, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	// FOUR clients, because two of the arms below must not be able to answer each
	// other. `second` joins the group on subjA, which makes it hold an in-process
	// claim on that durable — so the colliding subscribe has to come from a client
	// that holds none (`fourth`), or claimDurable answers it and the broker read is
	// never exercised. Measured: with them shared, disabling the broker comparison
	// left this test green.
	first, second := dial("dur1011-a"), dial("dur1011-b")
	third, fourth := dial("dur1011-c"), dial("dur1011-d")

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan Message, 4)
	subErr := make(chan error, 1)
	go func() {
		subErr <- first.Subscribe(subCtx, subjA, group, func(_ context.Context, m Message) error {
			got <- m
			return nil
		})
	}()

	// Wait for the durable to exist. Pending looks up the EXISTING consumer and
	// errors until Subscribe has created it, so it is the readiness signal
	// without a sleep.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := first.Pending(ctx, subjA, group); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first subscription never created its durable")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// THE CONTROL, FIRST. A second process subscribing to a NON-colliding subject
	// in the same group must still succeed — otherwise the refusal below could be
	// "Subscribe fails for a second client" and nothing about collisions.
	ctlCtx, ctlCancel := context.WithCancel(ctx)
	ctlErr := make(chan error, 1)
	go func() {
		ctlErr <- third.Subscribe(ctlCtx, subjC, group, func(context.Context, Message) error { return nil })
	}()
	ctlDeadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := third.Pending(ctx, subjC, group); err == nil {
			break
		}
		if time.Now().After(ctlDeadline) {
			t.Fatal("a non-colliding subject in the same group could not subscribe — the refusal is too wide")
		}
		select {
		case err := <-ctlErr:
			t.Fatalf("Subscribe(%q) was refused, and it collides with nothing: %v", subjC, err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	ctlCancel()
	<-ctlErr

	// THE SECOND CONTROL, AND IT IS THE ONE THAT MATTERS MOST. A different process
	// binding the SAME subject in the same group is not a collision — it is what a
	// consumer group IS, and it is what every replica after the first does on every
	// deploy. The broker read sees an existing durable here too, so a refusal keyed
	// on "a consumer already exists" rather than on "it is bound to another subject"
	// would pass every other assertion in this file and stop the estate from
	// scaling past one replica.
	if err := subscribeOutcome(t, second, subjA, group, 3*time.Second); err != nil {
		t.Fatalf("a second replica could not join the group on %q: %v", subjA, err)
	}

	// THE REFUSAL. A different process, a colliding subject, the same group.
	err = subscribeOutcome(t, fourth, subjB, group, 10*time.Second)
	if err == nil {
		t.Fatalf("Subscribe(%q) succeeded while durable %q is bound to %q — it has just rewritten the "+
			"first subscription's FilterSubject", subjB, durableName(group, subjA), subjA)
	}
	for _, want := range []string{durableName(group, subjA), subjA, subjB, "#1011"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}

	// AND THE FIRST FEED IS STILL ALIVE. This is the assertion that distinguishes
	// "refused" from "refused, but the damage was already done": the whole defect
	// is a live subscription going dark, so the test has to watch it not go dark.
	if _, err := setupJS.Publish(ctx, subjA, []byte("still-mine")); err != nil {
		t.Fatalf("publish to %q: %v", subjA, err)
	}
	select {
	case m := <-got:
		if string(m.Body) != "still-mine" {
			t.Fatalf("body = %q", m.Body)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the first subscription received nothing on its own subject after the colliding Subscribe")
	}

	cancel()
	if err := <-subErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("first subscription: %v", err)
	}
}

// TestDurableClaimHoldsAfterTheConsumerIsGone isolates the IN-PROCESS half.
//
// It is the one arrangement where the broker read cannot answer: this process has
// subscribed to A, the consumer has since been deleted from the broker, and the
// process now subscribes to a colliding B. The broker says "not found" — truthfully
// — so only the claim map can refuse. Without it, this Subscribe would create the
// durable under B's filter, and a later restart of the A subscription would then
// take it back, with the two feeds trading the same consumer on every deploy.
//
// It is also what proves Subscribe CONSULTS claimDurable at all; the unit tests
// above call it directly and would pass with the call site deleted.
func TestDurableClaimHoldsAfterTheConsumerIsGone(t *testing.T) {
	rig := newCollisionRig(t)
	subjA, subjB := rig.base+".a.b", rig.base+".a_b"

	subCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- rig.client.Subscribe(subCtx, subjA, rig.group, func(context.Context, Message) error { return nil })
	}()
	rig.awaitDurable(t, rig.client, subjA)
	cancel()
	<-done

	if err := rig.js.DeleteConsumer(context.Background(), rig.stream, durableName(rig.group, subjA)); err != nil {
		t.Fatalf("delete consumer: %v", err)
	}

	err := subscribeOutcome(t, rig.client, subjB, rig.group, 10*time.Second)
	if err == nil {
		t.Fatalf("Subscribe(%q) succeeded on a client that had already bound %q to the same durable %q — "+
			"the broker could not see the conflict (the consumer was deleted), so nothing else could",
			subjB, subjA, durableName(rig.group, subjA))
	}
	if !strings.Contains(err.Error(), subjA) {
		t.Errorf("refusal does not name the subject already holding the durable: %v", err)
	}
}

// TestDurableBoundToAMultiFilterConsumerIsRefused covers the plural-field arm.
//
// A JetStream consumer with several filters reports FilterSubject EMPTY and carries
// its filters in FilterSubjects instead (measured, nats-server 2.14.4). Comparing
// only the singular field would read such a consumer as `""` != want and refuse it
// anyway — but comparing only the singular in the OTHER direction, on a consumer
// this side is about to write with an empty FilterSubject, would match. This arm
// keeps the plural check honest rather than decorative.
func TestDurableBoundToAMultiFilterConsumerIsRefused(t *testing.T) {
	rig := newCollisionRig(t)
	ctx := context.Background()
	subj := rig.base + ".a.b"

	// An out-of-band consumer wearing exactly the name Subscribe is about to use,
	// bound to two subjects. `nats consumer add` can produce this.
	if _, err := rig.js.CreateOrUpdateConsumer(ctx, rig.stream, jetstream.ConsumerConfig{
		Durable:        durableName(rig.group, subj),
		AckPolicy:      jetstream.AckExplicitPolicy,
		FilterSubjects: []string{subj, rig.base + ".a.c"},
	}); err != nil {
		t.Fatalf("create multi-filter consumer: %v", err)
	}

	err := subscribeOutcome(t, rig.client, subj, rig.group, 10*time.Second)
	if err == nil {
		t.Fatalf("Subscribe(%q) took over a multi-filter consumer of the same name and narrowed it to one "+
			"subject — the other filter's deliveries stop with nothing said", subj)
	}
	if !strings.Contains(err.Error(), rig.base+".a.c") {
		t.Errorf("refusal does not name the other filter that would have been dropped: %v", err)
	}
}

// subscribeOutcome runs Subscribe and reports what it decided, BOUNDED. A non-nil
// return is a refusal; nil means it BOUND the durable and was still running when
// settle elapsed.
//
// Both directions need this, and calling Subscribe inline serves neither.
// Subscribe blocks on its context once it has bound, so a test expecting a refusal
// that stops refusing HANGS instead of failing — a hung suite reads as an
// environment problem rather than a defect, and a mutation that produces one has
// proved nothing. A test expecting success needs the same shape from the other
// side: it has to wait long enough to be sure no refusal is coming.
func subscribeOutcome(t *testing.T, c *NATSClient, subject, group string, settle time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.Subscribe(ctx, subject, group, func(context.Context, Message) error { return nil })
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(settle):
		cancel()
		<-done
		return nil
	}
}

// collisionRig is one throwaway stream, one client and one group, on the shared
// broker. Its subjects carry a per-run suffix and the stream is deleted on cleanup,
// so a run leaves no durable behind for a later test to bind by accident.
type collisionRig struct {
	base   string
	stream string
	group  string
	js     jetstream.JetStream
	client *NATSClient
}

func newCollisionRig(t *testing.T) *collisionRig {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set — this is the half of #1011 that needs a real CreateOrUpdateConsumer")
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	r := &collisionRig{base: "dur1011." + suffix, stream: "TEST_DUR1011_" + suffix, group: "g" + suffix}

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	r.js, err = jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := r.js.CreateStream(ctx, jetstream.StreamConfig{
		Name: r.stream, Subjects: []string{r.base + ".>"},
		Storage: jetstream.MemoryStorage, Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() { _ = r.js.DeleteStream(context.Background(), r.stream) })

	r.client, err = DialNATS(ctx, NATSConfig{URL: url, Name: "dur1011-rig"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = r.client.Close() })
	return r
}

// awaitDurable blocks until Subscribe has created the durable for subject. Pending
// looks up the EXISTING consumer and errors until then, so it is the readiness
// signal without a sleep.
func (r *collisionRig) awaitDurable(t *testing.T, c *NATSClient, subject string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := c.Pending(context.Background(), subject, r.group); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no durable appeared for %q", subject)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
