// Package replay holds the EVT-21d replay-determinism contract tests:
// running the same source log through the same Pipeline twice MUST
// produce byte-identical output, and the per-event envelope identity
// (event_id, idempotency_key, producer_sequence, lineage) MUST survive
// the re-marshal unchanged. Only QualityFlags is permitted to differ —
// QUALITY_FLAG_REPLAYED is appended idempotently by EVT-20c.
//
// kanz-schemas/README.md § Schema Evolution §8 names this contract directly:
// "replaying an old log with current code must produce identical output, which
// is only true if every intervening change was genuinely non-breaking or
// genuinely a new vN." The tests here are the runtime proof — a regression in
// the Pipeline marshal step, in the proto generator's unknown-field handling,
// or in StampReplayed's idempotency would break determinism and surface here
// before it silently corrupted a production replay.
//
// Tests sit in `replay_test` (external) so they consume only the
// public `tools/replay` + `pkg/bus` surface, matching the other
// EVT-21 contract trees (envelope, serialization, compatibility).
package replay_test

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/tools/replay"
)

// fixedTime pins every fixture timestamp so the wire bytes of two
// otherwise-identical Pipeline runs can be byte-compared meaningfully.
var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeSource is the external-package mirror of run_test.go's helper.
// One Pipeline run drains it via Next; each test constructs a fresh
// instance so the index resets between runs.
type fakeSource struct {
	steps []replay.Event
	i     int
}

func (f *fakeSource) Next(_ context.Context) (replay.Event, error) {
	if f.i >= len(f.steps) {
		return replay.Event{}, io.EOF
	}
	e := f.steps[f.i]
	f.i++
	return e, nil
}

// capturePub records every published message. Goroutine-safety is
// unneeded — Pipeline.Run is single-goroutine.
type capturePub struct {
	messages []bus.Message
}

func (c *capturePub) Publish(_ context.Context, m bus.Message) error {
	c.messages = append(c.messages, m)
	return nil
}

// canonicalEnvelope builds an envelope where every field carries a
// deterministic, distinct value. Each call returns a fresh pointer so
// the test can hand independent copies to two Pipeline runs (Pipeline
// mutates Envelope.QualityFlags in place via StampReplayed).
func canonicalEnvelope(eventID, eventType, partitionKey string, seq uint64) *envelopepb.Envelope {
	ts := timestamppb.New(fixedTime)
	return &envelopepb.Envelope{
		EventId:          eventID,
		EventType:        eventType,
		SchemaVersion:    1,
		EnvelopeVersion:  1,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		Domain:           "market",
		EventTime:        ts,
		IngestionTime:    ts,
		PublishTime:      ts,
		CorrelationId:    eventID,
		Source:           "determinism-test/inst-1",
		ProducerVersion:  "det-1.0.0",
		PartitionKey:     partitionKey,
		ProducerSequence: seq,
		IdempotencyKey:   eventID,
		PayloadSchemaRef: "market.v1.MarketDataEvent:1",
	}
}

// canonicalLog returns a freshly-built source-log slice. Each call
// produces independent envelope pointers so two runs over "the same
// log" do not share mutable state.
func canonicalLog() []replay.Event {
	return []replay.Event{
		{
			Envelope:  canonicalEnvelope("evt-1", "market.equity.trade", "AAPL", 1),
			Payload:   []byte("payload-1"),
			Key:       []byte("AAPL"),
			Topic:     "market.equity",
			Partition: 0,
			Offset:    100,
			KafkaTime: fixedTime,
			Headers:   map[string]string{"Nats-Msg-Id": "evt-1"},
		},
		{
			Envelope:  canonicalEnvelope("evt-2", "market.equity.quote", "AAPL", 2),
			Payload:   []byte("payload-2"),
			Key:       []byte("AAPL"),
			Topic:     "market.equity",
			Partition: 0,
			Offset:    101,
			KafkaTime: fixedTime,
			Headers:   map[string]string{"Nats-Msg-Id": "evt-2"},
		},
		{
			Envelope:  canonicalEnvelope("evt-3", "market.equity.trade", "MSFT", 1),
			Payload:   []byte("payload-3"),
			Key:       []byte("MSFT"),
			Topic:     "market.equity",
			Partition: 1,
			Offset:    50,
			KafkaTime: fixedTime,
			Headers:   map[string]string{"Nats-Msg-Id": "evt-3"},
		},
	}
}

func runPipeline(t *testing.T, runID replay.RunID, log []replay.Event) []bus.Message {
	t.Helper()
	src := &fakeSource{steps: log}
	pub := &capturePub{}
	pl := &replay.Pipeline{RunID: runID, Source: src, Publisher: pub}
	if _, err := pl.Run(context.Background()); err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}
	return pub.messages
}

// --- Wire-determinism --------------------------------------------------

// The core EVT-21d claim: two Pipeline runs over the same source log
// with the same RunID produce byte-identical output. A regression that
// adds nondeterminism (e.g. a future Pipeline change that stamps
// per-run wall-clock metadata, or a proto-generator change that
// reorders fields nondeterministically) fails here.
func TestDeterminism_SameLogProducesIdenticalOutput(t *testing.T) {
	runID := replay.RunID("01900000000000000000000000000001")
	first := runPipeline(t, runID, canonicalLog())
	second := runPipeline(t, runID, canonicalLog())

	if len(first) != len(second) {
		t.Fatalf("message count diverged: %d vs %d", len(first), len(second))
	}
	for i := range first {
		assertMessagesByteIdentical(t, i, first[i], second[i])
	}
}

// Two runs of the same log with DIFFERENT RunIDs must produce
// byte-identical bodies (envelope + payload are identical) and only
// the subject differs (`replay.{runID}.…`). This proves the RunID is
// the only run-scoped input to the wire bytes and isolates the
// determinism property from accidental coupling.
func TestDeterminism_DifferentRunIDsDifferOnlyInSubject(t *testing.T) {
	a := runPipeline(t, "01900000000000000000000000000aaa", canonicalLog())
	b := runPipeline(t, "01900000000000000000000000000bbb", canonicalLog())

	if len(a) != len(b) {
		t.Fatalf("message count diverged: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i].Body, b[i].Body) {
			t.Errorf("message %d body diverged between RunIDs (envelope content should be RunID-independent)", i)
		}
		if !bytes.Equal(a[i].Key, b[i].Key) {
			t.Errorf("message %d key diverged: %q vs %q", i, a[i].Key, b[i].Key)
		}
		if !reflect.DeepEqual(a[i].Headers, b[i].Headers) {
			t.Errorf("message %d headers diverged: %v vs %v", i, a[i].Headers, b[i].Headers)
		}
		if a[i].Subject == b[i].Subject {
			t.Errorf("message %d subject equal across RunIDs: %q", i, a[i].Subject)
		}
	}
}

// Replay-of-replay: a Pipeline that consumes its own output (chained
// runs, an operator re-driving a partial replay) must converge to a
// byte-stable wire form because StampReplayed is idempotent and no
// other envelope mutation happens. This is the operational claim
// behind EVT-20c's idempotency: chained replays do not accumulate
// flags or drift in any other field.
func TestDeterminism_ReplayOfReplayConvergesToStableBytes(t *testing.T) {
	runID := replay.RunID("01900000000000000000000000000002")
	first := runPipeline(t, runID, canonicalLog())

	// Re-feed the first run's output as the next run's source. Each
	// re-fed Event carries the already-REPLAYED envelope, so the
	// second run's StampReplayed is a no-op.
	chained := make([]replay.Event, len(first))
	for i, m := range first {
		env, payload, err := bus.Unframe(m.Body)
		if err != nil {
			t.Fatalf("first-run message %d unframe: %v", i, err)
		}
		chained[i] = replay.Event{
			Envelope:  env,
			Payload:   payload,
			Key:       m.Key,
			Topic:     "market.equity", // not material for Pipeline
			Partition: 0,
			Offset:    int64(i),
			KafkaTime: fixedTime,
			Headers:   m.Headers,
		}
	}
	// The second run uses the same RunID; only the subject is
	// re-derived from envelope.EventType which the first run did not
	// rewrite — so the chained run's destination subject is the
	// same as the first's.
	second := runPipeline(t, runID, chained)

	if len(second) != len(first) {
		t.Fatalf("chained-run count diverged: first=%d chained=%d", len(first), len(second))
	}
	for i := range first {
		assertMessagesByteIdentical(t, i, first[i], second[i])
	}
}

// --- Envelope identity preservation -----------------------------------

// Replay must NOT mutate any envelope field besides QualityFlags. A
// regression that stamps per-run lineage or rewrites the source
// identifier would silently break downstream correlation logic and
// break replay's "same log → identical output" claim.
func TestDeterminism_OriginalEnvelopeIdentityPreserved(t *testing.T) {
	log := canonicalLog()
	originals := make([]*envelopepb.Envelope, len(log))
	for i, ev := range log {
		// proto.Clone to capture the pre-Pipeline state before
		// StampReplayed mutates the slice in place.
		originals[i] = proto.Clone(ev.Envelope).(*envelopepb.Envelope)
	}

	messages := runPipeline(t, "0190000000000000000000000000000a", log)

	for i, m := range messages {
		got, _, err := bus.Unframe(m.Body)
		if err != nil {
			t.Fatalf("message %d unframe: %v", i, err)
		}
		// Identity fields the contract pins.
		if got.EventId != originals[i].EventId {
			t.Errorf("message %d EventId mutated: got %q want %q", i, got.EventId, originals[i].EventId)
		}
		if got.IdempotencyKey != originals[i].IdempotencyKey {
			t.Errorf("message %d IdempotencyKey mutated: got %q want %q", i, got.IdempotencyKey, originals[i].IdempotencyKey)
		}
		if got.ProducerSequence != originals[i].ProducerSequence {
			t.Errorf("message %d ProducerSequence mutated: got %d want %d", i, got.ProducerSequence, originals[i].ProducerSequence)
		}
		if got.CorrelationId != originals[i].CorrelationId {
			t.Errorf("message %d CorrelationId mutated: got %q want %q", i, got.CorrelationId, originals[i].CorrelationId)
		}
		if got.CausationId != originals[i].CausationId {
			t.Errorf("message %d CausationId mutated: got %q want %q", i, got.CausationId, originals[i].CausationId)
		}
		if got.Source != originals[i].Source {
			t.Errorf("message %d Source mutated: got %q want %q", i, got.Source, originals[i].Source)
		}
		if got.ProducerVersion != originals[i].ProducerVersion {
			t.Errorf("message %d ProducerVersion mutated: got %q want %q", i, got.ProducerVersion, originals[i].ProducerVersion)
		}
		// Only QualityFlags is permitted to differ — exactly one
		// QUALITY_FLAG_REPLAYED added (idempotent), original flags
		// preserved in order.
		if !replay.IsReplayed(got) {
			t.Errorf("message %d missing REPLAYED flag after replay", i)
		}
		if len(got.QualityFlags) != len(originals[i].QualityFlags)+1 {
			t.Errorf("message %d QualityFlags length=%d want %d", i,
				len(got.QualityFlags), len(originals[i].QualityFlags)+1)
		}
	}
}

// --- Compatibility-during-replay link ---------------------------------

// The kanz-schemas/README.md § Schema Evolution §8 link: replay determinism
// depends on payload schema §2 compat holding. This test exercises the link
// directly — an envelope carrying an unknown field (simulating an event from a
// future publisher that landed in an old Kafka log) replays without dropping
// that field, because proto3 preserves unknown fields through unmarshal +
// re-marshal.
func TestDeterminism_UnknownEnvelopeFieldsSurviveReplay(t *testing.T) {
	const futureFieldNumber = protowire.Number(1000)

	// Build wire bytes for an envelope that carries a future field,
	// then unmarshal — that yields an *envelopepb.Envelope whose
	// unknown-fields slot holds the appended tag. Pipeline re-marshal
	// preserves it.
	base := canonicalEnvelope("evt-future", "market.equity.trade", "AAPL", 1)
	baseBytes, err := proto.Marshal(base)
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}
	withFuture := protowire.AppendTag(baseBytes, futureFieldNumber, protowire.VarintType)
	withFuture = protowire.AppendVarint(withFuture, 12345)

	var ev envelopepb.Envelope
	if err := proto.Unmarshal(withFuture, &ev); err != nil {
		t.Fatalf("unmarshal envelope with future field: %v", err)
	}
	if !containsTag(ev.ProtoReflect().GetUnknown(), futureFieldNumber) {
		t.Fatal("precondition failed: unknown field not preserved on initial unmarshal")
	}

	log := []replay.Event{{
		Envelope:  &ev,
		Payload:   []byte("p"),
		Key:       []byte("AAPL"),
		Topic:     "market.equity",
		Partition: 0,
		Offset:    1,
		KafkaTime: fixedTime,
		Headers:   map[string]string{"Nats-Msg-Id": "evt-future"},
	}}
	messages := runPipeline(t, "0190000000000000000000000000000c", log)
	if len(messages) != 1 {
		t.Fatalf("messages=%d want 1", len(messages))
	}

	out, _, err := bus.Unframe(messages[0].Body)
	if err != nil {
		t.Fatalf("output unframe: %v", err)
	}
	if !containsTag(out.ProtoReflect().GetUnknown(), futureFieldNumber) {
		t.Error("future envelope field dropped during replay — replay would silently corrupt forward-compat events")
	}
}

// --- Helpers -----------------------------------------------------------

func assertMessagesByteIdentical(t *testing.T, idx int, a, b bus.Message) {
	t.Helper()
	if a.Subject != b.Subject {
		t.Errorf("message %d subject diverged: %q vs %q", idx, a.Subject, b.Subject)
	}
	if !bytes.Equal(a.Key, b.Key) {
		t.Errorf("message %d key diverged: %q vs %q", idx, a.Key, b.Key)
	}
	if !bytes.Equal(a.Body, b.Body) {
		t.Errorf("message %d body diverged (%d vs %d bytes)", idx, len(a.Body), len(b.Body))
	}
	if !reflect.DeepEqual(a.Headers, b.Headers) {
		t.Errorf("message %d headers diverged: %v vs %v", idx, a.Headers, b.Headers)
	}
}

func containsTag(b []byte, want protowire.Number) bool {
	for len(b) > 0 {
		num, _, n := protowire.ConsumeField(b)
		if n < 0 {
			return false
		}
		if num == want {
			return true
		}
		b = b[n:]
	}
	return false
}
