package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

func TestNewRunIDFormat(t *testing.T) {
	id := NewRunID()
	s := string(id)
	if len(s) != 32 {
		t.Fatalf("len=%d want 32 (hex-encoded uuid)", len(s))
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("non-lowercase-hex char %q in run id %q", r, s)
		}
	}
}

func TestNewRunIDDistinctAndSortable(t *testing.T) {
	const n = 50
	ids := make([]string, n)
	for i := range ids {
		ids[i] = string(NewRunID())
	}
	seen := make(map[string]struct{}, n)
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = struct{}{}
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatal("run ids are not monotonically time-sortable")
		}
	}
}

func TestSubjectFor(t *testing.T) {
	cases := []struct {
		name     string
		runID    RunID
		original string
		want     string
		wantErr  string
	}{
		{"happy path", "abc", "market.equity.trade", "replay.abc.market.equity.trade", ""},
		{"empty runID", "", "x", "", "runID is empty"},
		{"empty original", "abc", "", "", "original subject is empty"},
		{"already replay", "abc", "replay.def.x.y.z", "", "cannot wrap a replay-namespaced subject"},
		{"dlq prefix", "abc", "dlq.market.equity", "", "cannot wrap a DLQ subject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SubjectFor(tc.runID, tc.original)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestConsumerGroup(t *testing.T) {
	got, err := ConsumerGroup("abc")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "replay-abc" {
		t.Fatalf("got %q want replay-abc", got)
	}
	if _, err := ConsumerGroup(""); err == nil {
		t.Fatal("expected error for empty runID")
	}
}

// fakeSource yields a scripted sequence of (Event, error) tuples. Threadsafe
// for the Pipeline's single-goroutine consumption pattern.
type fakeSource struct {
	steps []sourceStep
	i     int
}

type sourceStep struct {
	ev  Event
	err error
}

func (f *fakeSource) Next(ctx context.Context) (Event, error) {
	if f.i >= len(f.steps) {
		return Event{}, io.EOF
	}
	s := f.steps[f.i]
	f.i++
	return s.ev, s.err
}

// captureClient is a bus.Publisher that records every Publish call.
type captureClient struct {
	mu       sync.Mutex
	messages []bus.Message
	failOn   int   // 1-indexed: fail the Nth Publish
	failErr  error // returned on the failing call
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, m)
	if c.failOn > 0 && len(c.messages) == c.failOn {
		return c.failErr
	}
	return nil
}

func envFact(eventType, payload string) (*envelopepb.Envelope, []byte) {
	return &envelopepb.Envelope{
		EventId:          "id-" + eventType,
		EventType:        eventType,
		SchemaVersion:    1,
		EnvelopeVersion:  1,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		Domain:           strings.SplitN(eventType, ".", 2)[0],
		EventTime:        timestamppb.Now(),
		IngestionTime:    timestamppb.Now(),
		PublishTime:      timestamppb.Now(),
		CorrelationId:    "id-" + eventType,
		Source:           "test",
		ProducerVersion:  "t",
		IdempotencyKey:   "id-" + eventType,
		PayloadSchemaRef: "test:1",
	}, []byte(payload)
}

func TestPipelineHappyPath(t *testing.T) {
	env1, p1 := envFact("market.equity.trade", "p1")
	env2, p2 := envFact("market.equity.quote", "p2")
	src := &fakeSource{steps: []sourceStep{
		{ev: Event{Envelope: env1, Payload: p1, Key: []byte("AAPL"), Topic: "market.equity", Partition: 0, Offset: 100, Headers: map[string]string{"Nats-Msg-Id": "id-market.equity.trade"}}},
		{ev: Event{Envelope: env2, Payload: p2, Key: []byte("AAPL"), Topic: "market.equity", Partition: 0, Offset: 101}},
	}}
	pub := &captureClient{}
	pl := &Pipeline{RunID: "RUN1", Source: src, Publisher: pub}

	stats, err := pl.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Published != 2 || stats.Malformed != 0 {
		t.Fatalf("stats=%+v want Published=2 Malformed=0", stats)
	}
	if len(pub.messages) != 2 {
		t.Fatalf("messages=%d want 2", len(pub.messages))
	}

	want := []string{
		"replay.RUN1.market.equity.trade",
		"replay.RUN1.market.equity.quote",
	}
	for i, m := range pub.messages {
		if m.Subject != want[i] {
			t.Errorf("message %d subject=%q want %q", i, m.Subject, want[i])
		}
		if string(m.Key) != "AAPL" {
			t.Errorf("message %d key=%q want AAPL", i, m.Key)
		}
		// Re-unframe and check envelope round-trip.
		env, payload, err := bus.Unframe(m.Body)
		if err != nil {
			t.Fatalf("message %d unframe: %v", i, err)
		}
		if env.EventType != strings.TrimPrefix(want[i], "replay.RUN1.") {
			t.Errorf("message %d envelope.EventType=%q", i, env.EventType)
		}
		if !proto.Equal(env, []*envelopepb.Envelope{env1, env2}[i]) {
			t.Errorf("message %d envelope drift after re-marshal", i)
		}
		if string(payload) != []string{"p1", "p2"}[i] {
			t.Errorf("message %d payload=%q", i, payload)
		}
	}
	// Nats-Msg-Id propagates so destination dedup works.
	if pub.messages[0].Headers["Nats-Msg-Id"] != "id-market.equity.trade" {
		t.Errorf("Nats-Msg-Id header dropped: %v", pub.messages[0].Headers)
	}
	// EVT-20c: every replayed event must carry QUALITY_FLAG_REPLAYED.
	for i, m := range pub.messages {
		env, _, err := bus.Unframe(m.Body)
		if err != nil {
			t.Fatalf("message %d unframe: %v", i, err)
		}
		if !IsReplayed(env) {
			t.Errorf("message %d missing REPLAYED flag: %v", i, env.QualityFlags)
		}
	}
}

func TestPipelineStampingIsIdempotent(t *testing.T) {
	// Source events already flagged (e.g. replay-of-a-replay): Pipeline must
	// not double-stamp, mirroring StampReplayed's idempotency.
	env, payload := envFact("market.equity.trade", "p")
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	src := &fakeSource{steps: []sourceStep{
		{ev: Event{Envelope: env, Payload: payload, Offset: 1}},
	}}
	pub := &captureClient{}
	pl := &Pipeline{RunID: "R", Source: src, Publisher: pub}

	if _, err := pl.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out, _, err := bus.Unframe(pub.messages[0].Body)
	if err != nil {
		t.Fatalf("unframe: %v", err)
	}
	flags := 0
	for _, qf := range out.QualityFlags {
		if qf == envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED {
			flags++
		}
	}
	if flags != 1 {
		t.Errorf("REPLAYED flag count=%d want 1, all=%v", flags, out.QualityFlags)
	}
}

func TestPipelineSkipsMalformedFrame(t *testing.T) {
	env, payload := envFact("market.equity.trade", "p")
	mfe := &MalformedFrameError{Topic: "market.equity", Partition: 1, Offset: 42, Err: errors.New("bad")}
	src := &fakeSource{steps: []sourceStep{
		{ev: Event{Topic: "market.equity", Partition: 1, Offset: 42}, err: mfe},
		{ev: Event{Envelope: env, Payload: payload, Key: []byte("k"), Topic: "market.equity", Partition: 0, Offset: 1}},
	}}
	pub := &captureClient{}
	pl := &Pipeline{RunID: "R", Source: src, Publisher: pub}

	stats, err := pl.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Published != 1 || stats.Malformed != 1 {
		t.Fatalf("stats=%+v want Published=1 Malformed=1", stats)
	}
	if len(pub.messages) != 1 {
		t.Fatalf("messages=%d want 1", len(pub.messages))
	}
}

func TestPipelineFatalSourceErrorAborts(t *testing.T) {
	fatal := errors.New("kafka exploded")
	src := &fakeSource{steps: []sourceStep{
		{ev: Event{Topic: "x", Partition: 0}, err: fatal},
	}}
	pub := &captureClient{}
	pl := &Pipeline{RunID: "R", Source: src, Publisher: pub}

	stats, err := pl.Run(context.Background())
	if err == nil || !errors.Is(err, fatal) {
		t.Fatalf("err=%v want wrapping %v", err, fatal)
	}
	if stats.Published != 0 || stats.Malformed != 0 {
		t.Fatalf("stats=%+v want zero", stats)
	}
	if len(pub.messages) != 0 {
		t.Fatalf("messages=%d want 0", len(pub.messages))
	}
}

func TestPipelinePublishErrorAborts(t *testing.T) {
	env1, p1 := envFact("market.equity.trade", "p1")
	env2, p2 := envFact("market.equity.quote", "p2")
	env3, p3 := envFact("market.equity.bar", "p3")
	src := &fakeSource{steps: []sourceStep{
		{ev: Event{Envelope: env1, Payload: p1, Offset: 1}},
		{ev: Event{Envelope: env2, Payload: p2, Offset: 2}},
		{ev: Event{Envelope: env3, Payload: p3, Offset: 3}},
	}}
	boom := errors.New("broker down")
	pub := &captureClient{failOn: 2, failErr: boom}
	pl := &Pipeline{RunID: "R", Source: src, Publisher: pub}

	stats, err := pl.Run(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("err=%v want wrapping %v", err, boom)
	}
	if stats.Published != 1 {
		t.Fatalf("stats.Published=%d want 1", stats.Published)
	}
	// 2 attempts: the first succeeded, the second hit captureClient.failOn.
	if len(pub.messages) != 2 {
		t.Fatalf("messages=%d want 2", len(pub.messages))
	}
}

func TestPipelineFieldValidation(t *testing.T) {
	src := &fakeSource{}
	pub := &captureClient{}
	cases := []struct {
		name string
		pl   *Pipeline
		want string
	}{
		{"empty runID", &Pipeline{Source: src, Publisher: pub}, "RunID is empty"},
		{"nil source", &Pipeline{RunID: "R", Publisher: pub}, "Source is nil"},
		{"nil publisher", &Pipeline{RunID: "R", Source: src}, "Publisher is nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.pl.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want substring %q", err, tc.want)
			}
		})
	}
}

func TestMalformedFrameErrorIsRecognizedThroughWrap(t *testing.T) {
	// Defense-in-depth: Pipeline uses errors.As, so a wrapped
	// MalformedFrameError still routes to the skip-and-continue path.
	wrapped := fmt.Errorf("outer: %w", &MalformedFrameError{Topic: "x", Offset: 0, Err: errors.New("e")})
	env, payload := envFact("market.equity.trade", "p")
	src := &fakeSource{steps: []sourceStep{
		{ev: Event{Topic: "x", Offset: 0}, err: wrapped},
		{ev: Event{Envelope: env, Payload: payload, Offset: 1}},
	}}
	pub := &captureClient{}
	pl := &Pipeline{RunID: "R", Source: src, Publisher: pub}
	stats, err := pl.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Malformed != 1 || stats.Published != 1 {
		t.Fatalf("stats=%+v want Malformed=1 Published=1", stats)
	}
}
