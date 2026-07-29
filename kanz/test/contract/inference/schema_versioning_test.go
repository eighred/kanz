// Package inference holds the schema-versioning contract tests for the
// inference payloads (PRED-12). They prove the runtime invariants the
// prediction layer's versioning + cross-path design depend on, for the
// two payload schemas FeatureVector (PRED-01 input) and
// PredictionEnvelope (PRED-01 output):
//
//   - Version-identifier conventions: FeatureVector.feature_set_ref is
//     "{name}:{version}" and PredictionEnvelope.model is "{name}@{version}".
//     These string-encoded versions are the routing keys (PRED-03/04
//     feature→model routing, PRED-09 registry lookup), so the separators
//     and round-trip stability are a contract, not a formatting detail.
//   - Forward compatibility (older code reads newer bytes): a field a
//     future quant-ml schema bump adds survives unmarshal + re-marshal,
//     so a FeatureVector logged today can be replayed by tomorrow's
//     schema and vice versa without field loss.
//   - Backward compatibility (current code reads older bytes): proto3
//     wire-omits zero-valued fields, so a v0 producer's payload decodes
//     cleanly with new optional fields at their zero value.
//   - oneof + open-enum semantics: FeatureValue keeps categorical
//     distinct from scalar on the wire (a class id can never be read as
//     a magnitude), and a future PredictionMode value is preserved as an
//     unknown enum number rather than silently coerced to NORMAL.
//   - Cross-path payload identity (PRED-06): the same proto on the
//     streaming path (EventFrame body) and the sync gRPC path produces
//     byte-identical wire bytes, the property that lets a feature logged
//     from the bus be replayed into the sync RPC and compared.
//
// These complement the static `buf breaking` gate (EVT-07) and
// CODEOWNERS review on proto/inference/ (schema-evolution §5): both
// operate at schema-text / review time and cannot prove the live
// bindings behave as the docs promise. The tests sit in the external
// inference_test package so they exercise only the public surface, like
// the other EVT-21 / PRED contract trees.
package inference_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func canonicalFeatureVector() *inferencepb.FeatureVector {
	return &inferencepb.FeatureVector{
		SubjectId:     "AAPL",
		FeatureSetRef: "equity-momentum:7",
		Values: map[string]*inferencepb.FeatureValue{
			"px":     {Value: &inferencepb.FeatureValue_Scalar{Scalar: 192.5}},
			"dow":    {Value: &inferencepb.FeatureValue_Ordinal{Ordinal: 3}},
			"sector": {Value: &inferencepb.FeatureValue_Categorical{Categorical: "TECH"}},
			"embed":  {Value: &inferencepb.FeatureValue_Vector{Vector: &inferencepb.DoubleArray{Values: []float64{0.1, 0.2, 0.3}}}},
		},
		AsOf:           timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		SourceEventIds: []string{"evt-mkt-1", "evt-pos-2"},
	}
}

func canonicalPrediction() *inferencepb.PredictionEnvelope {
	return &inferencepb.PredictionEnvelope{
		SubjectId:            "AAPL",
		Model:                "vol-forecast@1.4.2",
		Value:                0.73,
		Confidence:           0.91,
		ClassLabel:           "",
		Explanation:          map[string]float64{"px": 0.4, "dow": -0.1},
		Mode:                 inferencepb.PredictionMode_PREDICTION_MODE_NORMAL,
		FeatureVectorEventId: "evt-feat-1",
		AsOf:                 timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
}

// unknownFieldNumber stands in for a field a future quant-ml schema
// bump might add. 1000 is well above the fields currently allocated on
// either inference message and outside the 19000–19999 reserved range,
// so it is a plausible next-allocation candidate (same choice as the
// EVT-21c compatibility tests).
const unknownFieldNumber = protowire.Number(1000)

func appendUnknownVarintField(b []byte, num protowire.Number, value uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	b = protowire.AppendVarint(b, value)
	return b
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

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	body, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// --- Version-identifier conventions ------------------------------------

// feature_set_ref is "{name}:{version}" and model is "{name}@{version}".
// The distinct separators are load-bearing: they are how the routing
// layer (PRED-03/04, PRED-09) tells a feature-set ref from a model id at
// a glance, and they parse into (name, version) without ambiguity. A
// refactor that normalised one separator to the other would silently
// reroute traffic — this test pins the convention.
func TestVersionConvention_FeatureSetRefAndModelParseDistinctly(t *testing.T) {
	fv := canonicalFeatureVector()
	name, version, ok := strings.Cut(fv.FeatureSetRef, ":")
	if !ok || name == "" || version == "" {
		t.Fatalf("feature_set_ref %q is not %q form", fv.FeatureSetRef, "name:version")
	}
	if strings.Contains(fv.FeatureSetRef, "@") {
		t.Errorf("feature_set_ref %q must not use the model '@' separator", fv.FeatureSetRef)
	}

	pred := canonicalPrediction()
	mName, mVersion, ok := strings.Cut(pred.Model, "@")
	if !ok || mName == "" || mVersion == "" {
		t.Fatalf("model %q is not %q form", pred.Model, "name@version")
	}
	if strings.Contains(pred.Model, ":") {
		t.Errorf("model %q must not use the feature-set ':' separator", pred.Model)
	}
}

// The version identifiers are wire strings — they must survive a
// marshal/unmarshal round-trip byte-for-byte, since routing happens
// after the payload crosses the bus / gRPC boundary.
func TestVersionConvention_IdentifiersRoundTrip(t *testing.T) {
	var fv inferencepb.FeatureVector
	if err := proto.Unmarshal(mustMarshal(t, canonicalFeatureVector()), &fv); err != nil {
		t.Fatalf("unmarshal FeatureVector: %v", err)
	}
	if fv.FeatureSetRef != "equity-momentum:7" {
		t.Errorf("feature_set_ref=%q want %q", fv.FeatureSetRef, "equity-momentum:7")
	}
	var pred inferencepb.PredictionEnvelope
	if err := proto.Unmarshal(mustMarshal(t, canonicalPrediction()), &pred); err != nil {
		t.Fatalf("unmarshal PredictionEnvelope: %v", err)
	}
	if pred.Model != "vol-forecast@1.4.2" {
		t.Errorf("model=%q want %q", pred.Model, "vol-forecast@1.4.2")
	}
	// The features→prediction join key (PRED-05 lineage) must survive too.
	if pred.FeatureVectorEventId != "evt-feat-1" {
		t.Errorf("feature_vector_event_id=%q want %q", pred.FeatureVectorEventId, "evt-feat-1")
	}
}

// --- Forward compatibility ---------------------------------------------

// A future FeatureVector field must survive unmarshal + re-marshal via
// current bindings. Replay (EVT-20) re-marshals payloads, so a dropped
// field here would corrupt a replay of features authored by a newer
// schema — the replay-determinism property PRED-06 extends across paths.
func TestForwardCompat_UnknownFeatureVectorFieldPreserved(t *testing.T) {
	body := appendUnknownVarintField(mustMarshal(t, canonicalFeatureVector()), unknownFieldNumber, 42)

	var got inferencepb.FeatureVector
	if err := proto.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal with unknown field: %v", err)
	}
	if got.SubjectId != "AAPL" {
		t.Errorf("known field corrupted: SubjectId=%q", got.SubjectId)
	}
	if !containsTag(got.ProtoReflect().GetUnknown(), unknownFieldNumber) {
		t.Fatalf("unknown field %d not preserved on unmarshal", unknownFieldNumber)
	}
	if !containsTag(mustMarshal(t, &got), unknownFieldNumber) {
		t.Errorf("unknown field %d dropped on re-marshal", unknownFieldNumber)
	}
}

// Same property for PredictionEnvelope — predictions are also replayed
// and join-compared, so a future field must survive the round-trip.
func TestForwardCompat_UnknownPredictionFieldPreserved(t *testing.T) {
	body := appendUnknownVarintField(mustMarshal(t, canonicalPrediction()), unknownFieldNumber, 7)

	var got inferencepb.PredictionEnvelope
	if err := proto.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal with unknown field: %v", err)
	}
	if got.Model != "vol-forecast@1.4.2" {
		t.Errorf("known field corrupted: Model=%q", got.Model)
	}
	if !containsTag(got.ProtoReflect().GetUnknown(), unknownFieldNumber) {
		t.Fatalf("unknown field %d not preserved on unmarshal", unknownFieldNumber)
	}
	if !containsTag(mustMarshal(t, &got), unknownFieldNumber) {
		t.Errorf("unknown field %d dropped on re-marshal", unknownFieldNumber)
	}
}

// A future FeatureValue oneof arm (a new value type) must not break
// decoding an existing FeatureVector. The whole value reads as unset on
// the current binding but the bytes are preserved as an unknown field
// inside the FeatureValue, so re-marshal does not lose it. This is what
// lets quant-ml add (say) a sparse-vector arm without a coordinated
// flag-day across every consumer.
func TestForwardCompat_UnknownFeatureValueOneofArmPreserved(t *testing.T) {
	const futureArm = protowire.Number(99) // not one of scalar/ordinal/categorical/vector (1..4)
	fvalBody := appendUnknownVarintField(nil, futureArm, 5)

	var fval inferencepb.FeatureValue
	if err := proto.Unmarshal(fvalBody, &fval); err != nil {
		t.Fatalf("unmarshal FeatureValue with future arm: %v", err)
	}
	if fval.Value != nil {
		t.Errorf("future oneof arm should read as unset value, got %T", fval.Value)
	}
	if !containsTag(mustMarshal(t, &fval), futureArm) {
		t.Errorf("future oneof arm %d dropped on re-marshal", futureArm)
	}
}

// --- Backward compatibility -------------------------------------------

// Current code reading a v0 producer's bytes: optional fields a later
// schema added (here class_label / explanation / source_event_ids /
// degraded_reason) are wire-absent in the old payload and decode to
// their zero value, not an error.
func TestBackwardCompat_AbsentOptionalFieldsDefaultToZero(t *testing.T) {
	// A minimal FeatureVector as an early producer might have emitted:
	// only the required routing fields, no source_event_ids.
	minFV := &inferencepb.FeatureVector{
		SubjectId:     "AAPL",
		FeatureSetRef: "equity-momentum:7",
		AsOf:          timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	var fv inferencepb.FeatureVector
	if err := proto.Unmarshal(mustMarshal(t, minFV), &fv); err != nil {
		t.Fatalf("unmarshal minimal FeatureVector: %v", err)
	}
	if len(fv.SourceEventIds) != 0 || len(fv.Values) != 0 {
		t.Errorf("absent fields not zero: SourceEventIds=%v Values=%v", fv.SourceEventIds, fv.Values)
	}

	// A minimal NORMAL prediction: no class_label (regression), no
	// explanation, no degraded_reason.
	minPred := &inferencepb.PredictionEnvelope{
		SubjectId: "AAPL",
		Model:     "vol-forecast@1.4.2",
		Value:     0.5,
		Mode:      inferencepb.PredictionMode_PREDICTION_MODE_NORMAL,
		AsOf:      timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	var pred inferencepb.PredictionEnvelope
	if err := proto.Unmarshal(mustMarshal(t, minPred), &pred); err != nil {
		t.Fatalf("unmarshal minimal PredictionEnvelope: %v", err)
	}
	if pred.ClassLabel != "" || len(pred.Explanation) != 0 || pred.DegradedReason != "" {
		t.Errorf("absent fields not zero: ClassLabel=%q Explanation=%v DegradedReason=%q",
			pred.ClassLabel, pred.Explanation, pred.DegradedReason)
	}
}

// --- oneof tagged-union discrimination --------------------------------

// FeatureValue is a tagged union: scalar (1), ordinal (2), categorical
// (3), vector (4) ride distinct wire tags. The contract from PRED-01 is
// that a categorical can NEVER be misread as a scalar magnitude — the
// wire tag, not field order, carries the type. Round-trip each arm and
// assert the type is preserved.
func TestOneof_FeatureValueArmsKeepTheirType(t *testing.T) {
	cases := map[string]*inferencepb.FeatureValue{
		"scalar":      {Value: &inferencepb.FeatureValue_Scalar{Scalar: 1.5}},
		"ordinal":     {Value: &inferencepb.FeatureValue_Ordinal{Ordinal: 4}},
		"categorical": {Value: &inferencepb.FeatureValue_Categorical{Categorical: "USD"}},
		"vector":      {Value: &inferencepb.FeatureValue_Vector{Vector: &inferencepb.DoubleArray{Values: []float64{1, 2}}}},
	}
	for name, in := range cases {
		var got inferencepb.FeatureValue
		if err := proto.Unmarshal(mustMarshal(t, in), &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		switch name {
		case "scalar":
			if _, ok := got.Value.(*inferencepb.FeatureValue_Scalar); !ok {
				t.Errorf("scalar decoded as %T", got.Value)
			}
		case "ordinal":
			if _, ok := got.Value.(*inferencepb.FeatureValue_Ordinal); !ok {
				t.Errorf("ordinal decoded as %T", got.Value)
			}
		case "categorical":
			if _, ok := got.Value.(*inferencepb.FeatureValue_Categorical); !ok {
				t.Errorf("categorical decoded as %T (must not be read as a magnitude)", got.Value)
			}
		case "vector":
			if _, ok := got.Value.(*inferencepb.FeatureValue_Vector); !ok {
				t.Errorf("vector decoded as %T", got.Value)
			}
		}
	}
}

// --- PredictionMode open-enum semantics -------------------------------

// proto3 enums are open: a future PredictionMode value (e.g. a third
// trust level) decoded by current bindings is preserved as its raw
// number, NOT clamped to a known value. The PRED-02 §4 consumer rule —
// treat any non-NORMAL mode (UNSPECIFIED or unknown) as DEGRADED — then
// keeps a current consumer safe in front of a newer producer: it must
// never read an unrecognised mode as NORMAL and act on full trust.
func TestOpenEnum_UnknownPredictionModePreservedAndNotNormal(t *testing.T) {
	const futureMode = 3 // beyond UNSPECIFIED(0)/NORMAL(1)/DEGRADED(2)
	body := canonicalPrediction()
	bodyBytes := mustMarshal(t, body)
	// Overwrite the mode field (tag 7, varint) by appending a second
	// occurrence — proto3 last-wins for scalar fields on decode.
	bodyBytes = appendUnknownVarintField(bodyBytes, 7, futureMode)

	var got inferencepb.PredictionEnvelope
	if err := proto.Unmarshal(bodyBytes, &got); err != nil {
		t.Fatalf("unmarshal with future mode: %v", err)
	}
	if int32(got.Mode) != futureMode {
		t.Errorf("unknown enum number not preserved: Mode=%d want %d", got.Mode, futureMode)
	}
	if got.Mode == inferencepb.PredictionMode_PREDICTION_MODE_NORMAL {
		t.Error("unknown mode must not decode as NORMAL — PRED-02 §4 requires non-NORMAL ⇒ DEGRADED")
	}
	// Re-marshal preserves the unknown number so a relay/replay does not
	// downgrade the producer's signal.
	var round inferencepb.PredictionEnvelope
	if err := proto.Unmarshal(mustMarshal(t, &got), &round); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if int32(round.Mode) != futureMode {
		t.Errorf("unknown enum number lost on re-marshal: Mode=%d", round.Mode)
	}
}

// --- Cross-path payload identity (PRED-06) ----------------------------

// The same FeatureVector proto is the body on both paths: wrapped in an
// EventFrame on the streaming bus, and used directly as the gRPC request
// on the sync path (PRED-06 — no wrapper messages). The contract is that
// the payload bytes are identical across paths, so a feature logged from
// the bus can be replayed into the sync RPC and the two answers compared.
// Assert the payload recovered from an EventFrame equals the bytes a sync
// caller would marshal directly.
func TestCrossPath_FeatureVectorPayloadIdenticalAcrossTransports(t *testing.T) {
	fv := canonicalFeatureVector()
	syncBody, err := proto.MarshalOptions{Deterministic: true}.Marshal(fv)
	if err != nil {
		t.Fatalf("marshal sync body: %v", err)
	}

	// Streaming path: payload rides inside an EventFrame.
	frame := &envelopepb.EventFrame{
		Envelope: &envelopepb.Envelope{EventId: "evt-feat-1"},
		Payload:  syncBody,
	}
	frameBytes, err := proto.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	_, streamingPayload, err := bus.Unframe(frameBytes)
	if err != nil {
		t.Fatalf("unframe: %v", err)
	}
	if !bytes.Equal(streamingPayload, syncBody) {
		t.Errorf("payload differs across paths:\n  streaming: %x\n  sync:      %x", streamingPayload, syncBody)
	}

	// And it decodes back to the same FeatureVector on either path.
	var got inferencepb.FeatureVector
	if err := proto.Unmarshal(streamingPayload, &got); err != nil {
		t.Fatalf("unmarshal streaming payload: %v", err)
	}
	if !proto.Equal(&got, fv) {
		t.Errorf("FeatureVector not equal after cross-path round-trip")
	}
}
