package archive_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/archiver/internal/archive"
)

// fakeGate drives the single-writer gate BOTH ways. A gate that could only ever
// answer "not live" would let every test below pass while the refusal never
// fired, which is the one outcome that would matter in production.
type fakeGate struct {
	active bool
	err    error
	probed []string
}

func (g *fakeGate) ConsumerActive(_ context.Context, subject, _ string) (bool, error) {
	g.probed = append(g.probed, subject)
	return g.active, g.err
}

// fakeReader replays a fixed set of parked records, then blocks until the drain
// cancels — the same shape bus.KafkaClient.Subscribe has, so a drain that never
// cancelled would hang here exactly as it would against a real broker.
type fakeReader struct {
	parked    int64
	records   []bus.Message
	delivered int
	backlogFn func() ([]bus.PartitionBacklog, error)
}

func (r *fakeReader) Backlog(_ context.Context, _, _ string) ([]bus.PartitionBacklog, error) {
	if r.backlogFn != nil {
		return r.backlogFn()
	}
	return []bus.PartitionBacklog{{Messages: r.parked}}, nil
}

func (r *fakeReader) Subscribe(ctx context.Context, _, _ string, h bus.Handler) error {
	for _, m := range r.records {
		if ctx.Err() != nil {
			break
		}
		r.delivered++
		if err := h(ctx, m); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func parked(t *testing.T, origin string, e *envelopepb.Envelope, headers map[string]string) bus.Message {
	t.Helper()
	h := map[string]string{
		bus.HeaderDLQOriginalSubject: origin,
		bus.HeaderDLQClass:           bus.ClassTerminal,
	}
	for k, v := range headers {
		h[k] = v
	}
	return bus.Message{Subject: archive.DLQSubject, Body: body(t, e), Headers: h}
}

func newDrain(cfg archive.DrainConfig) *archive.Drain {
	if cfg.Tenant == "" {
		cfg.Tenant = "acme"
	}
	if cfg.Group == "" {
		cfg.Group = "archiver"
	}
	if cfg.Subjects == nil {
		cfg.Subjects = []string{"order.>"}
	}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return archive.NewDrain(cfg)
}

func routable() *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventType:    "order.order.submitted",
		TenantId:     "acme",
		EventId:      "evt-1",
		PartitionKey: "portfolio-7",
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
}

func run(t *testing.T, d *archive.Drain) (archive.DrainReport, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.Run(ctx)
}

// THE ORDERING GUARANTEE. The archiver is a single writer by deployment because
// two producers can invert per-key order in Kafka. This refusal is the entire
// reason the drain is safe to exist, so it is asserted before anything else.
func TestDrainRefusesWhileTheArchiverIsConsuming(t *testing.T) {
	k := &fakeKafka{}
	d := newDrain(archive.DrainConfig{
		Kafka:  k,
		Reader: &fakeReader{parked: 1, records: []bus.Message{parked(t, "order.order.submitted", routable(), nil)}},
		Gate:   &fakeGate{active: true},
		Write:  true,
	})

	_, err := run(t, d)
	if !errors.Is(err, archive.ErrArchiverLive) {
		t.Fatalf("a drain ran beside a LIVE archiver: two producers can invert per-key order in Kafka "+
			"and a reordered log rebuilds a different book downstream. err = %v", err)
	}
	if len(k.got) != 0 {
		t.Fatalf("the drain published %d message(s) after the gate refused", len(k.got))
	}
}

// UNKNOWN IS NOT IDLE. A probe that errored means the broker did not answer,
// which is not evidence the archiver is stopped.
func TestDrainTreatsAnUnansweredProbeAsLive(t *testing.T) {
	k := &fakeKafka{}
	d := newDrain(archive.DrainConfig{
		Kafka:  k,
		Reader: &fakeReader{parked: 1, records: []bus.Message{parked(t, "order.order.submitted", routable(), nil)}},
		Gate:   &fakeGate{err: errors.New("broker unreachable")},
		Write:  true,
	})

	_, err := run(t, d)
	if !errors.Is(err, archive.ErrArchiverLive) {
		t.Fatalf("an unanswered gate probe was treated as an idle archiver; err = %v", err)
	}
	if len(k.got) != 0 {
		t.Fatalf("published %d message(s) despite an unanswered probe", len(k.got))
	}
}

// A guard with nothing to check passes vacuously, which reads identically to a
// guard that checked and was satisfied.
func TestDrainRefusesWhenTheGateHasNothingToProbe(t *testing.T) {
	k := &fakeKafka{}
	// Built directly rather than through newDrain, which fills Subjects in: the
	// whole point here is the empty one.
	d := archive.NewDrain(archive.DrainConfig{
		Tenant: "acme", Group: "archiver", Subjects: nil,
		Kafka: k, Reader: &fakeReader{parked: 1}, Gate: &fakeGate{}, Write: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	_, err := run(t, d)
	if err == nil {
		t.Fatal("a drain with no subjects ran: its single-writer gate probed nothing and passed vacuously")
	}
	if !strings.Contains(err.Error(), "vacuously") && !strings.Contains(err.Error(), "no subjects") {
		t.Errorf("the refusal must say the gate had nothing to probe; got %v", err)
	}
}

// EVERY subject, not the first: the archiver runs one durable per subject, so a
// probe of one says nothing about the rest.
func TestDrainProbesEverySubject(t *testing.T) {
	g := &fakeGate{}
	d := newDrain(archive.DrainConfig{
		Kafka:    &fakeKafka{},
		Reader:   &fakeReader{parked: 0},
		Gate:     g,
		Subjects: []string{"order.>", "exec.>", "risk.>"},
	})

	if _, err := run(t, d); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(g.probed) != 3 {
		t.Fatalf("the gate probed %d subject(s) (%v), want all 3 — one durable per subject, so a probe "+
			"of one says nothing about the others", len(g.probed), g.probed)
	}
}

// The default is a dry run, and a dry run writes NOTHING — not the re-archive,
// and not a re-park either. A read-only run that mutated a redrive count would be
// changing the thing it was asked only to inspect.
func TestDrainDryRunWritesNothing(t *testing.T) {
	k := &fakeKafka{}
	d := newDrain(archive.DrainConfig{
		Kafka:  k,
		Reader: &fakeReader{parked: 1, records: []bus.Message{parked(t, "order.order.submitted", routable(), nil)}},
		Gate:   &fakeGate{},
		// Write deliberately absent: the zero value must be the safe one.
	})

	rep, err := run(t, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.DryRun {
		t.Error("the zero value of Write produced a WRITING run; the safe state must be the default")
	}
	if rep.Routed != 1 {
		t.Errorf("Routed = %d, want 1 — a dry run still reports what it WOULD do", rep.Routed)
	}
	if rep.Produced != 0 {
		t.Errorf("Produced = %d, want 0 in a dry run", rep.Produced)
	}
	if len(k.got) != 0 {
		t.Fatalf("a DRY RUN published %d message(s): %+v", len(k.got), k.got)
	}
}

// The drain is a transport, like the archiver: body verbatim, key from
// partition_key, routed by the CURRENT table rather than a cached destination.
func TestDrainReArchivesVerbatimToTheMappedTopic(t *testing.T) {
	e := routable()
	msg := parked(t, "order.order.submitted", e, nil)
	k := &fakeKafka{}
	d := newDrain(archive.DrainConfig{
		Kafka:  k,
		Reader: &fakeReader{parked: 1, records: []bus.Message{msg}},
		Gate:   &fakeGate{},
		Write:  true,
	})

	rep, err := run(t, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Produced != 1 {
		t.Fatalf("Produced = %d, want 1", rep.Produced)
	}
	if len(k.got) != 1 {
		t.Fatalf("published %d messages, want 1", len(k.got))
	}
	out := k.got[0]
	if out.Subject != "acme.order.order" {
		t.Errorf("re-archived to %q, want %q — the drain must route by the CURRENT table",
			out.Subject, "acme.order.order")
	}
	if string(out.Key) != "portfolio-7" {
		t.Errorf("key = %q, want the envelope's partition_key: events sharing a key must land on one "+
			"partition, in order", out.Key)
	}
	if string(out.Body) != string(msg.Body) {
		t.Error("the body was rewritten on its way into the log of record, so it is no longer a record " +
			"of what happened")
	}
	if out.Headers[archive.HeaderEventID] != "evt-1" {
		t.Errorf("event id header = %q, want evt-1 — it is the downstream dedup key",
			out.Headers[archive.HeaderEventID])
	}
}

// An event the fixed table STILL cannot route is re-parked, not skipped. Kafka
// offsets are sequential, so a record left alone is gone when the offset advances
// past it and the 30-day retention fires.
func TestDrainReparksWhatStillCannotRoute(t *testing.T) {
	bad := &envelopepb.Envelope{
		EventType:  "not-a-valid-type",
		TenantId:   "acme",
		EventId:    "evt-bad",
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	k := &fakeKafka{}
	d := newDrain(archive.DrainConfig{
		Kafka:  k,
		Reader: &fakeReader{parked: 1, records: []bus.Message{parked(t, "order.order.submitted", bad, nil)}},
		Gate:   &fakeGate{},
		Write:  true,
	})

	rep, err := run(t, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Routed != 0 || rep.Produced != 0 {
		t.Fatalf("an unroutable event was counted as routed/produced: %+v", rep)
	}
	if rep.Reparked != 1 {
		t.Fatalf("Reparked = %d, want 1 — a skipped record is LOST once the offset moves past it", rep.Reparked)
	}
	if len(k.got) != 1 {
		t.Fatalf("published %d messages, want exactly the re-park", len(k.got))
	}
	rp := k.got[0]
	if rp.Subject != archive.DLQSubject {
		t.Errorf("re-parked to %q, want %q", rp.Subject, archive.DLQSubject)
	}
	if got := rp.Headers[bus.HeaderDLQRedrives]; got != "1" {
		t.Errorf("%s = %q, want \"1\" — the count is the loop bound that stops this draining forever",
			bus.HeaderDLQRedrives, got)
	}
	if rp.Headers[bus.HeaderDLQOriginalSubject] != "order.order.submitted" {
		t.Error("the re-park dropped the original subject, so the next drain cannot say where it came from")
	}
}

// An existing redrive count carries forward rather than resetting, or the loop
// bound would never be reached.
func TestDrainIncrementsAnExistingRedriveCount(t *testing.T) {
	bad := &envelopepb.Envelope{EventType: "nope", TenantId: "acme", EventId: "e", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
	k := &fakeKafka{}
	d := newDrain(archive.DrainConfig{
		Kafka: k,
		Reader: &fakeReader{parked: 1, records: []bus.Message{
			parked(t, "order.order.submitted", bad, map[string]string{bus.HeaderDLQRedrives: "2"}),
		}},
		Gate:  &fakeGate{},
		Write: true,
	})

	if _, err := run(t, d); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := k.got[0].Headers[bus.HeaderDLQRedrives]; got != "3" {
		t.Errorf("%s = %q, want \"3\" — a count that resets means the bound is never reached",
			bus.HeaderDLQRedrives, got)
	}
}

// THE RE-PARK LOOP. A run reads only what was parked when it STARTED. Without
// that bound the drain reads its own re-parks and spins until the redrive limit.
func TestDrainReadsOnlyWhatWasParkedWhenItStarted(t *testing.T) {
	bad := &envelopepb.Envelope{EventType: "nope", TenantId: "acme", EventId: "e", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
	// The topic holds five records, but only two were parked when the run began —
	// the rest stand in for this run's own re-parks landing behind them.
	recs := make([]bus.Message, 5)
	for i := range recs {
		recs[i] = parked(t, "order.order.submitted", bad, nil)
	}
	r := &fakeReader{parked: 2, records: recs}
	d := newDrain(archive.DrainConfig{Kafka: &fakeKafka{}, Reader: r, Gate: &fakeGate{}, Write: true})

	rep, err := run(t, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Read != 2 {
		t.Fatalf("read %d records, want 2 — an unbounded drain reads its own re-parks and never "+
			"terminates", rep.Read)
	}
}

// Nothing parked is a real answer, and it must not look like a run that was
// refused or that failed.
func TestDrainReportsAnEmptyTopicAsEmpty(t *testing.T) {
	k := &fakeKafka{}
	d := newDrain(archive.DrainConfig{Kafka: k, Reader: &fakeReader{parked: 0}, Gate: &fakeGate{}, Write: true})

	rep, err := run(t, d)
	if err != nil {
		t.Fatalf("Run on an empty topic must succeed, not error: %v", err)
	}
	if rep.Parked != 0 || rep.Read != 0 || rep.Produced != 0 {
		t.Errorf("empty-topic report is not empty: %+v", rep)
	}
	if len(k.got) != 0 {
		t.Errorf("published %d message(s) with nothing parked", len(k.got))
	}
}

// A backlog probe that fails must stop the run: "the broker did not answer" is
// not "there is nothing parked".
func TestDrainStopsWhenItCannotCountWhatIsParked(t *testing.T) {
	d := newDrain(archive.DrainConfig{
		Kafka: &fakeKafka{},
		Reader: &fakeReader{backlogFn: func() ([]bus.PartitionBacklog, error) {
			return nil, errors.New("metadata request failed")
		}},
		Gate:  &fakeGate{},
		Write: true,
	})

	if _, err := run(t, d); err == nil {
		t.Fatal("a failed backlog probe was treated as an empty topic")
	}
}
