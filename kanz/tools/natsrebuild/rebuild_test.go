package natsrebuild

import (
	"context"
	"io"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/tools/replay"
)

// sliceSource yields a fixed list of events then io.EOF — a replay.Source stub.
type sliceSource struct {
	events []replay.Event
	i      int
}

func (s *sliceSource) Next(context.Context) (replay.Event, error) {
	if s.i >= len(s.events) {
		return replay.Event{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}

// capturePublisher records every published message.
type capturePublisher struct{ msgs []bus.Message }

func (c *capturePublisher) Publish(_ context.Context, m bus.Message) error {
	c.msgs = append(c.msgs, m)
	return nil
}

func ev(eventType string) replay.Event {
	return replay.Event{
		Envelope: &envelopepb.Envelope{EventId: "id-" + eventType, EventType: eventType},
		Payload:  []byte("p-" + eventType),
		Key:      []byte("k"),
		Headers:  map[string]string{"Nats-Msg-Id": "id-" + eventType},
	}
}

func TestRebuildPublishesToLiveSubjectUnflagged(t *testing.T) {
	src := &sliceSource{events: []replay.Event{ev("risk.portfolio.recomputed"), ev("market.equity.trade")}}
	pub := &capturePublisher{}
	p := &Pipeline{Source: src, Publisher: pub}

	stats, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Published != 2 {
		t.Fatalf("published=%d want 2", stats.Published)
	}
	if pub.msgs[0].Subject != "risk.portfolio.recomputed" || pub.msgs[1].Subject != "market.equity.trade" {
		t.Fatalf("wrong subjects: %q, %q", pub.msgs[0].Subject, pub.msgs[1].Subject)
	}
	// Headers (Nats-Msg-Id for dedup) and key must propagate.
	if pub.msgs[0].Headers["Nats-Msg-Id"] != "id-risk.portfolio.recomputed" || string(pub.msgs[0].Key) != "k" {
		t.Fatalf("headers/key not propagated: %+v", pub.msgs[0])
	}
	// The published frame must carry the UNCHANGED envelope — no REPLAYED flag.
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(pub.msgs[0].Body, &frame); err != nil {
		t.Fatal(err)
	}
	for _, f := range frame.GetEnvelope().GetQualityFlags() {
		if f == envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED {
			t.Fatal("rebuild stamped REPLAYED — live consumers would reject the rebuilt spine")
		}
	}
	if frame.GetEnvelope().GetEventId() != "id-risk.portfolio.recomputed" {
		t.Fatalf("envelope mutated: %q", frame.GetEnvelope().GetEventId())
	}
}

func TestRebuildEmptyEventTypeIsFatal(t *testing.T) {
	src := &sliceSource{events: []replay.Event{{Envelope: &envelopepb.Envelope{EventId: "x"}, Payload: nil}}}
	p := &Pipeline{Source: src, Publisher: &capturePublisher{}}
	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("expected fatal error on empty event_type")
	}
}

func TestRebuildNilGuards(t *testing.T) {
	if _, err := (&Pipeline{Publisher: &capturePublisher{}}).Run(context.Background()); err == nil {
		t.Fatal("nil Source should error")
	}
	if _, err := (&Pipeline{Source: &sliceSource{}}).Run(context.Background()); err == nil {
		t.Fatal("nil Publisher should error")
	}
}
