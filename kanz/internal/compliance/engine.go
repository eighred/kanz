// Package compliance is the COMP-01 mandate/rule engine — the pure core that
// evaluates a candidate portfolio book against a tenant/portfolio mandate and
// returns a compliance.v1.ComplianceResult. It is the shared brain behind both
// enforcement points: the pre-trade gate the OMS calls before admitting an
// order (gate.go, consumed via OMS-01f) and the post-trade monitor that
// re-evaluates live books (the compliance service, COMP-01d).
//
// # Shape mirrors the RISK-07 measure registry
//
// Rules are pure functions in a registry keyed by RuleType, exactly as
// compute.MeasureFunc is keyed by MeasureName: adding a rule type is one
// Register call, the contract never changes, and tests substitute deterministic
// implementations through the same hook.
//
// # Own book model, by the RISK-02 boundary
//
// The pre-trade check needs portfolio exposures, and RISK-06's
// compute.ComputeExposure already computes them — but that lives under
// kanz/internal/risk, which the RISK-02 arch test makes private to the risk
// module (outsiders may import only risk/api/v*). So compliance builds its own
// Book from the SHARED domain.v1.PositionState wire schema and reimplements the
// gross-exposure fold here (gross = Σ|market value|, the same definition
// ComputeExposure uses). The duplication is the cost of not coupling compliance
// to risk internals — the boundary the contract design exists to protect.
//
// # Deny-by-default
//
// An unknown RuleType, a rule whose params do not match its type, or a rule the
// engine cannot evaluate resolves to BREACH, never PASS. A misconfigured
// mandate fails closed.
//
// # Currency / FX
//
// Concentration and leverage are ratios; numerator and denominator must share a
// currency. Like risk (RISK-06) this layer does no FX conversion — it sums raw
// market-value amounts and is correct for a single-currency book. The
// CurrencyRestriction rule is the v1 guard that keeps a book single-currency.
package compliance

import (
	"context"
	"math/big"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Position is one holding in the candidate book — the compliance-local working
// shape, built from a domain.v1.PositionState.
type Position struct {
	InstrumentID string
	Quantity     *commonpb.Decimal
	MarketValue  *commonpb.Money
}

// Book is the portfolio state a mandate is evaluated against — built from the
// shared domain.v1 wire schema, independent of the risk module's internal types
// (RISK-02). It is the post-trade projection for the pre-trade gate and the
// live book for the post-trade monitor.
type Book struct {
	PortfolioID  string
	BaseCurrency string
	// NAV is net asset value (positions + cash), the leverage-rule denominator.
	// nil when unknown, which fails the leverage rule closed.
	NAV       *commonpb.Money
	Positions []Position
}

// BookFromSnapshot builds a Book from a domain.v1.PortfolioSnapshot — the
// bootstrap shape both the gate's book source and the monitor consume.
func BookFromSnapshot(s *domainpb.PortfolioSnapshot) *Book {
	b := &Book{
		PortfolioID:  s.GetPortfolio().GetPortfolioId(),
		BaseCurrency: s.GetPortfolio().GetBaseCurrency(),
		NAV:          s.GetPortfolio().GetTotalMarketValue(),
	}
	for _, ps := range s.GetPositions() {
		b.Positions = append(b.Positions, Position{
			InstrumentID: ps.GetInstrumentId(),
			Quantity:     ps.GetQuantity(),
			MarketValue:  ps.GetMarketValue(),
		})
	}
	return b
}

// clone returns a deep-enough copy: the positions slice is fresh (so projection
// can mutate it) while the proto pointer fields are treated as immutable.
func (b *Book) clone() *Book {
	cp := *b
	cp.Positions = make([]Position, len(b.Positions))
	copy(cp.Positions, b.Positions)
	return &cp
}

// Attributes is the reference data a rule needs to bucket a position on a
// dimension beyond what the position carries — issuer, sector, asset class.
// Currency comes from the position's MarketValue, so it is not here.
type Attributes struct {
	Issuer     string
	Sector     string
	AssetClass string
}

// Classifier resolves an instrument's classification as of a point in time —
// the compliance mirror of factor.Classifier (point-in-time so a historical
// re-evaluation reads the mapping in effect then). ok=false ⇒ unknown
// instrument: the engine buckets it under the empty key rather than dropping it.
type Classifier interface {
	Classify(ctx context.Context, instrumentID string, asOf time.Time) (Attributes, bool)
}

// StaticClassifier is an in-memory Classifier backed by a fixed map. The
// composition root loads it from reference data; tests construct it directly.
type StaticClassifier map[string]Attributes

// Classify implements Classifier; asOf is ignored (a single snapshot).
func (c StaticClassifier) Classify(_ context.Context, instrumentID string, _ time.Time) (Attributes, bool) {
	a, ok := c[instrumentID]
	return a, ok
}

var _ Classifier = StaticClassifier(nil)

// Candidate is the book under evaluation plus the classifier resolving its
// dimensions and the evaluation time.
type Candidate struct {
	Book       *Book
	Classifier Classifier
	AsOf       time.Time
}

// RuleFunc evaluates one rule against a candidate book. It returns nil when the
// rule is satisfied, or a *Violation (Message + Evidence) describing the
// failure. Pure — no I/O, no state. The engine stamps rule_id/type/severity.
type RuleFunc func(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation

// Registry is the catalog of rule evaluators keyed by RuleType.
type Registry struct {
	funcs map[compliancepb.RuleType]RuleFunc
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{funcs: make(map[compliancepb.RuleType]RuleFunc)}
}

// Register adds or replaces the evaluator for a rule type.
func (r *Registry) Register(t compliancepb.RuleType, fn RuleFunc) { r.funcs[t] = fn }

// DefaultRegistry returns the COMP-01 baseline: one evaluator per concrete rule
// type. Deployments override by constructing this and re-Registering.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(compliancepb.RuleType_RULE_TYPE_CONCENTRATION, ConcentrationRule)
	r.Register(compliancepb.RuleType_RULE_TYPE_RESTRICTION, RestrictionRule)
	r.Register(compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION, IssuerExclusionRule)
	r.Register(compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE, LeverageRule)
	r.Register(compliancepb.RuleType_RULE_TYPE_CURRENCY, CurrencyRule)
	return r
}

// Engine evaluates candidate books against mandates using a rule registry.
type Engine struct {
	registry *Registry
}

// NewEngine wraps a registry (DefaultRegistry when nil).
func NewEngine(r *Registry) *Engine {
	if r == nil {
		r = DefaultRegistry()
	}
	return &Engine{registry: r}
}

// Evaluate runs every rule in the mandate against the candidate and returns the
// aggregate result. status is the worst severity across rules
// (PASS < WARN < BREACH). An unknown rule type is denied by default: it
// produces a BREACH violation rather than being skipped.
func (e *Engine) Evaluate(ctx context.Context, c *Candidate, m *compliancepb.Mandate) *compliancepb.ComplianceResult {
	_ = ctx
	res := &compliancepb.ComplianceResult{
		Status:         compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS,
		PortfolioId:    m.GetPortfolioId(),
		MandateId:      m.GetMandateId(),
		MandateVersion: m.GetVersion(),
		EvaluatedAt:    timestamppb.New(c.AsOf),
	}
	for _, rule := range m.GetRules() {
		fn, ok := e.registry.funcs[rule.GetType()]
		if !ok {
			res.Violations = append(res.Violations, &compliancepb.Violation{
				RuleId:   rule.GetRuleId(),
				RuleType: rule.GetType(),
				Severity: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH,
				Message:  "unknown rule type — denied by default",
				Evidence: map[string]string{"rule_type": rule.GetType().String()},
			})
			res.Status = worst(res.Status, compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH)
			continue
		}
		v := fn(c, rule)
		if v == nil {
			continue
		}
		v.RuleId = rule.GetRuleId()
		v.RuleType = rule.GetType()
		v.Severity = violationSeverity(rule)
		res.Violations = append(res.Violations, v)
		res.Status = worst(res.Status, v.Severity)
	}
	return res
}

// violationSeverity is the rule's configured on_violation, defaulting to BREACH
// (deny-by-default) when unspecified.
func violationSeverity(rule *compliancepb.Rule) compliancepb.ComplianceStatus {
	s := rule.GetOnViolation()
	if s == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED {
		return compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
	}
	return s
}

// worst returns the more severe of two statuses (PASS < WARN < BREACH).
func worst(a, b compliancepb.ComplianceStatus) compliancepb.ComplianceStatus {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func rank(s compliancepb.ComplianceStatus) int {
	switch s {
	case compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH:
		return 3
	case compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN:
		return 2
	case compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS:
		return 1
	default:
		return 0
	}
}

// --- exact decimal/ratio helpers -------------------------------------------
//
// Concentration and leverage compare ratios of money amounts; doing that in
// float would reintroduce the rounding the Decimal type exists to avoid. We
// lift amounts to math/big.Rat (exact) and compare there.

// ratFromDecimal converts a Decimal (coefficient × 10^exponent) to an exact Rat.
func ratFromDecimal(d *commonpb.Decimal) *big.Rat {
	r := new(big.Rat)
	if d == nil {
		return r
	}
	r.SetInt64(d.GetCoefficient())
	exp := d.GetExponent()
	if exp == 0 {
		return r
	}
	n := int64(exp)
	if n < 0 {
		n = -n
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil))
	if exp > 0 {
		return r.Mul(r, scale)
	}
	return r.Quo(r, scale)
}

// absRatFromMoney returns the absolute amount of a Money as an exact Rat.
func absRatFromMoney(m *commonpb.Money) *big.Rat {
	r := ratFromDecimal(m.GetAmount())
	return r.Abs(r)
}

// ratString renders a Rat as a fixed-point decimal string for evidence.
func ratString(r *big.Rat) string { return r.FloatString(6) }
