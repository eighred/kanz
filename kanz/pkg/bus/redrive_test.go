package bus_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

const (
	liveSubject   = "order.order.submit"
	parkedSubject = "dlq.order.order.submit"
)

// parkedMsg builds a message shaped the way Consumer.publishDLQ actually parks
// one. Built from the EXPORTED header constants rather than string literals, so
// a rename that breaks the parker/drainer contract breaks this test too rather
// than leaving it asserting against a spelling nothing writes any more.
func parkedMsg(mutate ...func(map[string]string)) bus.Message {
	h := map[string]string{
		bus.HeaderDLQOriginalSubject: liveSubject,
		bus.HeaderDLQAttempts:        "1",
		bus.HeaderDLQError:           "store load: connection refused",
		bus.HeaderDLQParkedAt:        time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		bus.HeaderDLQClass:           bus.ClassTransient,
		"Nats-Msg-Id":                "idem-key-1",
	}
	for _, m := range mutate {
		m(h)
	}
	return bus.Message{Subject: parkedSubject, Key: []byte("acct-1"), Body: []byte("frame-bytes"), Headers: h}
}

func TestPlanRedriveSendsATransientFailureBackToItsOriginalSubject(t *testing.T) {
	got, err := bus.PlanRedrive(parkedMsg(), bus.RedriveOptions{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("PlanRedrive: %v", err)
	}
	if got.Subject != liveSubject {
		t.Errorf("destination = %q, want %q — a redrive that does not land on the live subject "+
			"the OMS is listening to has not recovered anything", got.Subject, liveSubject)
	}
	if string(got.Body) != "frame-bytes" {
		t.Errorf("body = %q, want the parked bytes VERBATIM: the frame is already a stamped "+
			"envelope and re-minting it would break the causation chain", got.Body)
	}
	if string(got.Key) != "acct-1" {
		t.Errorf("key = %q, want the original partition key preserved — losing it reorders the "+
			"redriven order against its own account's other traffic", got.Key)
	}
	// The failure metadata describes a delivery that is over. Carrying it onto a
	// live subject would make a recovered order look parked to anything reading
	// headers.
	for _, h := range []string{
		bus.HeaderDLQOriginalSubject, bus.HeaderDLQAttempts,
		bus.HeaderDLQError, bus.HeaderDLQParkedAt, bus.HeaderDLQClass,
	} {
		if v, ok := got.Headers[h]; ok {
			t.Errorf("%s = %q survived onto the live subject; it describes the failed delivery, not this one", h, v)
		}
	}
	if got.Headers[bus.HeaderDLQRedrives] != "1" {
		t.Errorf("%s = %q, want \"1\" — the count is the loop bound and it must be stamped on the "+
			"way out, or a message that fails again comes back looking untouched",
			bus.HeaderDLQRedrives, got.Headers[bus.HeaderDLQRedrives])
	}
	// Re-stamped, or the destination stream's 2m dupe window answers the
	// republish with a successful PubAck and discards it.
	if id := got.Headers["Nats-Msg-Id"]; id == "idem-key-1" {
		t.Error("Nats-Msg-Id was carried through unchanged — inside the destination stream's " +
			"dupe window that publish is silently discarded and the redrive reports success " +
			"having done nothing")
	} else if id != "idem-key-1-redrive-1" {
		t.Errorf("Nats-Msg-Id = %q, want a DETERMINISTIC derivation (%q): a random id would turn "+
			"every crash between publish and ack into a duplicate order", id, "idem-key-1-redrive-1")
	}
}

// THE LOOP BOUND. This is the property that makes a drain safe to give an
// operator: a message cannot cycle DLQ → live → DLQ indefinitely.
func TestPlanRedriveRefusesOnceTheLoopBoundIsReached(t *testing.T) {
	opt := bus.RedriveOptions{MaxRedrives: 3}
	for _, n := range []string{"0", "1", "2"} {
		if _, err := bus.PlanRedrive(parkedMsg(withHeader(bus.HeaderDLQRedrives, n)), opt, time.Now().UTC()); err != nil {
			t.Fatalf("redrives=%s was refused but is within the budget of 3: %v", n, err)
		}
	}
	for _, n := range []string{"3", "4", "99"} {
		_, err := bus.PlanRedrive(parkedMsg(withHeader(bus.HeaderDLQRedrives, n)), opt, time.Now().UTC())
		if err == nil {
			t.Fatalf("redrives=%s was ALLOWED past a budget of 3 — the loop bound does not hold "+
				"and a permanently-failing message can be cycled forever", n)
		}
		if !errors.Is(err, bus.ErrRedriveRefused) {
			t.Errorf("redrives=%s: error %v does not match ErrRedriveRefused, so a caller cannot "+
				"tell a working safety limit from a broken drain", n, err)
		}
	}
}

// The count survives the round trip only because dlqHeaders copies inbound wire
// headers. That coupling is invisible at either site, so it is asserted
// end-to-end here: plan a redrive, park the RESULT the way the consumer would,
// and check the next plan sees a higher number.
func TestRedriveCountAccumulatesAcrossTheDLQRoundTrip(t *testing.T) {
	msg := parkedMsg()
	for want := 1; want <= 3; want++ {
		out, err := bus.PlanRedrive(msg, bus.RedriveOptions{MaxRedrives: 99}, time.Now().UTC())
		if err != nil {
			t.Fatalf("round trip %d: %v", want, err)
		}
		if got := out.Headers[bus.HeaderDLQRedrives]; got != fmt.Sprint(want) {
			t.Fatalf("round trip %d: %s = %q, want %q — the count is not accumulating, so the "+
				"loop bound resets every cycle and bounds nothing",
				want, bus.HeaderDLQRedrives, got, fmt.Sprint(want))
		}
		// Park it again exactly as a failing consumer would: same subject, headers
		// copied forward.
		msg = bus.Message{
			Subject: parkedSubject,
			Key:     out.Key,
			Body:    out.Body,
			Headers: bus.ExportedDLQHeaders(out.Headers, liveSubject, 1,
				errors.New("store load: connection refused"), time.Now().UTC().Add(-time.Hour)),
		}
	}
}

func TestPlanRedriveRefusesTerminalUnlessAsked(t *testing.T) {
	terminal := parkedMsg(withHeader(bus.HeaderDLQClass, bus.ClassTerminal))

	_, err := bus.PlanRedrive(terminal, bus.RedriveOptions{}, time.Now().UTC())
	if err == nil {
		t.Fatal("a TERMINAL message was redriven by default — a malformed payload will fail the " +
			"same way on arrival and burn the loop budget getting back to where it started")
	}
	if !errors.Is(err, bus.ErrRedriveRefused) {
		t.Errorf("error %v does not match ErrRedriveRefused", err)
	}

	if _, err := bus.PlanRedrive(terminal, bus.RedriveOptions{IncludeTerminal: true}, time.Now().UTC()); err != nil {
		t.Errorf("--include-terminal did not allow it: %v — an operator who has fixed the defect "+
			"must have a way to release the messages that hit it", err)
	}
}

// A handler that says NOTHING gets transient, and is therefore drainable. If
// this inverts, every ordinary store failure needs an extra operator flag to
// recover from, during an incident.
func TestUnclassifiedFailuresAreDrainableByDefault(t *testing.T) {
	msg := parkedMsg(func(h map[string]string) { delete(h, bus.HeaderDLQClass) })
	if _, err := bus.PlanRedrive(msg, bus.RedriveOptions{}, time.Now().UTC()); err != nil {
		t.Fatalf("a message with no %s header was refused: %v — the default must be the "+
			"recoverable one", bus.HeaderDLQClass, err)
	}
}

func TestPlanRedriveRefusesAMessageTooYoungToSurviveTheDedupWindows(t *testing.T) {
	now := time.Now().UTC()
	young := parkedMsg(withHeader(bus.HeaderDLQParkedAt, now.Add(-30*time.Second).Format(time.RFC3339Nano)))

	_, err := bus.PlanRedrive(young, bus.RedriveOptions{MinAge: 5 * time.Minute}, now)
	if err == nil {
		t.Fatal("a message parked 30s ago was redriven under a 5m minimum — inside the consumer's " +
			"dedup window the dispatch is skipped and acked, so the run reports success having " +
			"done nothing")
	}

	// Age unverifiable ⇒ refuse, not assume. "Nothing configured" must not look
	// like "checked, and fine".
	noStamp := parkedMsg(func(h map[string]string) { delete(h, bus.HeaderDLQParkedAt) })
	if _, err := bus.PlanRedrive(noStamp, bus.RedriveOptions{MinAge: 5 * time.Minute}, now); err == nil {
		t.Error("a message with no Parked-At header passed the age check — an age that cannot be " +
			"established was treated as an age that is fine")
	}
	// …and MinAge=0 is the documented, explicit override for exactly that case.
	if _, err := bus.PlanRedrive(noStamp, bus.RedriveOptions{MinAge: 0}, now); err != nil {
		t.Errorf("MinAge=0 did not disable the age check: %v — that is the only way to drain "+
			"messages parked before the header existed", err)
	}
}

// A rewritten destination header is the one way a redrive could put a payload
// onto a subject it never came from. Refusing the mismatch is what keeps a
// harmless `dlq.market.…` message from being turned into an order submission.
func TestPlanRedriveRefusesADestinationThatContradictsTheSubject(t *testing.T) {
	cases := map[string]bus.Message{
		"header names a different subject": parkedMsg(withHeader(bus.HeaderDLQOriginalSubject, "order.order.cancel")),
		"header missing entirely": parkedMsg(func(h map[string]string) {
			delete(h, bus.HeaderDLQOriginalSubject)
		}),
		"header points back into the DLQ namespace": {
			Subject: "dlq.dlq.order.order.submit",
			Headers: map[string]string{bus.HeaderDLQOriginalSubject: "dlq.order.order.submit"},
		},
		"read from a live subject, not the DLQ": {
			Subject: liveSubject,
			Headers: map[string]string{bus.HeaderDLQOriginalSubject: liveSubject},
		},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := bus.PlanRedrive(msg, bus.RedriveOptions{}, time.Now().UTC())
			if err == nil {
				t.Fatalf("ALLOWED, planning a publish to %q — a redrive must never route a payload "+
					"to a subject the parked message does not itself attest to", out.Subject)
			}
		})
	}
}

func TestPlanRedriveRefusesAnUnparseableLoopCount(t *testing.T) {
	for _, bad := range []string{"not-a-number", "-1"} {
		if _, err := bus.PlanRedrive(parkedMsg(withHeader(bus.HeaderDLQRedrives, bad)), bus.RedriveOptions{}, time.Now().UTC()); err == nil {
			t.Errorf("%s=%q was accepted — a loop bound that cannot be evaluated is not a bound",
				bus.HeaderDLQRedrives, bad)
		}
	}
}

// ---- the driver ----------------------------------------------------------

// fakeDLQ replays a fixed set of parked messages through the Subscriber
// contract and records which were acked (handler returned nil).
type fakeDLQ struct {
	msgs []bus.Message

	mu     sync.Mutex
	acked  []string
	nacked []string
}

func (f *fakeDLQ) Subscribe(ctx context.Context, subject, group string, h bus.Handler) error {
	for _, m := range f.msgs {
		if ctx.Err() != nil {
			break
		}
		err := h(ctx, m)
		f.mu.Lock()
		if err == nil {
			f.acked = append(f.acked, m.Headers[bus.HeaderDLQError])
		} else {
			f.nacked = append(f.nacked, m.Headers[bus.HeaderDLQError])
		}
		f.mu.Unlock()
	}
	<-ctx.Done()
	return nil
}

type recordingPublisher struct {
	mu   sync.Mutex
	sent []bus.Message
	fail error
}

func (p *recordingPublisher) Publish(_ context.Context, m bus.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	p.sent = append(p.sent, m)
	return nil
}

func TestRedriverDrainsAndStopsWhenIdle(t *testing.T) {
	src := &fakeDLQ{msgs: []bus.Message{
		parkedMsg(withHeader(bus.HeaderDLQError, "one")),
		parkedMsg(withHeader(bus.HeaderDLQError, "two")),
	}}
	dst := &recordingPublisher{}
	r := &bus.Redriver{Source: src, Dest: dst, IdleTimeout: 100 * time.Millisecond}

	stats, err := r.Run(context.Background(), parkedSubject, "kanz-redrive")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Redriven != 2 || stats.Inspected != 2 {
		t.Fatalf("stats = %+v, want 2 inspected / 2 redriven", stats)
	}
	if len(dst.sent) != 2 {
		t.Fatalf("published %d, want 2", len(dst.sent))
	}
	for _, m := range dst.sent {
		if m.Subject != liveSubject {
			t.Errorf("published to %q, want %q", m.Subject, liveSubject)
		}
	}
	if len(src.acked) != 2 {
		t.Errorf("acked %d of 2 — a redriven message that is not acked is redriven AGAIN by the "+
			"next run, which on this subject means a duplicate order", len(src.acked))
	}
}

// HALT AND SURFACE, DO NOT SKIP — the #219 decision, applied here. The message
// after the refusal must NOT be drained: stepping over a parked capital-path
// order is the loss this tool exists to prevent.
func TestRedriverHaltsOnRefusalAndLeavesItParked(t *testing.T) {
	src := &fakeDLQ{msgs: []bus.Message{
		parkedMsg(withHeader(bus.HeaderDLQError, "first-ok")),
		parkedMsg(withHeader(bus.HeaderDLQRedrives, "9"), withHeader(bus.HeaderDLQError, "exhausted")),
		parkedMsg(withHeader(bus.HeaderDLQError, "behind-the-blockage")),
	}}
	dst := &recordingPublisher{}
	r := &bus.Redriver{Source: src, Dest: dst, Options: bus.RedriveOptions{MaxRedrives: 3}, IdleTimeout: time.Second}

	stats, err := r.Run(context.Background(), parkedSubject, "kanz-redrive")
	if err == nil {
		t.Fatal("Run succeeded despite a message that exceeded the loop bound — the refusal did " +
			"not surface, so an operator would read this run as a clean drain")
	}
	var refusal *bus.RedriveRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a *RedriveRefusal, so the caller cannot distinguish a holding "+
			"safety limit from a broken transport", err)
	}
	if stats.Redriven != 1 {
		t.Errorf("redriven = %d, want 1 (the message before the refusal)", stats.Redriven)
	}
	if len(dst.sent) != 1 {
		t.Fatalf("published %d, want 1 — the run continued past the refusal", len(dst.sent))
	}
	for _, e := range src.acked {
		if e == "exhausted" {
			t.Error("the REFUSED message was acked — its durable cursor has moved past a parked " +
				"capital-path order, and the one recoverable copy is now unreachable")
		}
		if e == "behind-the-blockage" {
			t.Error("a message BEHIND the refusal was drained; the run did not halt")
		}
	}
}

func TestRedriverHaltsWithoutAckingWhenThePublishFails(t *testing.T) {
	src := &fakeDLQ{msgs: []bus.Message{parkedMsg(withHeader(bus.HeaderDLQError, "boom"))}}
	dst := &recordingPublisher{fail: errors.New("stream not found")}
	r := &bus.Redriver{Source: src, Dest: dst, IdleTimeout: time.Second}

	stats, err := r.Run(context.Background(), parkedSubject, "kanz-redrive")
	if err == nil {
		t.Fatal("a failed publish was reported as a successful run")
	}
	if errors.Is(err, bus.ErrRedriveRefused) {
		t.Error("a transport failure was classified as a REFUSAL — those need opposite responses " +
			"from an operator (fix the spine vs. decide about one message)")
	}
	if stats.Redriven != 0 {
		t.Errorf("redriven = %d after a failed publish, want 0", stats.Redriven)
	}
	if len(src.acked) != 0 {
		t.Error("the message was acked even though the publish failed — that is the message " +
			"reaching neither its subject nor the DLQ, i.e. silent permanent loss")
	}
}

func TestRedriverStopsAtTheLimitWithoutDrainingTheRest(t *testing.T) {
	src := &fakeDLQ{msgs: []bus.Message{
		parkedMsg(withHeader(bus.HeaderDLQError, "one")),
		parkedMsg(withHeader(bus.HeaderDLQError, "two")),
		parkedMsg(withHeader(bus.HeaderDLQError, "three")),
	}}
	dst := &recordingPublisher{}
	r := &bus.Redriver{Source: src, Dest: dst, IdleTimeout: time.Second, Limit: 1}

	stats, err := r.Run(context.Background(), parkedSubject, "kanz-redrive")
	if err != nil {
		t.Fatalf("hitting the limit is what the operator asked for, not a failure: %v", err)
	}
	if stats.Redriven != 1 {
		t.Fatalf("redriven = %d, want exactly 1 — --limit 1 is the control an operator uses to "+
			"prove the drain on ONE order before releasing the rest", stats.Redriven)
	}
	if len(dst.sent) != 1 {
		t.Errorf("published %d, want 1", len(dst.sent))
	}
	if len(src.acked) != 1 {
		t.Errorf("acked %d, want 1 — the messages past the limit must stay parked for the next run", len(src.acked))
	}
}

func TestRedriverRefusesToDrainALiveSubject(t *testing.T) {
	r := &bus.Redriver{Source: &fakeDLQ{}, Dest: &recordingPublisher{}, IdleTimeout: 50 * time.Millisecond}
	_, err := r.Run(context.Background(), liveSubject, "kanz-redrive")
	if err == nil {
		t.Fatal("Run accepted a LIVE subject — it would read live traffic and republish it onto " +
			"itself, an infinite loop rather than a drain")
	}
	if !strings.Contains(err.Error(), "dlq.") {
		t.Errorf("error %q does not name the expected namespace", err)
	}
}

func TestRedriverRequiresADurableGroup(t *testing.T) {
	r := &bus.Redriver{Source: &fakeDLQ{}, Dest: &recordingPublisher{}, IdleTimeout: 50 * time.Millisecond}
	if _, err := r.Run(context.Background(), parkedSubject, ""); err == nil {
		t.Fatal("Run accepted an empty group — with no durable cursor the next run re-reads and " +
			"re-sends every message this one already redrove")
	}
}

func withHeader(k, v string) func(map[string]string) {
	return func(h map[string]string) { h[k] = v }
}
