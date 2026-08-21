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
	// Risk answers what the RISK ENGINE has computed for this portfolio, by
	// measure name, or ok=false when it is UNKNOWN (#438).
	//
	// A FUNCTION RATHER THAN A MAP, because "unknown" here has three causes that
	// must stay one answer: never announced, absent from the last announcement,
	// or too old to be current. A map would make the third invisible — a stale
	// VaR reads exactly like a fresh one — and the freshness bound is the whole
	// reason this gate can be trusted while the risk engine is degraded.
	//
	// NIL IS THE COMMON CASE TODAY AND THAT IS SAFE, for the same reason Cash's
	// nil is: the rule only runs when a mandate DECLARES a risk limit, and
	// nothing declares one. A portfolio with no such rule is unaffected; one that
	// declares it and cannot supply the measure is REFUSED rather than admitted.
	Risk func(measure string) (*big.Rat, bool)
	// NAV is net asset value (positions + cash), the leverage-rule denominator.
	// nil when unknown, which fails the leverage rule closed.
	NAV *commonpb.Money
	// Cash is UNINVESTED CASH in BaseCurrency — what the portfolio can actually
	// spend (#415). nil when unknown, which fails the buying-power rule CLOSED,
	// exactly as an absent NAV fails the leverage rule closed.
	//
	// NIL IS THE COMMON CASE TODAY AND THAT IS SAFE, because the rule only runs
	// when a mandate DECLARES a buying-power limit. A portfolio with no such rule
	// is unaffected; one that declares it and cannot supply cash is refused rather
	// than admitted. The alternative — treating unknown cash as unlimited — is a
	// control that reports success, which is the failure mode this platform
	// designs against.
	//
	// It is populated from domain.v1.PortfolioState.cash_balance. risk sets that
	// field; the OMS position book does not yet (its Snapshot says NAV is "a
	// funded-book proxy until a cash/equity source lands"), so a deployment fed by
	// the OMS book supplies nil until that link is wired.
	Cash *commonpb.Money
	// CashCompleteness is what the PRODUCER of Cash said that number contains
	// (#614). nil means it said nothing.
	//
	// A BALANCE IS ONLY AS COMPLETE AS ITS INPUTS. accounting's ledger folds six
	// kinds of journal entry and two of them have no producer anywhere in this
	// platform (#588): no dividend, coupon or merger cash has ever reached the
	// book, and no accrual has. Cash is therefore not the portfolio's money — it
	// is the portfolio's money as far as the feeds that exist can see. Without
	// this field a rule reading Cash cannot tell that from a whole number, and
	// BuyingPowerRule turns the gap into a refusal that reads exactly like a
	// spending limit being hit.
	//
	// IT DOES NOT CHANGE ANY VERDICT, and that is deliberate. The sign of the
	// error is not knowable: an unfolded dividend understates the cash of a book
	// that is LONG the instrument and OVERSTATES the cash of one that is SHORT it
	// (accounting's foldCorpAct pays quantity x per-unit, signed). Correcting the
	// number by this list would be guessing, and inflating buying power on a guess
	// turns a control that refuses too much into one that admits what the fund
	// cannot pay for. So it is carried into the EVIDENCE of a refusal and nowhere
	// else: the refusal stands, and it says what it could not account for.
	CashCompleteness *CashCompleteness
	Positions        []Position
}

// CashCompleteness is a producer's statement about the balance it published —
// which kinds of journal entry the deployment that computed it actually feeds
// (#614).
//
// nil IS NOT "COMPLETE", IT IS "UNSTATED". A producer that says nothing has not
// shown its number to be whole, and the two must not collapse into one answer;
// that collapse is the whole defect this type exists to end. Only a non-nil
// value with an empty OmittedEntryTypes means "everything this book folds is
// fed".
type CashCompleteness struct {
	// OmittedEntryTypes names the kinds of journal entry the book of record can
	// fold and NOTHING in the producing deployment produces, by the book's own
	// entry-type names ("corporate_action", "accrual"). Empty means the producer
	// stated that nothing is missing.
	OmittedEntryTypes []string
}

// Incomplete reports whether the producer named something missing from the
// balance. A nil receiver is NOT incomplete — it is unstated, which Stated
// answers; a caller that needs to tell them apart must ask both.
func (c *CashCompleteness) Incomplete() bool { return c != nil && len(c.OmittedEntryTypes) > 0 }

// Stated reports whether the producer said anything at all about completeness.
func (c *CashCompleteness) Stated() bool { return c != nil }

// Vouched reports whether the producer stated that the balance is WHOLE — it said
// something, and what it said names nothing missing.
//
// THREE STATES COLLAPSE TO TWO HERE, DELIBERATELY, and only for the caller that
// asks "may I rely on this number". Unstated and incomplete are different facts
// with different operator actions — nobody wired the statement, versus a feed that
// does not exist — and Stated/Incomplete keep them apart for the reader that needs
// them. What they share is the only thing this predicate is about: the producer
// did not vouch for the figure, so a control that spends against it is spending
// against a number no one stands behind.
func (c *CashCompleteness) Vouched() bool { return c.Stated() && !c.Incomplete() }

// Omitted returns the entry types the producer named as missing, nil-safe.
func (c *CashCompleteness) Omitted() []string {
	if c == nil {
		return nil
	}
	return c.OmittedEntryTypes
}

// BookFromSnapshot builds a Book from a domain.v1.PortfolioSnapshot — the
// bootstrap shape both the gate's book source and the monitor consume.
func BookFromSnapshot(s *domainpb.PortfolioSnapshot) *Book {
	b := &Book{
		PortfolioID:  s.GetPortfolio().GetPortfolioId(),
		BaseCurrency: s.GetPortfolio().GetBaseCurrency(),
		NAV:          s.GetPortfolio().GetTotalMarketValue(),
		Cash:         s.GetPortfolio().GetCashBalance(),
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
//
// NO PRODUCTION IMPLEMENTATION EXISTS, and a nil Classifier is what every
// composition root passes today. That is SAFE because the three rules that need
// one — ConcentrationRule, RestrictionRule and IssuerExclusionRule, on the
// ISSUER, SECTOR and ASSET_CLASS dimensions — now REFUSE rather than pass; see
// unresolvedDimension in rules.go. It was not safe before: an unresolvable
// dimension put every holding in the empty bucket, and a sector cap approved a
// book that was entirely in the capped sector (#640).
type Classifier interface {
	Classify(ctx context.Context, instrumentID string, asOf time.Time) (Attributes, bool)
}

// StaticClassifier is an in-memory Classifier backed by a fixed map.
//
// TESTS ARE ITS ONLY CONSTRUCTOR, and this doc used to claim otherwise — "the
// composition root loads it from reference data" was written as a description of
// an estate that has never had one (#640). The reference-data source it would be
// loaded from does not exist: reference.v1.InstrumentReference is a schema whose
// only builder, datamaster's feed.NormalizeReference, is called from tests
// alone, is never published or persisted, and carries no issuer at all. Filling
// this map with invented sectors to make the seam non-nil would turn a control
// that silently passes into one that is confidently wrong, which #345 rules out.
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

	// Order is the order this evaluation is ABOUT, when there is one. nil for the
	// post-trade monitor, which re-evaluates a live book with no order in hand.
	//
	// EVERY RULE BUT ONE IGNORES IT, deliberately. The pre-trade check is
	// hypothetical — it projects the post-trade book and asks whether THAT book
	// is inside the mandate — and reading the order instead of the projection is
	// how a rule ends up bounding the trade rather than the position. What the
	// projection cannot express is DIRECTION: a book at 3x leverage looks the same
	// whether the order took it there or brought it down from 4x, and a margin
	// control has to tell those apart or it blocks the fund from de-risking. See
	// VenueMarginRule.
	//
	// A rule reading this must behave when it is nil, and must fail CLOSED there
	// if the answer depends on it.
	Order *CandidateOrder

	// Margin answers what the EXCHANGE reported about the margin state of the
	// venue account this candidate's portfolio trades from, or ok=false when it is
	// UNKNOWN (#408, control 3).
	//
	// A FUNCTION AND NOT A VALUE, for the reason Book.Risk is one: "unknown" has
	// several causes — never observed, observed without the figure, observed too
	// long ago, no account bound — and they must stay ONE answer. Resolving it
	// eagerly would also mean resolving it for every order under every mandate,
	// including the portfolios that do not trade on margin at all.
	//
	// NIL MEANS NOTHING OBSERVES MARGIN HERE, which VenueMarginRule refuses on. It
	// is the common case and it is safe, exactly as a nil Book.Risk is: the rule
	// only runs when a mandate DECLARES margin trading.
	Margin func(venue string) (MarginState, bool)
}

// CandidateOrder is the order under evaluation, in the terms a rule needs it —
// which is deliberately not the whole order.
//
// SignedQuantity is +buy/−sell, the same delta project() applied to the book, so
// a rule can recover the PRE-trade quantity by subtracting it from the projected
// one. Venue is where the order will execute, which is what decides WHICH
// exchange account's collateral is at stake.
type CandidateOrder struct {
	InstrumentID   string
	SignedQuantity *commonpb.Decimal
	Venue          string
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
	r.Register(compliancepb.RuleType_RULE_TYPE_BUYING_POWER, BuyingPowerRule)
	r.Register(compliancepb.RuleType_RULE_TYPE_RISK_MEASURE, RiskLimitRule)
	r.Register(compliancepb.RuleType_RULE_TYPE_VENUE_MARGIN, VenueMarginRule)
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
