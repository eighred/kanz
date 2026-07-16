package cdc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	commonv1 "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/lake-sink/internal/cdc"
	"github.com/kanz-eng/kanz/services/lake-sink/internal/decode"
	"github.com/kanz-eng/kanz/services/lake-sink/internal/sink"
)

type fakeSink struct {
	rows     []sink.Row
	err      error
	flushErr error

	flushed int      // number of Flush calls
	calls   []string // "write"/"flush" in invocation order, for ordering assertions
}

func (f *fakeSink) Write(_ context.Context, r sink.Row) error {
	f.calls = append(f.calls, "write")
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, r)
	return nil
}
func (f *fakeSink) Flush() error {
	f.calls = append(f.calls, "flush")
	f.flushed++
	return f.flushErr
}
func (f *fakeSink) Close() error { return nil }

type fakeResolver struct {
	md  protoreflect.MessageDescriptor
	err error
}

func (r fakeResolver) Resolve(context.Context, string) (protoreflect.MessageDescriptor, error) {
	return r.md, r.err
}

func newSink(res decode.Resolver, fs *fakeSink) *cdc.EventSink {
	at := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	return cdc.NewEventSink(decode.NewDecoder(res), fs, func() time.Time { return at }, nil, nil)
}

func env(ref string) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventId:          "e1",
		Domain:           "common",
		EventType:        "decimal.recorded",
		PayloadSchemaRef: ref,
		EventTime:        timestamppb.New(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)),
	}
}

func TestHandleDecodesAndLands(t *testing.T) {
	md := (&commonv1.Decimal{}).ProtoReflect().Descriptor()
	fs := &fakeSink{}
	es := newSink(fakeResolver{md: md}, fs)

	payload, _ := proto.Marshal(&commonv1.Decimal{Coefficient: 7, Exponent: -1})
	if err := es.Handle(context.Background(), env("common.v1.Decimal:3"), payload); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(fs.rows) != 1 {
		t.Fatalf("landed %d rows, want 1", len(fs.rows))
	}
	r := fs.rows[0]
	if r.Entity != "Decimal" {
		t.Errorf("entity = %q, want Decimal (from ref message name)", r.Entity)
	}
	if r.Payload == nil || r.DecodeError != "" {
		t.Errorf("want decoded payload and no error, got payload=%s err=%q", r.Payload, r.DecodeError)
	}
	if fs.flushed != 1 {
		t.Errorf("flushed %d times, want exactly 1 — Handle must not ack (nil) until the row is durable", fs.flushed)
	}
}

func TestHandlePermanentDecodeLandsEnvelopeOnlyAndAcks(t *testing.T) {
	fs := &fakeSink{}
	es := newSink(fakeResolver{err: errors.New("unknown ref")}, fs)

	// Permanent decode failure must ack (nil) — a poison message can't block the
	// partition — and still land the row so the lake records the event occurred.
	if err := es.Handle(context.Background(), env("bad.v1.Nope:1"), []byte{1, 2}); err != nil {
		t.Fatalf("want ack (nil) on permanent decode error, got %v", err)
	}
	if len(fs.rows) != 1 {
		t.Fatalf("landed %d rows, want 1", len(fs.rows))
	}
	r := fs.rows[0]
	if r.Payload != nil || r.DecodeError == "" {
		t.Errorf("want envelope-only with DecodeError set, got payload=%s err=%q", r.Payload, r.DecodeError)
	}
	// A permanent-decode-error row still lands and still acks — the bus still
	// commits its offset — so it must be durable too, exactly like the ok path.
	if fs.flushed != 1 {
		t.Errorf("flushed %d times on the permanent-decode-error path, want exactly 1", fs.flushed)
	}
}

func TestHandleTransientDecodeNaks(t *testing.T) {
	fs := &fakeSink{}
	es := newSink(fakeResolver{err: &decode.TransientError{Err: errors.New("registry down")}}, fs)

	// Transient failure must return the error (NAK → redelivery) and land nothing,
	// so the decoded payload isn't lost.
	err := es.Handle(context.Background(), env("common.v1.Decimal:1"), []byte{1, 2})
	if err == nil {
		t.Fatal("want error (NAK) on transient decode failure, got nil")
	}
	if len(fs.rows) != 0 {
		t.Errorf("landed %d rows on transient failure, want 0", len(fs.rows))
	}
	if fs.flushed != 0 {
		t.Errorf("flushed %d times on transient decode failure (no row written), want 0", fs.flushed)
	}
}

func TestHandleWriteErrorRetries(t *testing.T) {
	fs := &fakeSink{err: errors.New("disk full")}
	es := newSink(fakeResolver{md: (&commonv1.Decimal{}).ProtoReflect().Descriptor()}, fs)
	payload, _ := proto.Marshal(&commonv1.Decimal{})
	if err := es.Handle(context.Background(), env("common.v1.Decimal:1"), payload); err == nil {
		t.Fatal("want error so the bus retries a failed sink write, got nil")
	}
	if fs.flushed != 0 {
		t.Errorf("flushed %d times after a failed write, want 0 — nothing landed, nothing to flush", fs.flushed)
	}
}

// TestHandleFlushErrorRetries is the crash-defense unit test: a flush failure
// (the fsync itself failing — disk full, I/O error, ENOSPC) must be treated
// exactly like a write failure. The row was accepted into the buffer but
// never reached the fd, so Handle must return the error — never nil — or the
// bus commits the offset for a row that still only exists in memory.
func TestHandleFlushErrorRetries(t *testing.T) {
	fs := &fakeSink{flushErr: errors.New("fsync failed")}
	es := newSink(fakeResolver{md: (&commonv1.Decimal{}).ProtoReflect().Descriptor()}, fs)
	payload, _ := proto.Marshal(&commonv1.Decimal{})
	if err := es.Handle(context.Background(), env("common.v1.Decimal:1"), payload); err == nil {
		t.Fatal("want error so the bus does not commit when Flush fails, got nil")
	}
	if fs.flushed != 1 {
		t.Errorf("flush called %d times, want exactly 1", fs.flushed)
	}
}

// TestHandleFlushesAfterWrite pins the ordering the whole fix depends on:
// Flush must happen strictly after Write, on every call to Handle. Flushing
// before the row is written would fsync nothing useful; flushing out of
// order would be indistinguishable from not flushing at all under a
// buggy-but-passing fake.
func TestHandleFlushesAfterWrite(t *testing.T) {
	fs := &fakeSink{}
	es := newSink(fakeResolver{md: (&commonv1.Decimal{}).ProtoReflect().Descriptor()}, fs)
	payload, _ := proto.Marshal(&commonv1.Decimal{})
	if err := es.Handle(context.Background(), env("common.v1.Decimal:1"), payload); err != nil {
		t.Fatalf("handle: %v", err)
	}
	want := []string{"write", "flush"}
	if len(fs.calls) != len(want) {
		t.Fatalf("call sequence = %v, want %v", fs.calls, want)
	}
	for i := range want {
		if fs.calls[i] != want[i] {
			t.Fatalf("call sequence = %v, want %v", fs.calls, want)
		}
	}
}
