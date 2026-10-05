package replay

import (
	"encoding/json"
	"errors"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/pricing/structured"
)

// The canonical working spec contains an interface. Tag its concrete model
// explicitly rather than decoding it into a map and guessing its semantics.
type structuredInput struct {
	Spec       compute.StructuredSpec
	PrepayKind string
	Prepay     json.RawMessage
}

func encodeStructure(spec compute.StructuredSpec) (structuredInput, error) {
	out := structuredInput{Spec: spec}
	out.Spec.Prepay = nil
	if spec.Prepay == nil {
		return out, nil
	}
	switch spec.Prepay.(type) {
	case structured.ConstantCPR, *structured.ConstantCPR:
		out.PrepayKind = "constant-cpr-v1"
	case structured.PSA, *structured.PSA:
		out.PrepayKind = "psa-v1"
	case structured.Behavioral, *structured.Behavioral:
		out.PrepayKind = "behavioral-v1"
	default:
		return out, errors.New("prepayment model has no replay codec")
	}
	var err error
	out.Prepay, err = json.Marshal(spec.Prepay)
	return out, err
}

func (in structuredInput) decode() (compute.StructuredSpec, error) {
	out := in.Spec
	if out.Prepay != nil {
		return out, errors.New("untagged prepayment model")
	}
	var model structured.PrepayModel
	switch in.PrepayKind {
	case "":
		if len(in.Prepay) != 0 && string(in.Prepay) != "null" {
			return out, errors.New("untagged prepayment bytes")
		}
		return out, nil
	case "constant-cpr-v1":
		model = new(structured.ConstantCPR)
	case "psa-v1":
		model = new(structured.PSA)
	case "behavioral-v1":
		model = new(structured.Behavioral)
	default:
		return out, errors.New("unsupported retained prepayment model")
	}
	if len(in.Prepay) == 0 || string(in.Prepay) == "null" {
		return out, errors.New("missing retained prepayment parameters")
	}
	if err := json.Unmarshal(in.Prepay, model); err != nil {
		return out, err
	}
	out.Prepay = model
	return out, nil
}
