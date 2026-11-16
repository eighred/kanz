package compliance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The wire schema, rather than the production registry, supplies the complete
// baseline enum and its matching message shapes.
func contractRules(t *testing.T) []*compliancepb.Rule {
	t.Helper()
	names := []string{"concentration", "restriction", "issuer_exclusion", "leverage_cap", "currency_restriction", "buying_power", "risk_measure", "venue_margin", "cash_buffer"}
	var rules []*compliancepb.Rule
	for i, name := range names {
		r := &compliancepb.Rule{RuleId: name, Type: compliancepb.RuleType(i + 1), OnViolation: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN}
		field := r.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name))
		if field == nil {
			t.Fatalf("missing schema field %s", name)
		}
		r.ProtoReflect().Mutable(field)
		rules = append(rules, r)
	}
	// A new enum requires an explicit acceptance decision and fixture.
	if len(compliancepb.RuleType_name) != len(rules)+1 {
		t.Fatal("baseline schema rule coverage changed")
	}
	return rules
}

func TestMandateRuleContractMatrix(t *testing.T) {
	rules := contractRules(t)
	for _, r := range rules {
		t.Run(r.GetType().String(), func(t *testing.T) {
			if err := ValidateMandate(mandate(r)); err != nil {
				t.Fatalf("matching message refused: %v", err)
			}
			missing := proto.Clone(r).(*compliancepb.Rule)
			missing.Params = nil
			variants := []*compliancepb.Rule{missing}
			for _, other := range rules {
				if other.Type != r.Type {
					wrong := proto.Clone(other).(*compliancepb.Rule)
					wrong.Type = r.Type
					variants = append(variants, wrong)
				}
			}
			for i, bad := range variants {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					if err := ValidateMandate(mandate(bad)); !errors.Is(err, ErrMandateIncomplete) {
						t.Fatalf("unreadable params accepted: %v", err)
					}
					called := false
					registry := DefaultRegistry()
					registry.Register(bad.Type, func(*Candidate, *compliancepb.Rule) *compliancepb.Violation { called = true; return nil })
					res := NewEngine(registry).Evaluate(context.Background(), &Candidate{Book: twoEqualBook(), AsOf: t0}, mandate(bad))
					if called || res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH || len(res.GetViolations()) != 1 || res.GetViolations()[0].GetSeverity() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
						t.Fatalf("unreadable WARN reached evaluator or escaped hard denial: called=%v result=%v", called, res)
					}
				})
			}
		})
	}
	var wrappedNil *compliancepb.Rule_Concentration
	for _, r := range []*compliancepb.Rule{nil, {RuleId: "nil-wrapper", Type: 1, Params: wrappedNil}, {RuleId: "nil-message", Type: 1, Params: &compliancepb.Rule_Concentration{}}, {RuleId: "future", Type: 99}} {
		if err := ValidateMandate(mandate(r)); !errors.Is(err, ErrMandateIncomplete) {
			t.Fatalf("nil/unknown rule accepted: %v", err)
		}
	}
}

func TestMandateRuleDecimalDomainBeforeEvaluation(t *testing.T) {
	for _, exp := range []int32{-65, -64, 64, 65} {
		t.Run(fmt.Sprint(exp), func(t *testing.T) {
			r := concentrationRule(60)
			r.GetConcentration().MaxWeight = dec(1, exp)
			r.OnViolation = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN
			valid := exp >= -64 && exp <= 64
			err := ValidateMandate(mandate(r))
			if (err == nil) != valid {
				t.Fatalf("domain validation = %v for exponent %d", err, exp)
			}
			called := false
			registry := DefaultRegistry()
			registry.Register(r.Type, func(*Candidate, *compliancepb.Rule) *compliancepb.Violation { called = true; return nil })
			res := NewEngine(registry).Evaluate(context.Background(), &Candidate{Book: twoEqualBook(), AsOf: t0}, mandate(r))
			if called != valid {
				t.Fatalf("evaluator called=%v, domain-valid=%v", called, valid)
			}
			if !valid && res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
				t.Fatalf("invalid domain downgraded to %v", res)
			}
		})
	}
	// Existing contractual defaults remain available: no buying-power floor means zero.
	r := &compliancepb.Rule{RuleId: "bp", Type: compliancepb.RuleType_RULE_TYPE_BUYING_POWER, Params: &compliancepb.Rule_BuyingPower{BuyingPower: &compliancepb.BuyingPowerLimit{}}}
	if err := ValidateMandate(mandate(r)); err != nil {
		t.Fatalf("existing default floor refused: %v", err)
	}
	// Economic WARN remains WARN after readable parameters reach the normal rule.
	r = concentrationRule(40)
	r.OnViolation = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN
	if got := NewEngine(nil).Evaluate(context.Background(), &Candidate{Book: twoEqualBook(), AsOf: t0}, mandate(r)).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN {
		t.Fatalf("readable economic WARN changed: %v", got)
	}
}

func TestMandateRetainedSetContractPreflightIsAtomic(t *testing.T) {
	reg := NewMandateRegistry()
	original := concentrationMandate(60)
	mustPut(t, reg, original)
	good := proto.Clone(original).(*compliancepb.Mandate)
	good.Version = 2
	bad := proto.Clone(original).(*compliancepb.Mandate)
	bad.Version = 3
	bad.Rules[0].Params = nil
	value, err := MarshalMandateSetValue([]*compliancepb.Mandate{good, bad})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewMandateLoader(reg).Apply(&lifecyclepb.ConfigChanged{ConfigKey: MandateConfigKey("t1", "p1"), NewValue: value})
	if err == nil {
		t.Fatal("poisoned retained set accepted")
	}
	got, ok, lookupErr := reg.Mandate(context.Background(), "t1", "p1", t0)
	if lookupErr != nil || ok.NoMandate() || got.Version != original.Version {
		t.Fatalf("valid member partially applied before poison: got=%v err=%v", got, lookupErr)
	}
}

func TestMandateAllParameterDecimalsAreBounded(t *testing.T) {
	fields := map[string]string{"concentration": "max_weight", "leverage_cap": "max_gross_leverage", "buying_power": "min_cash_after", "risk_measure": "max_value", "venue_margin": "min_margin_ratio", "cash_buffer": "max_idle_cash"}
	for _, r := range contractRules(t) {
		name, ok := fields[r.RuleId]
		if !ok {
			continue
		}
		for _, coeff := range []int64{0, 1} {
			for _, exp := range []int32{-65, -64, 64, 65} {
				t.Run(fmt.Sprintf("%s/%d/%d", r.RuleId, coeff, exp), func(t *testing.T) {
					clone := proto.Clone(r).(*compliancepb.Rule)
					wire := clone.ProtoReflect()
					field := wire.WhichOneof(wire.Descriptor().Oneofs().ByName("params"))
					params := wire.Mutable(field).Message()
					target := params.Descriptor().Fields().ByName(protoreflect.Name(name))
					if target == nil {
						t.Fatal("missing Decimal fixture field")
					}
					params.Set(target, protoreflect.ValueOfMessage((&commonpb.Decimal{Coefficient: coeff, Exponent: exp}).ProtoReflect()))
					valid := exp >= -64 && exp <= 64
					if err := ValidateMandate(mandate(clone)); (err == nil) != valid {
						t.Fatalf("domain accepted=%v want=%v: %v", err == nil, valid, err)
					}
					called := false
					registry := DefaultRegistry()
					registry.Register(clone.Type, func(*Candidate, *compliancepb.Rule) *compliancepb.Violation { called = true; return nil })
					res := NewEngine(registry).Evaluate(context.Background(), &Candidate{Book: twoEqualBook(), AsOf: t0}, mandate(clone))
					if called != valid || (!valid && res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH) {
						t.Fatalf("unreadable decimal reached evaluation or escaped hard refusal: called=%v result=%v", called, res)
					}
				})
			}
		}
	}
}
func TestGateRecordsDenialForUnreadableRuleParams(t *testing.T) {
	m := concentrationMandate(60)
	m.Rules[0].Params = nil
	m.Rules[0].OnViolation = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN
	reg := NewMandateRegistry()
	mustPut(t, reg, m)
	rec := &recordingRecorder{}
	gate := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, rec, nil)
	decision, err := gate.Evaluate(context.Background(), OrderDelta{TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL", SignedQuantity: dec(1, 0), Price: dec(1000, 0), Currency: "USD", OrderID: "unreadable-rule", AsOf: t0})
	if err != nil || decision.Allowed || len(rec.records) != 1 || rec.records[0].Allowed || rec.records[0].Result.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("unreadable rule escaped durable denial: decision=%+v err=%v records=%+v", decision, err, rec.records)
	}
}
