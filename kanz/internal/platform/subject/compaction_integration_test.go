package subject_test

// THE DEFECT, AT THE LAYER IT ACTUALLY LIVES (#999).
//
// The unit tests prove Token is injective. That is necessary and not sufficient: what
// broke was a COMPACTED JETSTREAM STREAM keeping one message per subject, so the claim
// worth proving is that two instruments which used to collide now retain their own
// current state and BOTH come back from DeliverLastPerSubject — the single read a booting
// consumer uses to learn the whole book.
//
// Gated on TEST_NATS_URL. A JetStream broker needs no Docker: `go install
// github.com/nats-io/nats-server/v2@latest && nats-server -js -sd <dir>`, then bootstrap
// the POSITION stream from infra/nats/bootstrap-job.yaml.

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

	"github.com/eighred/kanz/internal/platform/subject"
)

// positionStream binds the REAL POSITION stream and refuses to run against anything else.
//
// THE COMPACTION IS THE SUBJECT OF THE TEST. On a stream without MaxMsgsPerSubject=1
// every message is simply retained, every assertion below holds, and the run is green
// while proving nothing about the store this defect lives in. Asserted, not assumed —
// the same reason internal/compliance's mandate tests assert the MANDATE config.
func positionStream(t *testing.T, ctx context.Context) jetstream.JetStream {
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
	st, err := js.Stream(ctx, "POSITION")
	if err != nil {
		t.Skipf("no POSITION stream on this broker (%v) — bootstrap the CI topology first; a "+
			"scratch stream would retain every message and pass this test for the wrong reason", err)
	}
	info, err := st.Info(ctx)
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
	return js
}

func positionPayload(t *testing.T, portfolio, instrument string, qty int64) []byte {
	t.Helper()
	b, err := proto.Marshal(&domainpb.PositionState{
		PortfolioId:  portfolio,
		InstrumentId: instrument,
		Quantity:     &commonpb.Decimal{Coefficient: qty, Exponent: 0},
		AsOf:         timestamppb.New(time.Now().UTC()),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TWO INSTRUMENTS, TWO RETAINED HOLDINGS, ONE READ.
//
// `VOD.L` and `VOD_L` used to be one subject. Whichever traded last was the position the
// stream kept, and the other holding was simply absent from the book every consumer arms
// from — including the compliance monitor's, which is what makes a fund holding a
// forbidden instrument look compliant.
//
// Each instrument is published TWICE, so the test also proves the stream is doing what it
// is configured to do: keep the LATEST per subject, not merely keep both messages.
func TestCollidingInstrumentsRetainSeparateStateOnTheCompactedStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	js := positionStream(t, ctx)

	// A per-run portfolio. The POSITION stream never ages a message out, so a broker
	// reused between runs would otherwise carry the previous run's holdings into this
	// one's read and the assertion would drift.
	portfolio := fmt.Sprintf("pf-999-%d", time.Now().UnixNano())
	tenant := "acme"

	type holding struct {
		instrument string
		qty        int64
	}
	// The pairs #999 names, and the empty-id sentinel that used to collide with an id
	// that IS an underscore.
	holdings := []holding{
		{"VOD.L", 100},
		{"VOD_L", 200},
		{"AAPL US Equity", 300},
		{"AAPL_US_Equity", 400},
		{"BTC-USD", 500},
	}

	for _, h := range holdings {
		subj := subject.PositionFor(tenant, portfolio, h.instrument)
		// Published twice: the first value must be the one compaction DISCARDS.
		if _, err := js.Publish(ctx, subj, positionPayload(t, portfolio, h.instrument, h.qty-1)); err != nil {
			t.Fatalf("publish %q on %q: %v", h.instrument, subj, err)
		}
		if _, err := js.Publish(ctx, subj, positionPayload(t, portfolio, h.instrument, h.qty)); err != nil {
			t.Fatalf("publish %q on %q: %v", h.instrument, subj, err)
		}
	}

	// THE READ A BOOTING CONSUMER MAKES: the current state of every holding, in one pass.
	cons, err := js.CreateConsumer(ctx, "POSITION", jetstream.ConsumerConfig{
		DeliverPolicy: jetstream.DeliverLastPerSubjectPolicy,
		FilterSubject: subject.PositionChanged + "." + subject.Token(tenant) + "." + subject.Token(portfolio) + ".>",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	book := map[string]int64{}
	batch, err := cons.Fetch(len(holdings)+8, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for msg := range batch.Messages() {
		var ps domainpb.PositionState
		if err := proto.Unmarshal(msg.Data(), &ps); err != nil {
			t.Fatalf("unmarshal from %q: %v", msg.Subject(), err)
		}
		// The consumer folds by the PAYLOAD id, exactly as the compliance monitor
		// (monitor.go: b.positions[ps.GetInstrumentId()]), the risk engine and
		// webhook-ingest do.
		book[ps.GetInstrumentId()] = ps.GetQuantity().GetCoefficient()
		_ = msg.Ack()
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("batch: %v", err)
	}

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
// Existing compacted subjects carry the LOSSY spelling: a holding of `VOD.L` published
// before this fix sits on `...VOD_L` and will never be overwritten, because `VOD.L` now
// publishes to `...VOD%2EL`. That residue is harmless, and this proves why rather than
// asserting it in prose: every consumer of the position spine folds by the PAYLOAD's
// instrument_id, not by the subject, so a legacy message arms the book under the id it
// was always about. The stale subject self-heals on that instrument's next fill.
//
// What the fix changes is not the legacy message — it is that `VOD.L` and `VOD_L` can no
// longer be the same message.
func TestALegacyLossySubjectStillArmsTheBookUnderItsRealInstrument(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	js := positionStream(t, ctx)

	portfolio := fmt.Sprintf("pf-999-legacy-%d", time.Now().UnixNano())
	tenant := "acme"

	// A message written by the OLD encoder: instrument `VOD.L`, subject token `VOD_L`.
	legacy := subject.PositionChanged + "." + tenant + "." + portfolio + ".VOD_L"
	if _, err := js.Publish(ctx, legacy, positionPayload(t, portfolio, "VOD.L", 100)); err != nil {
		t.Fatalf("publish legacy: %v", err)
	}
	// The same instrument's next fill, under the new encoder.
	if _, err := js.Publish(ctx, subject.PositionFor(tenant, portfolio, "VOD.L"),
		positionPayload(t, portfolio, "VOD.L", 150)); err != nil {
		t.Fatalf("publish current: %v", err)
	}

	cons, err := js.CreateConsumer(ctx, "POSITION", jetstream.ConsumerConfig{
		DeliverPolicy: jetstream.DeliverLastPerSubjectPolicy,
		FilterSubject: subject.PositionChanged + "." + tenant + "." + portfolio + ".>",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	book := map[string]int64{}
	batch, err := cons.Fetch(8, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for msg := range batch.Messages() {
		var ps domainpb.PositionState
		if err := proto.Unmarshal(msg.Data(), &ps); err != nil {
			t.Fatal(err)
		}
		book[ps.GetInstrumentId()] = ps.GetQuantity().GetCoefficient()
		_ = msg.Ack()
	}
	if len(book) != 1 {
		t.Fatalf("the legacy and current subjects armed %d instruments, want 1 — they are the same "+
			"holding and must fold onto one id: %v", len(book), book)
	}
	if got := book["VOD.L"]; got != 150 {
		t.Errorf("VOD.L armed at %d, want 150 — the stale subject must not win over the current one; "+
			"messages arrive in stream order, so the later publish is the one that stands", got)
	}
}
