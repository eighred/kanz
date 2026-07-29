package sustainability

import (
	"context"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

	"github.com/eighred/kanz/internal/compliance"
)

// ALT/CLIMATE-01b exclusion screening — GENUINE COMP-01 reuse, not a re-coded
// screen. An ESG exclusion ("hold nothing in coal/tobacco/controversial-weapons
// sectors, and nothing in these named issuers") is exactly a COMP-01 RESTRICTION
// (deny-list) plus an ISSUER_EXCLUSION rule. Rather than re-implement deny-by-
// default screening, this builds those rules into a compliance.Mandate and runs
// the SAME compliance.Engine the pre-trade gate and post-trade monitor use — one
// source of truth for "a holding you may not own", so an ESG exclusion and a
// mandate restriction can never disagree. compliance lives outside internal/risk,
// so importing it crosses no RISK-02 boundary.

// ExclusionPolicy is an ESG exclusion list: sectors and issuers the portfolio may
// not hold (e.g. sectors "COAL","TOBACCO"; issuers of controversial-weapons
// makers). It is translated to COMP-01 rules by Screen.
type ExclusionPolicy struct {
	// ExcludedSectors are sector codes (in the classifier's taxonomy) the
	// portfolio may not hold.
	ExcludedSectors []string
	// ExcludedIssuers are issuer ids the portfolio may not hold.
	ExcludedIssuers []string
}

// Empty reports whether the policy excludes nothing.
func (p ExclusionPolicy) Empty() bool {
	return len(p.ExcludedSectors) == 0 && len(p.ExcludedIssuers) == 0
}

// Mandate builds the COMP-01 mandate that enforces the exclusion policy: a SECTOR
// deny-list restriction (when sectors are excluded) and an issuer-exclusion rule
// (when issuers are excluded). The portfolio id stamps the result.
func (p ExclusionPolicy) Mandate(portfolioID string) *compliancepb.Mandate {
	m := &compliancepb.Mandate{MandateId: "esg-exclusion", PortfolioId: portfolioID, Version: 1}
	if len(p.ExcludedSectors) > 0 {
		m.Rules = append(m.Rules, &compliancepb.Rule{
			RuleId: "esg-sector-exclusion",
			Type:   compliancepb.RuleType_RULE_TYPE_RESTRICTION,
			Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
				Dimension: compliancepb.Dimension_DIMENSION_SECTOR,
				Values:    p.ExcludedSectors,
				Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
			}},
		})
	}
	if len(p.ExcludedIssuers) > 0 {
		m.Rules = append(m.Rules, &compliancepb.Rule{
			RuleId: "esg-issuer-exclusion",
			Type:   compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
			Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
				IssuerIds: p.ExcludedIssuers,
			}},
		})
	}
	return m
}

// Screen evaluates a portfolio book against an ESG exclusion policy through the
// COMP-01 engine and returns the compliance result: BREACH (with per-rule
// violations naming the offending instrument) when the book holds an excluded
// sector/issuer, PASS otherwise. book and classifier are the same shapes the
// COMP-01 pre-trade gate uses, so a caller already holding a portfolio snapshot
// passes it straight through. An empty policy passes trivially.
func Screen(ctx context.Context, book *compliance.Book, classifier compliance.Classifier, policy ExclusionPolicy, asOf time.Time) *compliancepb.ComplianceResult {
	mandate := policy.Mandate(book.PortfolioID)
	cand := &compliance.Candidate{Book: book, Classifier: classifier, AsOf: asOf}
	return compliance.NewEngine(nil).Evaluate(ctx, cand, mandate)
}
