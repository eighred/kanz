package replay

import (
	"encoding/json"
	"testing"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/pricing/structured"
)

func TestPrepaymentModelsRetainParametersAndBehavior(t *testing.T) {
	models := []structured.PrepayModel{
		structured.ConstantCPR{CPR: .06, CDR: .01, Sev: .4},
		&structured.ConstantCPR{CPR: .07, CDR: .02, Sev: .5},
		structured.PSA{Multiple: 1.3, CDR: .02, Sev: .5},
		&structured.PSA{Multiple: .8, CDR: .03, Sev: .6},
		structured.Behavioral{Base: .02, Max: .3, Steepness: 40, CDR: .02, Sev: .5},
		&structured.Behavioral{Base: .03, Max: .4, Steepness: 30, CDR: .01, Sev: .6},
	}
	for _, model := range models {
		stored, err := encodeStructure(compute.StructuredSpec{Prepay: model})
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(stored)
		if err != nil {
			t.Fatal(err)
		}
		var restored structuredInput
		if err := json.Unmarshal(body, &restored); err != nil {
			t.Fatal(err)
		}
		spec, err := restored.decode()
		if err != nil {
			t.Fatal(err)
		}
		for month := 1; month <= 360; month++ {
			if spec.Prepay.SMM(month, .06, structured.RateEnv{}) != model.SMM(month, .06, structured.RateEnv{}) || spec.Prepay.MDR(month) != model.MDR(month) || spec.Prepay.Severity() != model.Severity() {
				t.Fatalf("prepayment behavior changed for %T", model)
			}
		}
	}
	for _, bad := range []structuredInput{{PrepayKind: "unknown"}, {PrepayKind: "psa-v1"}, {PrepayKind: "psa-v1", Prepay: json.RawMessage("null")}, {Prepay: json.RawMessage("{}")}} {
		if _, err := bad.decode(); err == nil {
			t.Fatal("invalid prepayment codec accepted")
		}
	}
}
