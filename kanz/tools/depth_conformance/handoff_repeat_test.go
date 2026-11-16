package main

import (
	"bytes"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type handoffObservation struct {
	deadline time.Time
	count    int
}

func (o *handoffObservation) take(now time.Time) bool {
	if !now.Before(o.deadline) || o.count >= 10000 {
		return false
	}
	o.count++
	return true
}

// Only the immediately preceding fully validated Rust payload is repeatable.
// Envelope publication identity may change; venue time and payload may not.
type handoffPrevious struct {
	frame    *envelopepb.EventFrame
	sequence uint64
	producer uint64
}

func (p *handoffPrevious) classify(f *envelopepb.EventFrame, actual, target uint64, now, deadline time.Time) (bool, error) {
	e := f.GetEnvelope()
	if !now.Before(deadline) || e == nil || e.ProducerSequence == 0 || e.ProducerSequence <= p.producer {
		return false, errors.New("observation order or deadline refused")
	}
	if actual != target {
		if p.frame == nil || actual != p.sequence || !bytes.Equal(f.Payload, p.frame.Payload) || !proto.Equal(e.EventTime, p.frame.GetEnvelope().EventTime) {
			return false, errors.New("non-immediate or altered repeat refused")
		}
		p.producer = e.ProducerSequence
		return true, nil
	}
	p.producer = e.ProducerSequence
	return false, nil
}

func (p *handoffPrevious) remember(f *envelopepb.EventFrame, sequence uint64) {
	p.frame = proto.Clone(f).(*envelopepb.EventFrame)
	p.sequence = sequence
}

func TestFeedHandoffExactRepeatControl(t *testing.T) {
	now := time.Unix(100, 0)
	deadline := now.Add(10 * time.Second)
	frame := func(payload string, producer uint64) *envelopepb.EventFrame {
		return &envelopepb.EventFrame{Payload: []byte(payload), Envelope: &envelopepb.Envelope{ProducerSequence: producer, EventTime: timestamppb.New(now)}}
	}
	previous := frame("validated-price50000-size4", 1)
	fresh := func() handoffPrevious {
		return handoffPrevious{frame: proto.Clone(previous).(*envelopepb.EventFrame), sequence: 101, producer: 1}
	}
	p := fresh()
	if repeat, err := p.classify(frame(string(previous.Payload), 2), 101, 102, now, deadline); err != nil || !repeat {
		t.Fatal("exact repeat refused")
	}
	target := frame("validated-next-state", 3)
	if repeat, err := p.classify(target, 102, 102, now, deadline); err != nil || repeat {
		t.Fatal("new target refused")
	}
	p.remember(target, 102)
	if _, err := p.classify(frame(string(previous.Payload), 4), 101, 105, now, deadline); err == nil {
		t.Fatal("non-immediate history accepted")
	}
	for _, mutate := range []func(*envelopepb.EventFrame){
		func(f *envelopepb.EventFrame) { f.Payload = []byte("validated-price1-size4") },
		func(f *envelopepb.EventFrame) { f.Payload = append(f.Payload, 0) },
		func(f *envelopepb.EventFrame) { f.Envelope.EventTime = timestamppb.New(now.Add(time.Nanosecond)) },
		func(f *envelopepb.EventFrame) { f.Envelope.ProducerSequence = 1 },
		func(f *envelopepb.EventFrame) { f.Envelope.ProducerSequence = 0 },
	} {
		p = fresh()
		f := frame(string(previous.Payload), 2)
		mutate(f)
		if _, err := p.classify(f, 101, 102, now, deadline); err == nil {
			t.Fatal("altered or regressing repeat accepted")
		}
	}
	p = fresh()
	p.producer = 3
	if _, err := p.classify(frame(string(previous.Payload), 2), 101, 102, now, deadline); err == nil {
		t.Fatal("positive regressing producer sequence accepted")
	}
	empty := handoffPrevious{}
	if _, err := empty.classify(previous, 101, 102, now, deadline); err == nil {
		t.Fatal("unproved initial repeat accepted")
	}
	p = fresh()
	if _, err := p.classify(target, 102, 102, deadline, deadline); err == nil {
		t.Fatal("ready target accepted at deadline")
	}
	budget := handoffObservation{deadline: deadline}
	for i := 0; i < 10000; i++ {
		if !budget.take(now) {
			t.Fatal("bounded inventory refused early")
		}
	}
	if budget.take(now) {
		t.Fatal("inventory overflow accepted")
	}
	budget = handoffObservation{deadline: deadline}
	if !budget.take(now) || budget.take(deadline) || !budget.deadline.Equal(deadline) {
		t.Fatal("observation deadline reset or ignored")
	}
}
