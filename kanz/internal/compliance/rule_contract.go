package compliance

import (
	"fmt"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/reflect/protoreflect"

	decimal "github.com/eighred/kanz/internal/dec"
)

type ruleContract struct {
	params   protoreflect.Name
	evaluate RuleFunc
}

// Keep baseline dispatch and its wire parameter contract together. A rule that
// cannot be read is not an economic soft-limit breach and cannot inherit WARN.
var baselineRuleContracts = map[compliancepb.RuleType]ruleContract{
	compliancepb.RuleType_RULE_TYPE_CONCENTRATION:    {"concentration", ConcentrationRule},
	compliancepb.RuleType_RULE_TYPE_RESTRICTION:      {"restriction", RestrictionRule},
	compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION: {"issuer_exclusion", IssuerExclusionRule},
	compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE:   {"leverage_cap", LeverageRule},
	compliancepb.RuleType_RULE_TYPE_CURRENCY:         {"currency_restriction", CurrencyRule},
	compliancepb.RuleType_RULE_TYPE_BUYING_POWER:     {"buying_power", BuyingPowerRule},
	compliancepb.RuleType_RULE_TYPE_RISK_MEASURE:     {"risk_measure", RiskLimitRule},
	compliancepb.RuleType_RULE_TYPE_VENUE_MARGIN:     {"venue_margin", VenueMarginRule},
	compliancepb.RuleType_RULE_TYPE_CASH_BUFFER:      {"cash_buffer", CashBufferRule},
}

func validateRuleContract(rule *compliancepb.Rule) error {
	contract, known := baselineRuleContracts[rule.GetType()]
	if !known {
		return fmt.Errorf("unsupported rule type %d", rule.GetType())
	}
	wire := rule.ProtoReflect()
	field := wire.Descriptor().Fields().ByName(contract.params)
	if field == nil || !wire.Has(field) || !wire.Get(field).Message().IsValid() {
		return fmt.Errorf("rule params do not match rule type; expected %s", contract.params)
	}
	if path, valid := decimal.InDomainDeep(rule); !valid {
		return fmt.Errorf("rule Decimal outside supported computational domain at %s", path)
	}
	return nil
}

func validateMandateRuleContracts(m *compliancepb.Mandate) error {
	for _, rule := range m.GetRules() {
		if err := validateRuleContract(rule); err != nil {
			return fmt.Errorf("%w: rule %q: %w", ErrMandateIncomplete, rule.GetRuleId(), err)
		}
	}
	return nil
}
