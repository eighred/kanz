package consume

import (
	"fmt"

	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/wealth"
)

// DecodeProto is the wealth.v1.HouseholdValued Decoder. Unlike alternatives,
// which needs a factory keyed on subject because four subjects each carry a
// different message, wealth has exactly one subject (SubjectHouseholdAll) and
// one message type — so this is a single Decoder, not a factory.
//
// It replaces DecodeJSON on the live path. DecodeJSON decodes the Go-native
// wealth.Household as JSON, which is not what a HouseholdValued FACT carries —
// it was a placeholder for exactly this, and consume.go's own doc says so.
//
// Exact Decimal values remain exact through aggregation and durable storage.
func DecodeProto(payload []byte) (wealth.Household, error) {
	if len(payload) > 8<<20 {
		return wealth.Household{}, wealth.ErrValuation
	}
	var m wealthpb.HouseholdValued
	if err := proto.Unmarshal(payload, &m); err != nil {
		return wealth.Household{}, fmt.Errorf("consume: HouseholdValued decode: %w", err)
	}

	accounts := make([]wealth.Account, 0, len(m.GetAccounts()))
	for _, va := range m.GetAccounts() {
		cash, ok := dec.FromProtoChecked(va.GetCash())
		if !ok || va.GetCash() == nil {
			return wealth.Household{}, fmt.Errorf(
				"consume: household %s account %s carries a cash balance this platform "+
					"cannot represent exactly; refusing rather than rounding it",
				m.GetHouseholdId(), va.GetAccountId())
		}
		cashF, err := dec.ExactFromRat(cash)
		if err != nil {
			return wealth.Household{}, err
		}

		holdings := make([]wealth.Holding, 0, len(va.GetHoldings()))
		for _, vh := range va.GetHoldings() {
			mv, ok := dec.FromProtoChecked(vh.GetMarketValue())
			if !ok || va.GetCash() == nil {
				return wealth.Household{}, fmt.Errorf(
					"consume: household %s account %s holding %s carries a market value "+
						"this platform cannot represent exactly; refusing rather than "+
						"rounding it",
					m.GetHouseholdId(), va.GetAccountId(), vh.GetInstrumentId())
			}
			if vh.GetMarketValue() == nil {
				return wealth.Household{}, wealth.ErrValuation
			}
			mvF, err := dec.ExactFromRat(mv)
			if err != nil {
				return wealth.Household{}, err
			}
			holdings = append(holdings, wealth.Holding{
				InstrumentID: vh.GetInstrumentId(),
				AssetClass:   vh.GetAssetClass(),
				MarketValue:  mvF,
			})
		}

		accounts = append(accounts, wealth.Account{
			AccountID: va.GetAccountId(),
			Holdings:  holdings,
			Cash:      cashF,
		})
	}

	profile, ok := domainProfile(m.GetRiskProfile())
	if !ok {
		// A profile this build does not know is REFUSED, not coerced. The
		// alternative is a numeric cast, which would map an unknown wire value
		// onto a domain profile no model serves — the household would then read
		// as "no target allocation" forever, which is indistinguishable from a
		// firm that has published none. A DLQ'd valuation is visible; that is not.
		return wealth.Household{}, fmt.Errorf(
			"consume: household %s carries risk_profile %v, which this build does not know; "+
				"refusing rather than folding a household whose target allocation cannot be selected",
			m.GetHouseholdId(), m.GetRiskProfile())
	}

	if m.GetAsOf() == nil || m.GetAsOf().CheckValid() != nil {
		return wealth.Household{}, wealth.ErrValuation
	}
	h := wealth.Household{
		HouseholdID:  m.GetHouseholdId(),
		Accounts:     accounts,
		RiskProfile:  profile,
		CurrencyCode: m.GetCurrencyCode(),
		AsOf:         m.GetAsOf().AsTime(),
		RecordedBy:   m.GetRecordedBy(),
		Reason:       m.GetReason(),
	}
	return h, h.ValidateValuation()
}

// domainProfile maps the wire risk profile onto the domain one. It is an EXPLICIT
// SWITCH rather than wealth.RiskProfile(v), because the two enums agreeing today
// is a fact about today: a value added to wealth.v1.RiskProfile and not to
// internal/wealth would be cast to a domain profile that no model can serve, and
// the household would silently report no target allocation rather than failing.
// ok=false is the UNKNOWN third value the callers turn into a refusal (#1010).
func domainProfile(p wealthpb.RiskProfile) (wealth.RiskProfile, bool) {
	switch p {
	case wealthpb.RiskProfile_RISK_PROFILE_UNSPECIFIED:
		return wealth.ProfileUnspecified, true
	case wealthpb.RiskProfile_RISK_PROFILE_CONSERVATIVE:
		return wealth.ProfileConservative, true
	case wealthpb.RiskProfile_RISK_PROFILE_MODERATE:
		return wealth.ProfileModerate, true
	case wealthpb.RiskProfile_RISK_PROFILE_BALANCED:
		return wealth.ProfileBalanced, true
	case wealthpb.RiskProfile_RISK_PROFILE_GROWTH:
		return wealth.ProfileGrowth, true
	case wealthpb.RiskProfile_RISK_PROFILE_AGGRESSIVE:
		return wealth.ProfileAggressive, true
	default:
		return wealth.ProfileUnspecified, false
	}
}

// DecodeModelProto preserves exact policy inputs. Legacy policy must be republished.
func DecodeModelProto(payload []byte) (wealth.ModelPortfolio, error) {
	if len(payload) > 2<<20 {
		return wealth.ModelPortfolio{}, wealth.ErrValuation
	}
	var m wealthpb.ModelPortfolio
	if err := proto.Unmarshal(payload, &m); err != nil {
		return wealth.ModelPortfolio{}, fmt.Errorf("consume: ModelPortfolio decode: %w", err)
	}
	profile, ok := domainProfile(m.GetRiskProfile())
	if !ok {
		return wealth.ModelPortfolio{}, fmt.Errorf(
			"consume: model %s carries risk_profile %v, which this build does not know; refusing rather "+
				"than admitting a model no household can be matched to",
			m.GetModelId(), m.GetRiskProfile())
	}
	targets := make(map[string]wealth.Number, len(m.GetExactTargetWeights()))
	for id, w := range m.GetExactTargetWeights() {
		targets[id] = wealth.Number(w)
	}
	return wealth.ModelPortfolio{
		ModelID:   m.GetModelId(),
		Profile:   profile,
		Targets:   targets,
		Tolerance: wealth.Number(m.GetExactDriftTolerance()), LegacyPrecision: m.GetArithmeticVersion() != 2 || len(m.GetTargetWeights()) != 0 || m.GetDriftTolerance() != 0,
		RecordedBy: m.GetRecordedBy(),
		Reason:     m.GetReason(),
	}, nil
}
