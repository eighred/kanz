// Package compatibility holds the runtime wire-level compatibility
// contract tests for EVT-21c. They prove the proto3 invariants the
// envelope-policy / schema-evolution docs depend on actually hold for
// the live bindings:
//
//   - Backward compatibility (current code reads older bytes): wire-zero
//     omits fields; an envelope marshaled with only its required fields
//     unmarshals cleanly into the current schema with optional fields at
//     their zero value.
//   - Forward compatibility (older code reads newer bytes): an unknown
//     field appended to envelope wire bytes is preserved through
//     unmarshal + re-marshal via proto3's unknown-fields mechanism, and
//     does not affect Validate.
//
// These complement two static gates that cannot prove runtime behavior:
//   - `buf breaking` (EVT-07) blocks wire-breaking schema edits at PR
//     time but says nothing about live decoder behavior.
//   - CODEOWNERS review on `proto/{domain}/` (schema-evolution §5)
//     catches semantically-breaking-but-wire-compatible changes that
//     `buf breaking` cannot see.
//
// EVT-21c is the third leg: a runtime contract test that asserts the
// wire-runtime behavior matches what the docs promise — so a regression
// in the bindings (e.g. a generator that drops unknown fields) fails the
// build instead of silently corrupting replay and rolling deployments.
package compatibility_test

import (
	"bytes"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// canonicalEnvelope is the same shape EVT-21a / EVT-21b use, lifted
// inline so the compatibility tests do not depend on the serialization
// fixture catalog.
func canonicalEnvelope() *envelopepb.Envelope {
	ts := timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	return &envelopepb.Envelope{
		EventId:          "evt-compat-1",
		EventType:        "market.equity.trade",
		SchemaVersion:    1,
		EnvelopeVersion:  1,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		Domain:           "market",
		EventTime:        ts,
		IngestionTime:    ts,
		PublishTime:      ts,
		CorrelationId:    "evt-compat-1",
		Source:           "compat-test/inst-1",
		ProducerVersion:  "compat-1.0.0",
		PartitionKey:     "AAPL",
		ProducerSequence: 1,
		IdempotencyKey:   "evt-compat-1",
		PayloadSchemaRef: "market.v1.MarketDataEvent:1",
	}
}

// unknownFieldNumber is well above every currently-allocated Envelope
// field number (1–19), so it stands in for a hypothetical future field
// added under envelope-policy §6's additive-only rule. Using 1000 (not
// a value inside the 19000–19999 reserved range) keeps the synthetic
// field a plausible real next-allocation candidate.
const unknownFieldNumber = protowire.Number(1000)

// appendUnknownVarintField appends a proto3 wire-format varint field
// (tag + value) to existing wire bytes. The resulting buffer is a valid
// proto3 message because field ordering is not significant.
func appendUnknownVarintField(b []byte, num protowire.Number, value uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	b = protowire.AppendVarint(b, value)
	return b
}

// containsTag scans wire bytes for any field carrying the given tag
// number. Used to assert unknown-field preservation across re-marshal.
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

// --- Forward compatibility ---------------------------------------------

// Older code reading newer bytes — the core forward-compat claim. An
// envelope authored by a hypothetical future publisher with an extra
// field MUST unmarshal cleanly via current bindings and the unknown
// bytes MUST be preserved (so replay re-marshaling does not strip
// fields that downstream tooling might care about).
func TestForwardCompat_UnknownEnvelopeFieldPreservedAcrossRoundTrip(t *testing.T) {
	body, err := proto.Marshal(canonicalEnvelope())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	withUnknown := appendUnknownVarintField(body, unknownFieldNumber, 42)

	var got envelopepb.Envelope
	if err := proto.Unmarshal(withUnknown, &got); err != nil {
		t.Fatalf("unmarshal envelope with unknown field: %v", err)
	}

	unknown := got.ProtoReflect().GetUnknown()
	if !containsTag(unknown, unknownFieldNumber) {
		t.Fatalf("unknown field %d not preserved on unmarshal; GetUnknown()=%x", unknownFieldNumber, unknown)
	}

	rebuilt, err := proto.Marshal(&got)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !containsTag(rebuilt, unknownFieldNumber) {
		t.Errorf("unknown field %d dropped on re-marshal", unknownFieldNumber)
	}
}

// Validate enforces envelope-policy invariants. It MUST stay
// indifferent to unknown fields — otherwise a future publisher's new
// field would synthetically fail the current consumer's validator,
// breaking forward compatibility.
func TestForwardCompat_UnknownEnvelopeFieldDoesNotAffectValidate(t *testing.T) {
	body, _ := proto.Marshal(canonicalEnvelope())
	body = appendUnknownVarintField(body, unknownFieldNumber, 1)

	var got envelopepb.Envelope
	if err := proto.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := bus.Validate(&got); err != nil {
		t.Errorf("Validate rejected envelope with unknown field: %v", err)
	}
}

// The same forward-compat property must hold at the EventFrame layer:
// an unknown field appended to the frame wire bytes must not break
// bus.Unframe. Replay (EVT-20) re-marshals the frame from Envelope +
// payload, so EventFrame-level unknown fields are dropped on the way
// out — the contract here is only "Unframe doesn't reject", not
// "unknown EventFrame fields survive a replay re-publish."
func TestForwardCompat_UnknownEventFrameFieldDoesNotBreakUnframe(t *testing.T) {
	frame := &envelopepb.EventFrame{
		Envelope: canonicalEnvelope(),
		Payload:  []byte("payload"),
	}
	body, err := proto.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	body = appendUnknownVarintField(body, unknownFieldNumber, 7)

	env, payload, err := bus.Unframe(body)
	if err != nil {
		t.Fatalf("Unframe frame with unknown field: %v", err)
	}
	if !bytes.Equal(payload, []byte("payload")) {
		t.Errorf("payload changed: got %q", payload)
	}
	if env.EventId != "evt-compat-1" {
		t.Errorf("envelope dropped during Unframe: EventId=%q", env.EventId)
	}
}

// --- Backward compatibility -------------------------------------------

// Current code reading older bytes — the core backward-compat claim.
// In proto3, zero-valued scalar fields are wire-absent: an envelope
// from a v0 producer that did not yet have causation_id / trace_context
// / quality_flags is wire-indistinguishable from a current envelope
// where those fields are unset. The contract: unmarshal succeeds and
// the optional fields read as their zero value.
func TestBackwardCompat_OptionalFieldsDefaultToZeroWhenAbsent(t *testing.T) {
	env := canonicalEnvelope()
	env.CausationId = ""
	env.TraceContext = ""
	env.QualityFlags = nil

	body, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got envelopepb.Envelope
	if err := proto.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.CausationId != "" {
		t.Errorf("CausationId=%q want empty (proto3 wire-zero)", got.CausationId)
	}
	if got.TraceContext != "" {
		t.Errorf("TraceContext=%q want empty", got.TraceContext)
	}
	if len(got.QualityFlags) != 0 {
		t.Errorf("QualityFlags=%v want empty", got.QualityFlags)
	}
}

// Zero-valued fields produce IDENTICAL wire bytes whether set
// explicitly or left default. This is the property that makes backward
// compat free in proto3: a v1 producer that never sets a field, and a
// v2 producer that sets the field but leaves it at the zero value,
// emit byte-identical events.
func TestBackwardCompat_WireZeroAndUnsetProduceIdenticalBytes(t *testing.T) {
	unset := canonicalEnvelope()
	unset.CausationId = "" // unset (zero string)
	unset.TraceContext = ""

	explicit := canonicalEnvelope()
	explicit.CausationId = "" // explicitly set to zero
	explicit.TraceContext = ""

	a, _ := proto.MarshalOptions{Deterministic: true}.Marshal(unset)
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(explicit)
	if !bytes.Equal(a, b) {
		t.Errorf("wire bytes differ between unset and explicit-zero envelopes\n  unset:    %x\n  explicit: %x", a, b)
	}
}

// --- Payload opacity + payload forward-compat --------------------------

// EventFrame.payload is `bytes` — totally opaque to envelope tooling.
// The contract: ANY byte string round-trips identically, regardless of
// whether it parses as a known proto, an unknown proto, or random
// garbage. This is what lets the registry / observability / replay
// stack remain payload-blind.
func TestPayloadOpacity_ArbitraryBytesRoundTripIdentically(t *testing.T) {
	payloads := [][]byte{
		nil,                                   // absent
		{},                                    // empty
		[]byte("plain-string"),                // ASCII
		{0x00, 0xff, 0x7f, 0x80, 0x01},        // arbitrary bytes
		bytesRepeating(0xab, 1024),            // larger blob
		mustMarshal(t, timestamppb.Now()),     // parseable as Timestamp
		appendUnknownVarintField(mustMarshal(t, timestamppb.Now()), 99, 5), // Timestamp + unknown
	}
	for i, want := range payloads {
		frame := &envelopepb.EventFrame{
			Envelope: canonicalEnvelope(),
			Payload:  want,
		}
		body, err := proto.Marshal(frame)
		if err != nil {
			t.Fatalf("payload[%d] marshal: %v", i, err)
		}
		_, got, err := bus.Unframe(body)
		if err != nil {
			t.Fatalf("payload[%d] unframe: %v", i, err)
		}
		// proto3 wire-omits zero-length bytes fields. Treat nil and []byte{}
		// as equivalent on the receive side — both decode to nil.
		if len(want) == 0 && len(got) == 0 {
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("payload[%d] mismatch: got %x want %x", i, got, want)
		}
	}
}

// Payload-side forward compat using Timestamp as a stand-in domain
// schema: simulate a v2 producer by appending an unknown field to a
// Timestamp's wire bytes. A current consumer that decodes the payload
// as Timestamp MUST unmarshal cleanly and preserve the unknown field
// across re-marshal — the same proto3 invariant the envelope relies
// on, demonstrated for an arbitrary payload schema.
func TestPayloadForwardCompat_UnknownFieldsPreservedInDecodedPayload(t *testing.T) {
	v1 := timestamppb.New(time.Unix(1767225600, 0))
	v1Body, err := proto.Marshal(v1)
	if err != nil {
		t.Fatalf("marshal v1: %v", err)
	}
	const futurePayloadField = protowire.Number(99)
	v2Body := appendUnknownVarintField(v1Body, futurePayloadField, 12345)

	var v2 timestamppb.Timestamp
	if err := proto.Unmarshal(v2Body, &v2); err != nil {
		t.Fatalf("unmarshal v2 payload: %v", err)
	}
	if v2.Seconds != 1767225600 {
		t.Errorf("known field corrupted: Seconds=%d", v2.Seconds)
	}
	if !containsTag(v2.ProtoReflect().GetUnknown(), futurePayloadField) {
		t.Error("unknown payload field dropped on unmarshal")
	}
	rebuilt, err := proto.Marshal(&v2)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !containsTag(rebuilt, futurePayloadField) {
		t.Error("unknown payload field dropped on re-marshal")
	}
}

// Payload-side backward compat: a v1 payload (fewer fields) decodes
// cleanly into a current decoder; absent fields default to zero.
// Timestamp stands in for any payload schema with optional fields.
func TestPayloadBackwardCompat_AbsentFieldsDefaultToZero(t *testing.T) {
	// Construct a Timestamp wire body with only `seconds` set (omit
	// `nanos`). This is what a v0 producer that did not yet have a
	// `nanos` field would have emitted.
	body := protowire.AppendTag(nil, 1, protowire.VarintType)
	body = protowire.AppendVarint(body, 1767225600)

	var got timestamppb.Timestamp
	if err := proto.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal v0 payload: %v", err)
	}
	if got.Seconds != 1767225600 {
		t.Errorf("Seconds=%d want 1767225600", got.Seconds)
	}
	if got.Nanos != 0 {
		t.Errorf("Nanos=%d want 0 (absent in v0 wire bytes)", got.Nanos)
	}
}

func bytesRepeating(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	body, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}
