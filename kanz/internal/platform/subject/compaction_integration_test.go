package subject_test

// THE DEFECT, AT THE LAYER IT ACTUALLY LIVES (#999).
//
// The unit tests prove Token is injective. That is necessary and not sufficient:
// what broke was a COMPACTED JETSTREAM STREAM keeping one message per subject, so
// the claim worth proving is that two instruments which used to collide now
// retain their own current state and BOTH come back from DeliverLastPerSubject —
// the single read a booting consumer uses to learn the whole book.
//
// # THESE PUBLISH FRAMED FACTs, AND THEY CLEAN UP AFTER THEMSELVES
//
// Both halves are load-bearing, and both were learned the expensive way. The
// first version of this file published RAW domain protos straight through
// jetstream.Publish, and purged nothing.
//
// The POSITION stream keeps one message per subject with NO max age. So every run
// left permanent, UNDECODABLE messages on `risk.position.changed.>`: a real
// consumer cold-starting from DeliverLastPerSubject hit them, bus.Unframe failed
// with "cannot parse invalid wire-format data", and the arming never completed.
// Measured on a broker that had run the suite twice — 28 undecodable retained
// messages, and three compliance-monitor arming tests that pass on a fresh broker
// timing out against it.
//
// CI never saw it, because CI gets a new broker every run. Every local suite run
// after the first went red, and the cause looked like the code under test rather
// than like litter. A test that writes to a shared, never-ageing stream must
// write what a real producer writes, and must take back what it wrote.
//
// Gated on TEST_NATS_URL. A JetStream broker needs no Docker: `go install
// github.com/nats-io/nats-server/v2@latest && nats-server -js -sd <dir>`, then
// bootstrap the POSITION stream from infra/nats/bootstrap-job.yaml.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/pkg/bus"
)

const positionTestTenant = "acme"

// positionRig binds the REAL POSITION stream, refuses to run against anything
// else, and publishes through a REAL producer.
//
// THE COMPACTION IS THE SUBJECT OF THE TEST. On a stream without
// MaxMsgsPerSubject=1 every message is simply retained, every assertion below
// holds, and the run is green while proving nothing about the store this defect
// lives in. Asserted, not assumed — the same reason internal/compliance's mandate
// tests assert the MANDATE config.
type positionRig struct {
	stream   jetstream.Stream
	producer *bus.Producer
}

func newPositionRig(t *testing.T, ctx context.Context) *positionRig {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the position subjects over a real compacted stream")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "POSITION")
	if err != nil {
		t.Skipf("no POSITION stream on this broker (%v) — bootstrap the CI topology first; a "+
			"scratch stream would retain every message and pass this test for the wrong reason", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxMsgsPerSubject != 1 {
		t.Fatalf("POSITION.MaxMsgsPerSubject = %d, want 1. This test exists because the stream keeps "+
			"exactly one message per holding; on a stream that keeps more it asserts nothing",
			info.Config.MaxMsgsPerSubject)
	}
	if info.Config.MaxAge != 0 {
		t.Fatalf("POSITION.MaxAge = %s, want 0 — a holding that ages off the stream is a position "+
			"the next restart is blind to", info.Config.MaxAge)
	}

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "position-subject-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "kanz-position-it", ProducerVersion: "it", Tenant: positionTestTenant,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &positionRig{stream: stream, producer: producer}
}

// publish writes ONE position FACT the way the OMS projector writes it — through
// a real bus.Producer, so what lands on the stream is a framed, validated
// EventFrame and not a bare domain proto no consumer can decode.
func (r *positionRig) publish(t *testing.T, ctx context.Context, subj, portfolio, instrument string, qty int64) {
	t.Helper()
	now := time.Now().UTC()
	err := r.producer.Publish(ctx, bus.Event{
		Subject:       subj,
		EventType:     subject.PositionChanged,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        "risk",
		EventTime:     now,
		// A FACT carries no IdempotencyKey; the broker refuses one that does.
		PartitionKey:     subj,
		PayloadSchemaRef: "domain.v1.PositionState:1",
		Payload: &domainpb.PositionState{
			PortfolioId:  portfolio,
			InstrumentId: instrument,
			Quantity:     &commonpb.Decimal{Coefficient: qty, Exponent: 0},
			AsOf:         timestamppb.New(now),
		},
	})
	if err != nil {
		t.Fatalf("publish %q on %q: %v", instrument, subj, err)
	}
}

// reclaim removes every subject this run wrote.
//
// The POSITION stream has no max age, so without this each run leaves its
// portfolios on the stream forever and every later consumer arming from
// `risk.position.changed.>` folds them. Purging by subject is exact: it touches
// only what this run created, never another test's holdings.
func (r *positionRig) reclaim(t *testing.T, subjects []string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, subj := range subjects {
			if err := r.stream.Purge(ctx, jetstream.WithPurgeSubject(subj)); err != nil {
				t.Logf("could not reclaim %q (%v) — a later consumer on this broker will fold it", subj, err)
			}
		}
	})
}

// armBook does what a booting consumer does: DeliverLastPerSubject over the
// portfolio's holdings, in one read, decoding each message the way every real
// consumer decodes it.
func (r *positionRig) armBook(t *testing.T, ctx context.Context, filter string, want int) map[string]int64 {
	t.Helper()
	cons, err := r.stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		DeliverPolicy: jetstream.DeliverLastPerSubjectPolicy,
		FilterSubject: filter,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	book := map[string]int64{}
	batch, err := cons.Fetch(want+8, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for msg := range batch.Messages() {
		env, payload, err := bus.Unframe(msg.Data())
		if err != nil {
			t.Fatalf("a message on %q does not unframe (%v) — this is what a real consumer hits, and "+
				"it stops the cold start dead", msg.Subject(), err)
		}
		if got := env.GetEventType(); got != subject.PositionChanged {
			t.Errorf("event_type = %q on %q", got, msg.Subject())
		}
		var ps domainpb.PositionState
		if err := proto.Unmarshal(payload, &ps); err != nil {
			t.Fatalf("payload from %q: %v", msg.Subject(), err)
		}
		// Folded by the PAYLOAD id, exactly as the compliance monitor
		// (monitor.go: b.positions[ps.GetInstrumentId()]), the risk engine and
		// webhook-ingest do.
		book[ps.GetInstrumentId()] = ps.GetQuantity().GetCoefficient()
		_ = msg.Ack()
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("batch: %v", err)
	}
	return book
}

// TWO INSTRUMENTS, TWO RETAINED HOLDINGS, ONE READ.
//
// `VOD.L` and `VOD_L` used to be one subject. Whichever traded last was the
// position the stream kept, and the other holding was simply absent from the book
// every consumer arms from — including the compliance monitor's, which is what
// makes a fund holding a forbidden instrument look compliant.
//
// Each instrument is published TWICE, so the test also proves the stream is doing
// what it is configured to do: keep the LATEST per subject, not merely keep both.
func TestCollidingInstrumentsRetainSeparateStateOnTheCompactedStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rig := newPositionRig(t, ctx)

	// A per-run portfolio. The POSITION stream never ages a message out, so a
	// broker reused between runs would otherwise carry the previous run's
	// holdings into this one's read.
	portfolio := fmt.Sprintf("pf-999-%d", time.Now().UnixNano())

	holdings := []struct {
		instrument string
		qty        int64
	}{
		{"VOD.L", 100},
		{"VOD_L", 200},
		{"AAPL US Equity", 300},
		{"AAPL_US_Equity", 400},
		{"BTC-USD", 500},
	}

	// RECLAIM IS REGISTERED BEFORE THE FIRST PUBLISH. A publish that fails calls
	// t.Fatalf, and anything written up to that point would otherwise stay on a
	// stream that never ages — the litter this test exists not to leave.
	subjects := make([]string, 0, len(holdings))
	for _, h := range holdings {
		subjects = append(subjects, subject.PositionFor(positionTestTenant, portfolio, h.instrument))
	}
	rig.reclaim(t, subjects)
	for i, h := range holdings {
		// Published twice: the first value must be the one compaction DISCARDS.
		rig.publish(t, ctx, subjects[i], portfolio, h.instrument, h.qty-1)
		rig.publish(t, ctx, subjects[i], portfolio, h.instrument, h.qty)
	}

	filter := subject.PositionChanged + "." + subject.Token(positionTestTenant) + "." +
		subject.Token(portfolio) + ".>"
	book := rig.armBook(t, ctx, filter, len(holdings))

	if len(book) != len(holdings) {
		t.Errorf("the book armed with %d holdings, want %d — a holding missing from this read is a "+
			"holding every consumer of the position spine is blind to: got %v", len(book), len(holdings), book)
	}
	for _, h := range holdings {
		got, ok := book[h.instrument]
		if !ok {
			t.Errorf("%q is ABSENT from the armed book — its current position was overwritten by "+
				"another instrument sharing its compacted subject", h.instrument)
			continue
		}
		if got != h.qty {
			t.Errorf("%q armed at %d, want %d — the stream kept the wrong message for this subject",
				h.instrument, got, h.qty)
		}
	}
}

// THE MIGRATION QUESTION, ANSWERED ON THE STREAM (#999).
//
// Existing compacted subjects carry the LOSSY spelling: a holding of `VOD.L`
// published before this fix sits on `...VOD_L` and will never be overwritten,
// because `VOD.L` now publishes to `...VOD%2EL`. That residue is harmless, and
// this proves why rather than asserting it in prose: every consumer of the
// position spine folds by the PAYLOAD's instrument_id, not by the subject, so a
// legacy message arms the book under the id it was always about. The stale
// subject self-heals on that instrument's next fill.
//
// What the fix changes is not the legacy message — it is that `VOD.L` and `VOD_L`
// can no longer be the same message.
func TestALegacyLossySubjectStillArmsTheBookUnderItsRealInstrument(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rig := newPositionRig(t, ctx)

	portfolio := fmt.Sprintf("pf-999-legacy-%d", time.Now().UnixNano())

	// A message written by the OLD encoder: instrument `VOD.L`, subject token
	// `VOD_L`. Framed, because that is what the old encoder's producer wrote too.
	legacy := subject.PositionChanged + "." + positionTestTenant + "." + portfolio + ".VOD_L"
	current := subject.PositionFor(positionTestTenant, portfolio, "VOD.L")
	rig.reclaim(t, []string{legacy, current})

	rig.publish(t, ctx, legacy, portfolio, "VOD.L", 100)
	rig.publish(t, ctx, current, portfolio, "VOD.L", 150)

	book := rig.armBook(t, ctx,
		subject.PositionChanged+"."+positionTestTenant+"."+portfolio+".>", 2)

	if len(book) != 1 {
		t.Fatalf("the legacy and current subjects armed %d instruments, want 1 — they are the same "+
			"holding and must fold onto one id: %v", len(book), book)
	}
	if got := book["VOD.L"]; got != 150 {
		t.Errorf("VOD.L armed at %d, want 150 — the stale subject must not win over the current one; "+
			"messages arrive in stream order, so the later publish is the one that stands", got)
	}
}
