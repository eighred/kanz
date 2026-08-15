// Package prediction is the Go side of the hybrid prediction
// layer — feature publishing (PRED-03), sync gRPC client (PRED-07),
// and the shared types both paths consume. Symmetric counterpart
// to kanz-py's `kanz_inference` package (PRED-04/05/08).
//
// # Where Go runs in the prediction layer
//
// THE HYBRID SPLIT, and it is verifiable from this repository rather than from
// the design document that first stated it (deleted 2026-07-29): features are COMPUTED in
// Go (the engine has the source state) and PUBLISHED to the bus.
// services/risk-engine/internal/app/features.go is the Go half and
// kanz-py/kanz_inference is the Python one.
// Inference is computed in Python (model serving stack lives
// there). Predictions flow back to Go via the bus (streaming) or
// gRPC (sync). PRED-03 is the first half of that loop: Go →
// inference.feature.computed FACT on the bus → Python worker
// (PRED-04) reads it → produces inference.prediction.scored.
//
// # Boundary
//
// Lives under `kanz/internal/`, so per the BRAIN's "promote to
// top-level when 2nd consumer appears" rule this is module-
// private. The cross-module surface (if any external consumer
// ever needs to construct a FeatureVector for replay tooling)
// would lift the types into `kanz/internal/prediction/api/v1/` —
// same shape as `kanz/internal/risk/api/v1/` from RISK-01.
package prediction

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// Event-type + schema-ref constants for the feature-published
// FACT. Subject taxonomy: `inference.feature.computed`
// (kanz-schemas/docs/subject-taxonomy.md §1 example).
const (
	EventTypeFeatureComputed = "inference.feature.computed"
	SchemaRefFeatureVector   = "inference.v1.FeatureVector:1"
)

// --- Go-native types --------------------------------------------------
//
// Mirror inference.v1 proto messages as plain Go structs. Same two-
// layer split RISK-03/RISK-10 chose: proto is the wire shape,
// Go-native is the in-memory engine shape. Translation happens at
// the publish boundary; downstream Go code (future feature-store
// integration PRED-11) sees Go-native types only.

// SubjectID identifies what the features describe.
type SubjectID string

// FeatureSetRef is the catalog version: "{name}:{version}". The
// inference layer routes by matching this to a model that consumes
// the same feature set (PRED-09).
type FeatureSetRef string

// FeatureName is one named feature in the catalog.
type FeatureName string

// FeatureVector is the Go-native form of inference.v1.FeatureVector.
type FeatureVector struct {
	SubjectID      SubjectID
	FeatureSetRef  FeatureSetRef
	Values         map[FeatureName]FeatureValue
	AsOf           time.Time
	SourceEventIDs []string
}

// ValueKind tags the FeatureValue oneof. Mirrors the proto's
// `value` oneof; the Kind field is the discriminator the
// translator switches on.
type ValueKind int

const (
	// ValueKindUnspecified is the zero value; never set explicitly
	// on a published value (rejected by validation).
	ValueKindUnspecified ValueKind = iota
	ValueKindScalar
	ValueKindOrdinal
	ValueKindCategorical
	ValueKindVector
)

// FeatureValue is a tagged-union mirroring inference.v1.FeatureValue.
// Only the field matching Kind is read by the translator — the
// others are ignored, matching proto oneof semantics.
type FeatureValue struct {
	Kind        ValueKind
	Scalar      float64
	Ordinal     int64
	Categorical string
	Vector      []float64
}

// Scalar / Ordinal / Categorical / Vector are convenience
// constructors for the four FeatureValue kinds. Recommended call-
// site form: `prediction.Scalar(1.5)` rather than building the
// FeatureValue struct by hand.
func Scalar(f float64) FeatureValue { return FeatureValue{Kind: ValueKindScalar, Scalar: f} }
func Ordinal(i int64) FeatureValue  { return FeatureValue{Kind: ValueKindOrdinal, Ordinal: i} }
func Categorical(s string) FeatureValue {
	return FeatureValue{Kind: ValueKindCategorical, Categorical: s}
}
func Vector(v []float64) FeatureValue {
	cp := make([]float64, len(v))
	copy(cp, v)
	return FeatureValue{Kind: ValueKindVector, Vector: cp}
}

// --- Publisher --------------------------------------------------------

// Publisher emits feature-vector FACTs to the bus. Shape mirrors
// risk/publish.Publisher (RISK-10): wraps a shared bus.Producer
// so producer_sequence stays monotonic across event types
// originating from the same source.
type Publisher struct {
	producer *bus.Producer
}

// NewPublisher returns a Publisher around the given bus.Producer.
func NewPublisher(producer *bus.Producer) (*Publisher, error) {
	if producer == nil {
		return nil, errors.New("prediction: producer is nil")
	}
	return &Publisher{producer: producer}, nil
}

// PublishFeatures publishes one FeatureVector as an
// inference.feature.computed FACT. Validates the Go-native input,
// translates to proto, hands to the Producer (which auto-stamps
// envelope identity + lineage from ctx per EVT-17b).
//
// partition_key = SubjectID so per-subject ordering holds end-to-
// end: feature events for one subject are ordered, the inference
// worker (PRED-04) sees them in order, and predictions back to
// Go preserve the same ordering.
func (p *Publisher) PublishFeatures(ctx context.Context, fv FeatureVector) error {
	if fv.SubjectID == "" {
		return errors.New("prediction: SubjectID required")
	}
	if fv.FeatureSetRef == "" {
		return errors.New("prediction: FeatureSetRef required")
	}
	if len(fv.Values) == 0 {
		return errors.New("prediction: Values must be non-empty")
	}
	if fv.AsOf.IsZero() {
		return errors.New("prediction: AsOf required")
	}
	for name, v := range fv.Values {
		if v.Kind == ValueKindUnspecified {
			return errors.New("prediction: FeatureValue Kind UNSPECIFIED for " + string(name))
		}
	}
	payload, err := toProtoFeatureVector(fv)
	if err != nil {
		return err
	}
	return p.producer.Publish(ctx, bus.Event{
		Subject:          EventTypeFeatureComputed,
		EventType:        EventTypeFeatureComputed,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "inference",
		EventTime:        fv.AsOf,
		PartitionKey:     string(fv.SubjectID),
		PayloadSchemaRef: SchemaRefFeatureVector,
		Payload:          payload,
	})
}

// --- Translation: Go-native → proto ----------------------------------

func toProtoFeatureVector(fv FeatureVector) (*inferencepb.FeatureVector, error) {
	values := make(map[string]*inferencepb.FeatureValue, len(fv.Values))
	for name, v := range fv.Values {
		pv, err := toProtoFeatureValue(v)
		if err != nil {
			return nil, errors.New("prediction: translate " + string(name) + ": " + err.Error())
		}
		values[string(name)] = pv
	}
	return &inferencepb.FeatureVector{
		SubjectId:      string(fv.SubjectID),
		FeatureSetRef:  string(fv.FeatureSetRef),
		Values:         values,
		AsOf:           timestamppb.New(fv.AsOf),
		SourceEventIds: fv.SourceEventIDs,
	}, nil
}

func toProtoFeatureValue(v FeatureValue) (*inferencepb.FeatureValue, error) {
	pv := &inferencepb.FeatureValue{}
	switch v.Kind {
	case ValueKindScalar:
		pv.Value = &inferencepb.FeatureValue_Scalar{Scalar: v.Scalar}
	case ValueKindOrdinal:
		pv.Value = &inferencepb.FeatureValue_Ordinal{Ordinal: v.Ordinal}
	case ValueKindCategorical:
		pv.Value = &inferencepb.FeatureValue_Categorical{Categorical: v.Categorical}
	case ValueKindVector:
		pv.Value = &inferencepb.FeatureValue_Vector{
			Vector: &inferencepb.DoubleArray{Values: v.Vector},
		}
	default:
		return nil, errors.New("unspecified FeatureValue.Kind")
	}
	return pv, nil
}
