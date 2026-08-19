package registry

// The Go end of the model registry's WIRE CONTRACT (#112 step 3).
//
// inference.v1.ModelRegistryEvent is the shared shape both halves of the fleet
// speak; kanz-py's BusRegistryPublisher is the producer. Until this file the Go
// registry.Event had NO wire form at all, which is why a registry that
// "converges across replicas" converged only across replicas written in one
// language.
//
// # Everything on the wire is carried, including what this side cannot yet use
//
// artifact_uri and confidence_threshold have no reader in Go today — risk-engine
// is a METADATA-ONLY follower and never materialises a model. They are decoded
// anyway, because the alternative is a codec that silently drops fields: the day
// a Go replica gains a ModelLoader, the log would have converged and the loader
// would have nothing to fetch, and nothing in between would have said so.
//
// Metadata.Version is the mirror image — a Go-side field with no wire field. It
// stays empty on everything decoded here rather than being invented from the
// model id, because a version nobody published is not a version.

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"
)

// The subject the registry log rides, and the payload contract on it. Both must
// agree with kanz-py's publish.py — SUBJECT_MODEL_REGISTERED and
// SCHEMA_REF_MODEL_REGISTRY there — because a producer and a consumer that
// disagree about either one produce silence, not an error.
const (
	SubjectModelRegistered = "platform.model.registered"
	SchemaRefModelRegistry = "inference.v1.ModelRegistryEvent:1"
	// DomainModelRegistry is the envelope domain. platform, not inference: the
	// subject namespace and the proto package are independent here, and this log
	// belongs on the long-retention platform stream rather than the 24h
	// inference one (the same split platform.authz.decision made carrying an
	// observation.v1.DecisionLog).
	DomainModelRegistry = "platform"
)

// ErrUnknownOp is returned for a wire op this build has no fold for.
//
// IT IS AN ERROR RATHER THAN A SKIP, and that is the whole point of it. A
// follower that ignored an op it did not recognise would rebuild a registry
// missing every mutation of that kind and report the result with full
// confidence — the failure mode that let MODEL_REGISTRY_OP_UNREGISTER have no
// Go fold at all until #112, so a retired model stayed primary forever. A
// decode error is visible; a silent skip is not.
var ErrUnknownOp = errors.New("registry: unknown model-registry op")

var opToProto = map[Op]inferencepb.ModelRegistryOp{
	OpRegister:   inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_REGISTER,
	OpValidate:   inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_RECORD_VALIDATION,
	OpUnregister: inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_UNREGISTER,
}

var opFromProto = map[inferencepb.ModelRegistryOp]Op{
	inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_REGISTER:          OpRegister,
	inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_RECORD_VALIDATION: OpValidate,
	inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_UNREGISTER:        OpUnregister,
}

// ToProto renders one registry mutation onto the wire.
func ToProto(e Event) (*inferencepb.ModelRegistryEvent, error) {
	if e.Metadata.ModelID == "" {
		return nil, errors.New("registry: model_id is required (it is the partition key)")
	}
	if e.Origin == "" {
		return nil, errors.New("registry: origin is required (loop prevention)")
	}
	op, ok := opToProto[e.Op]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownOp, e.Op)
	}
	out := &inferencepb.ModelRegistryEvent{
		Op:                  op,
		Origin:              e.Origin,
		ModelId:             e.Metadata.ModelID,
		FeatureSetRef:       e.Metadata.FeatureSetRef,
		ConfidenceThreshold: e.Metadata.ConfidenceThreshold,
		ArtifactUri:         e.Metadata.ArtifactURI,
		Role:                roleToProto(e.Role),
		Passed:              e.Validation.Passed,
		ReportUri:           e.Validation.Report,
	}
	if !e.Validation.ValidatedAt.IsZero() {
		out.ValidatedAt = timestamppb.New(e.Validation.ValidatedAt)
	}
	if !e.Validation.ExpiresAt.IsZero() {
		out.ExpiresAt = timestamppb.New(e.Validation.ExpiresAt)
	}
	return out, nil
}

// FromProto folds one wire event back into a registry mutation.
func FromProto(p *inferencepb.ModelRegistryEvent) (Event, error) {
	if p == nil {
		return Event{}, errors.New("registry: nil ModelRegistryEvent")
	}
	if p.GetModelId() == "" {
		return Event{}, errors.New("registry: model_id is required (it is the partition key)")
	}
	op, ok := opFromProto[p.GetOp()]
	if !ok {
		return Event{}, fmt.Errorf("%w: %s", ErrUnknownOp, p.GetOp())
	}
	e := Event{
		Origin: p.GetOrigin(),
		Op:     op,
		Metadata: Metadata{
			ModelID:             p.GetModelId(),
			FeatureSetRef:       p.GetFeatureSetRef(),
			ArtifactURI:         p.GetArtifactUri(),
			ConfidenceThreshold: p.GetConfidenceThreshold(),
		},
		Role: roleFromProto(p.GetRole()),
		Validation: Validation{
			Passed: p.GetPassed(),
			Report: p.GetReportUri(),
		},
	}
	if ts := p.GetValidatedAt(); ts != nil {
		e.Validation.ValidatedAt = ts.AsTime()
	}
	if ts := p.GetExpiresAt(); ts != nil {
		e.Validation.ExpiresAt = ts.AsTime()
	}
	return e, nil
}

// roleToProto maps a serving role onto the wire. RoleUnspecified stays
// unspecified rather than defaulting to CANDIDATE: a register that never said
// which role it wanted is a producer defect, and inventing the safer of the two
// here would hide it on every replica.
func roleToProto(r Role) inferencepb.ModelRole {
	switch r {
	case RoleCandidate:
		return inferencepb.ModelRole_MODEL_ROLE_CANDIDATE
	case RolePrimary:
		return inferencepb.ModelRole_MODEL_ROLE_PRIMARY
	default:
		return inferencepb.ModelRole_MODEL_ROLE_UNSPECIFIED
	}
}

func roleFromProto(r inferencepb.ModelRole) Role {
	switch r {
	case inferencepb.ModelRole_MODEL_ROLE_CANDIDATE:
		return RoleCandidate
	case inferencepb.ModelRole_MODEL_ROLE_PRIMARY:
		return RolePrimary
	default:
		return RoleUnspecified
	}
}
