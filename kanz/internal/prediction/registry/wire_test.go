package registry_test

import (
	"errors"
	"testing"
	"time"

	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"

	"github.com/eighred/kanz/internal/prediction/registry"
)

// THE CODEC IS THE CROSS-LANGUAGE SEAM, so what it drops is invisible until a
// replica in the other language is already wrong. Each case below names the
// consequence of the field it protects rather than asserting equality for its
// own sake.

func TestWire_RoundTripCarriesEveryFieldTheProducerSets(t *testing.T) {
	validatedAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	expiresAt := validatedAt.Add(90 * 24 * time.Hour)
	in := registry.Event{
		Origin: "inference-7",
		Op:     registry.OpRegister,
		Metadata: registry.Metadata{
			ModelID:             "portfolio-risk-gbm",
			FeatureSetRef:       "portfolio-risk:1",
			ArtifactURI:         "s3://models/portfolio-risk-gbm/3",
			ConfidenceThreshold: 0.72,
		},
		Role: registry.RolePrimary,
		Validation: registry.Validation{
			Passed:      true,
			Report:      "s3://reports/mlops-01a/gbm-3",
			ValidatedAt: validatedAt,
			ExpiresAt:   expiresAt,
		},
	}

	pb, err := registry.ToProto(in)
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	got, err := registry.FromProto(pb)
	if err != nil {
		t.Fatalf("FromProto: %v", err)
	}

	if got.Metadata.ArtifactURI != in.Metadata.ArtifactURI {
		t.Errorf("artifact_uri lost on the wire (%q) — a replica that gains a ModelLoader would "+
			"have a converged registry and nothing to fetch", got.Metadata.ArtifactURI)
	}
	if got.Metadata.ConfidenceThreshold != in.Metadata.ConfidenceThreshold {
		t.Errorf("confidence_threshold lost (%v) — each replica would re-derive its own and act "+
			"differently on the same score", got.Metadata.ConfidenceThreshold)
	}
	if got.Metadata.FeatureSetRef != in.Metadata.FeatureSetRef {
		t.Errorf("feature_set_ref lost (%q) — resolution falls back to name, the one thing this "+
			"package exists to prevent", got.Metadata.FeatureSetRef)
	}
	if !got.Validation.ValidatedAt.Equal(validatedAt) || !got.Validation.ExpiresAt.Equal(expiresAt) {
		t.Errorf("validation dates lost (validated=%v expires=%v) — an absent expiry reads as "+
			"'valid forever', which removes the only thing that makes SR 11-7 mean anything over time",
			got.Validation.ValidatedAt, got.Validation.ExpiresAt)
	}
	if got.Role != registry.RolePrimary || got.Op != registry.OpRegister || got.Origin != in.Origin {
		t.Errorf("op/role/origin round-trip: got op=%v role=%v origin=%q", got.Op, got.Role, got.Origin)
	}
	if !got.Validation.Passed || got.Validation.Report != in.Validation.Report {
		t.Errorf("validation verdict round-trip: %+v", got.Validation)
	}
}

func TestWire_UnregisterHasAFold(t *testing.T) {
	pb := &inferencepb.ModelRegistryEvent{
		Op:      inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_UNREGISTER,
		Origin:  "inference-2",
		ModelId: "retired-model",
	}
	got, err := registry.FromProto(pb)
	if err != nil {
		t.Fatalf("FromProto: %v", err)
	}
	if got.Op != registry.OpUnregister {
		t.Fatalf("UNREGISTER decoded as %v — with no fold for it a follower keeps naming a "+
			"WITHDRAWN model as primary for its contract, indefinitely", got.Op)
	}
}

func TestWire_AnUnspecifiedOpIsAnErrorRatherThanASilentSkip(t *testing.T) {
	_, err := registry.FromProto(&inferencepb.ModelRegistryEvent{ModelId: "m1"})
	if !errors.Is(err, registry.ErrUnknownOp) {
		t.Fatalf("want ErrUnknownOp, got %v — an op silently skipped rebuilds a registry missing "+
			"every mutation of that kind and reports the result with full confidence", err)
	}
}

func TestWire_ModelIDIsRequiredBecauseItIsThePartitionKey(t *testing.T) {
	if _, err := registry.ToProto(registry.Event{Origin: "a", Op: registry.OpRegister}); err == nil {
		t.Fatal("ToProto accepted an empty model_id — it is the partition key, and without it a " +
			"model's validation and its promotion can land on different partitions")
	}
	if _, err := registry.FromProto(&inferencepb.ModelRegistryEvent{
		Op: inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_REGISTER,
	}); err == nil {
		t.Fatal("FromProto accepted an empty model_id")
	}
}

func TestValidationExpiry(t *testing.T) {
	now := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	if (registry.Validation{}).Expired(now) {
		t.Error("a zero ExpiresAt reported as expired — that is what a producer which never set " +
			"the field publishes, and flagging it makes the finding about the reader")
	}
	if !(registry.Validation{ExpiresAt: now.Add(-time.Second)}).Expired(now) {
		t.Error("a lapsed validation reported as current")
	}
	if (registry.Validation{ExpiresAt: now.Add(time.Second)}).Expired(now) {
		t.Error("a current validation reported as expired")
	}
}
