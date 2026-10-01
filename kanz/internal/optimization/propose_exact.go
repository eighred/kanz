package optimization

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

const MaxProposalInstruments = 128

// TargetWeightPolicy describes a numerical recommendation, not observed holdings.
// The residual goes to the largest absolute weight, with instrument ID tie-breaking.
const TargetWeightPolicy = "decimal-12-nearest-even; budget-residual-largest-absolute-weight; exact-post-check"

var ErrExactProposal = errors.New("optimization: financial inputs or candidate book unavailable, unsupported or outside bounds")

type ExactProposal struct {
	ExactRebalanceProposal
	SchemaVersion      int
	ReadOnly           bool
	Currency           string
	TargetWeightPolicy string
	Objective          Objective
	ExpectedReturn     float64
	ExpectedRisk       *float64
	CovarianceQuality  CovarianceQuality
	Violations         []string
	EvaluatedWeights   map[string]dec.Exact
}

// FinancialInputs contain facts in one declared valuation currency. Map absence
// means zero holdings; an explicit empty value is unknown and must be refused.
type FinancialInputs struct {
	Current   map[string]dec.Exact
	NAV       dec.Exact
	Prices    map[string]dec.Exact
	Threshold dec.Exact
	Currency  string
}

func ProposeExact(ctx context.Context, portfolioID string, in MarketInputs, obj Objective, constraints *ExactConstraintSet, current FinancialInputs, classifier compliance.Classifier, engine *compliance.Engine, mandate *compliancepb.Mandate, asOf time.Time) (ExactProposal, error) {
	if err := ctx.Err(); err != nil {
		return ExactProposal{}, err
	}
	if err := ValidateProposalInputs(portfolioID, in, current); err != nil {
		return ExactProposal{}, err
	}
	solverConstraints, err := constraints.solver(in.Instruments)
	if err != nil {
		return ExactProposal{}, err
	}
	if math.IsNaN(obj.RiskAversion) || math.IsInf(obj.RiskAversion, 0) || obj.RiskAversion < 0 || math.IsNaN(obj.RiskFreeRate) || math.IsInf(obj.RiskFreeRate, 0) {
		return ExactProposal{}, ErrExactProposal
	}
	res, err := Optimize(in, obj, solverConstraints)
	if err != nil {
		return ExactProposal{}, err
	}
	weights, err := proposalWeights(res.Weights)
	if err != nil {
		return ExactProposal{}, err
	}
	proposal, err := RebalanceExact(portfolioID, current.Current, weights, current.NAV, current.Prices, current.Threshold, asOf)
	if err != nil {
		return ExactProposal{}, err
	}
	actual := make(map[string]dec.Exact, len(in.Instruments))
	for _, id := range in.Instruments {
		actual[id] = "0"
		if v, ok := current.Current[id]; ok {
			actual[id] = v
		}
	}
	for _, trade := range proposal.Trades {
		actual[trade.InstrumentID] = trade.TargetWeight
	}
	if err := constraints.check(ctx, proposal, classifier, asOf); err != nil {
		return ExactProposal{}, err
	}
	checked := proposal
	checked.Targets = actual
	if err := constraints.check(ctx, checked, classifier, asOf); err != nil {
		return ExactProposal{}, err
	}
	approximate := make([]float64, len(in.Instruments))
	for i, id := range in.Instruments {
		r, _ := actual[id].Rat()
		approximate[i], _ = r.Float64()
	}
	res.ExpectedReturn = 0
	if in.ExpectedReturns != nil {
		res.ExpectedReturn = dot(in.ExpectedReturns, approximate)
	}
	if res.ExpectedRisk != nil {
		risk := math.Sqrt(math.Max(quadForm(in.Covariance, approximate), 0))
		res.ExpectedRisk = &risk
	}
	if math.IsNaN(res.ExpectedReturn) || math.IsInf(res.ExpectedReturn, 0) || (res.ExpectedRisk != nil && (math.IsNaN(*res.ExpectedRisk) || math.IsInf(*res.ExpectedRisk, 0))) {
		return ExactProposal{}, ErrExactProposal
	}
	status, violations, err := CheckMandateExact(ctx, actual, current.NAV, current.Currency, classifier, engine, mandate, asOf)
	if err != nil {
		return ExactProposal{}, err
	}
	if err := ctx.Err(); err != nil {
		return ExactProposal{}, err
	}
	proposal.MandateStatus = status
	return ExactProposal{ExactRebalanceProposal: proposal, SchemaVersion: 2, ReadOnly: true, Currency: current.Currency, TargetWeightPolicy: TargetWeightPolicy, Objective: obj, ExpectedReturn: res.ExpectedReturn, ExpectedRisk: res.ExpectedRisk, CovarianceQuality: res.CovarianceQuality, Violations: violations, EvaluatedWeights: actual}, nil
}

func ValidateProposalInputs(portfolioID string, in MarketInputs, f FinancialInputs) error {
	if strings.TrimSpace(portfolioID) == "" || len(portfolioID) > 256 || len(in.Instruments) == 0 || len(in.Instruments) > MaxProposalInstruments || f.Current == nil || len(f.Current) > MaxProposalInstruments || len(f.Prices) > MaxProposalInstruments || len(f.Currency) < 3 || len(f.Currency) > 12 {
		return ErrExactProposal
	}
	for _, c := range f.Currency {
		if c < 'A' || c > 'Z' {
			return ErrExactProposal
		}
	}
	if in.Covariance != nil && (len(in.Covariance) != len(in.Instruments) || !square(in.Covariance, len(in.Instruments))) {
		return ErrInputsMismatch
	}
	if in.ExpectedReturns != nil && len(in.ExpectedReturns) != len(in.Instruments) {
		return ErrInputsMismatch
	}
	ids := make(map[string]bool, len(in.Instruments))
	for _, id := range in.Instruments {
		if strings.TrimSpace(id) == "" || len(id) > 256 || ids[id] {
			return ErrExactProposal
		}
		ids[id] = true
	}
	for _, values := range []map[string]dec.Exact{f.Current, f.Prices} {
		for id, value := range values {
			if !ids[id] {
				return ErrExactProposal
			}
			r, err := value.Rat()
			if err != nil || r.Sign() < 0 {
				return ErrExactProposal
			}
		}
	}
	for _, id := range in.Instruments {
		r, err := f.Prices[id].Rat()
		if err != nil || r.Sign() <= 0 {
			return ErrExactProposal
		}
	}
	for _, r := range in.ExpectedReturns {
		if math.IsNaN(r) || math.IsInf(r, 0) {
			return ErrExactProposal
		}
	}
	nav, err := f.NAV.Rat()
	if err != nil || nav.Sign() <= 0 {
		return ErrExactProposal
	}
	threshold, err := f.Threshold.Rat()
	if err != nil || threshold.Sign() < 0 || threshold.Cmp(big.NewRat(1, 1)) > 0 {
		return ErrExactProposal
	}
	sum := new(big.Rat)
	for _, v := range f.Current {
		r, _ := v.Rat()
		sum.Add(sum, r)
	}
	// This contract supports long-only fully invested portfolios, including an
	// empty initial book. Cash and leverage require their own explicit model.
	if sum.Sign() != 0 && sum.Cmp(big.NewRat(1, 1)) != 0 {
		return ErrExactProposal
	}
	return nil
}

func proposalWeights(source map[string]float64) (map[string]dec.Exact, error) {
	ids := make([]string, 0, len(source))
	for id := range source {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make(map[string]dec.Exact, len(source))
	sum := new(big.Rat)
	largest := ""
	largestValue := -1.0
	for _, id := range ids {
		v := source[id]
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return nil, ErrExactProposal
		}
		r, _ := new(big.Rat).SetString(strconv.FormatFloat(v, 'f', 12, 64))
		sum.Add(sum, r)
		out[id], _ = dec.ExactFromRat(r)
		if v > largestValue {
			largest, largestValue = id, v
		}
	}
	if largest == "" {
		return nil, ErrExactProposal
	}
	residual := new(big.Rat).Sub(big.NewRat(1, 1), sum)
	if new(big.Rat).Abs(residual).Cmp(big.NewRat(int64(len(ids)), 1_000_000_000_000)) > 0 {
		return nil, ErrExactProposal
	}
	r, _ := out[largest].Rat()
	r.Add(r, residual)
	if r.Sign() < 0 || r.Cmp(big.NewRat(1, 1)) > 0 {
		return nil, ErrExactProposal
	}
	out[largest], _ = dec.ExactFromRat(r)
	return out, nil
}

// CheckMandateExact never narrows a candidate amount to cents. A value outside
// canonical Decimal's exact representation is refused before any rule executes.
func CheckMandateExact(ctx context.Context, weights map[string]dec.Exact, nav dec.Exact, currency string, classifier compliance.Classifier, engine *compliance.Engine, mandate *compliancepb.Mandate, asOf time.Time) (MandateStatus, []string, error) {
	if err := ctx.Err(); err != nil {
		return MandateUnchecked, nil, err
	}
	if mandate == nil {
		return MandateUnchecked, nil, nil
	}
	n, err := nav.Rat()
	if err != nil || n.Sign() <= 0 {
		return MandateUnchecked, nil, ErrExactProposal
	}
	d, ok := dec.ToProtoExact(n)
	if !ok {
		return MandateUnchecked, nil, ErrExactProposal
	}
	book := &compliance.Book{PortfolioID: mandate.GetPortfolioId(), BaseCurrency: currency, NAV: &commonpb.Money{Amount: d, CurrencyCode: currency}, NAVBasis: compliance.NAVBasisEquity}
	ids := make([]string, 0, len(weights))
	for id := range weights {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w, err := weights[id].Rat()
		if err != nil {
			return MandateUnchecked, nil, ErrExactProposal
		}
		amount, ok := dec.ToProtoExact(new(big.Rat).Mul(w, n))
		if !ok {
			return MandateUnchecked, nil, ErrExactProposal
		}
		book.Positions = append(book.Positions, compliance.Position{InstrumentID: id, MarketValue: &commonpb.Money{Amount: amount, CurrencyCode: currency}})
	}
	if engine == nil {
		engine = compliance.NewEngine(nil)
	}
	result := engine.Evaluate(ctx, &compliance.Candidate{Book: book, Classifier: classifier, AsOf: asOf}, mandate)
	if err := ctx.Err(); err != nil {
		return MandateUnchecked, nil, err
	}
	if result.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS || result.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN {
		return MandateFeasible, nil, nil
	}
	messages := make([]string, 0, len(result.GetViolations()))
	for _, v := range result.GetViolations() {
		messages = append(messages, v.GetMessage())
	}
	return MandateInfeasible, messages, nil
}
