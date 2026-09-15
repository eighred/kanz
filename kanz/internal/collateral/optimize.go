package collateral

import (
	"context"
	"errors"
	"github.com/eighred/kanz/internal/dec"
	"math/big"
	"sort"
	"strings"
)

// Asset describes exact market value in one explicitly named currency. FX
// conversion and immutable valuation provenance belong to the calling domain.
type Asset struct {
	ID, Currency    string
	Available, Cost dec.Exact
}
type Eligibility struct {
	Eligible bool
	Haircut  dec.Exact
}
type Requirement struct {
	AgreementID, Currency string
	Amount                dec.Exact
	Schedule              map[string]Eligibility
}
type Allocation struct {
	AgreementID, AssetID         string
	UsedValue, PostedValue, Cost dec.Exact
}

// Certificate contains dual multipliers for an independently verifiable optimum
// or Farkas infeasibility witness. A resource refusal is never infeasibility.
type Certificate struct{ Capacity, Coverage map[string]dec.Exact }
type AllocationResult struct {
	Currency    string
	Feasible    bool
	Allocations []Allocation
	Cost        dec.Exact
	Certificate Certificate
}

var (
	ErrAllocationInput       = errors.New("collateral: invalid or unavailable allocation input")
	ErrAllocationLimit       = errors.New("collateral: exact allocation resource bound exceeded")
	ErrAllocationCertificate = errors.New("collateral: invalid allocation certificate")
)

const MaxAllocationAssets = 32
const MaxAllocationAgreements = 32
const MaxAllocationEdges = 512

type allocationEdge struct {
	asset, requirement int
	coverage, cost     *big.Rat
}
type allocationModel struct {
	assets          []Asset
	requirements    []Requirement
	available, need []*big.Rat
	edges           []allocationEdge
	currency        string
}

func allocationNumber(v dec.Exact) (*big.Rat, error) {
	r, err := v.Rat()
	if err != nil || r.Sign() < 0 {
		return nil, ErrAllocationInput
	}
	return r, nil
}
func buildAllocationModel(assets []Asset, requirements []Requirement) (*allocationModel, error) {
	if len(assets) > MaxAllocationAssets || len(requirements) > MaxAllocationAgreements {
		return nil, ErrAllocationLimit
	}
	m := &allocationModel{assets: append([]Asset(nil), assets...), requirements: append([]Requirement(nil), requirements...)}
	sort.Slice(m.assets, func(i, j int) bool { return m.assets[i].ID < m.assets[j].ID })
	sort.Slice(m.requirements, func(i, j int) bool { return m.requirements[i].AgreementID < m.requirements[j].AgreementID })
	checkCurrency := func(c string) bool {
		if len(c) != 3 || strings.IndexFunc(c, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 {
			return false
		}
		if m.currency == "" {
			m.currency = c
		}
		return m.currency == c
	}
	ids := map[string]int{}
	costs := make([]*big.Rat, len(m.assets))
	for i, a := range m.assets {
		if a.ID == "" || strings.TrimSpace(a.ID) != a.ID || len(a.ID) > 256 || !checkCurrency(a.Currency) {
			return nil, ErrAllocationInput
		}
		if _, ok := ids[a.ID]; ok {
			return nil, ErrAllocationInput
		}
		ids[a.ID] = i
		v, err := allocationNumber(a.Available)
		if err != nil {
			return nil, err
		}
		m.available = append(m.available, v)
		costs[i], err = allocationNumber(a.Cost)
		if err != nil {
			return nil, err
		}
	}
	for i, r := range m.requirements {
		if r.AgreementID == "" || strings.TrimSpace(r.AgreementID) != r.AgreementID || len(r.AgreementID) > 256 || !checkCurrency(r.Currency) || (i > 0 && m.requirements[i-1].AgreementID == r.AgreementID) {
			return nil, ErrAllocationInput
		}
		v, err := allocationNumber(r.Amount)
		if err != nil {
			return nil, err
		}
		m.need = append(m.need, v)
		if len(r.Schedule) > MaxAllocationAssets {
			return nil, ErrAllocationLimit
		}
		for id, e := range r.Schedule {
			if _, ok := ids[id]; !ok {
				return nil, ErrAllocationInput
			}
			h, err := allocationNumber(e.Haircut)
			if err != nil || h.Cmp(big.NewRat(1, 1)) >= 0 {
				return nil, ErrAllocationInput
			}
		}
		for ai, a := range m.assets {
			e, ok := r.Schedule[a.ID]
			if !ok || !e.Eligible {
				continue
			}
			h, _ := e.Haircut.Rat()
			m.edges = append(m.edges, allocationEdge{ai, i, new(big.Rat).Sub(big.NewRat(1, 1), h), costs[ai]})
		}
	}
	if len(m.edges) > MaxAllocationEdges {
		return nil, ErrAllocationLimit
	}
	return m, nil
}

// Optimize solves a bounded divisible-collateral LP with agreement-specific
// haircut coefficients. Nonnegative costs permit exact coverage equalities:
// excess posting can always be reduced without worsening cost or feasibility.
// Canonical IDs and Bland pivots make input permutations economically identical.
func Optimize(ctx context.Context, assets []Asset, requirements []Requirement) (AllocationResult, error) {
	if err := ctx.Err(); err != nil {
		return AllocationResult{}, err
	}
	m, err := buildAllocationModel(assets, requirements)
	if err != nil {
		return AllocationResult{}, err
	}
	t := newAllocationTableau(m)
	phaseOne := make([]big.Rat, t.columns)
	for j := len(m.edges) + len(m.assets); j < t.columns; j++ {
		phaseOne[j].SetInt64(1)
	}
	if err = t.minimize(ctx, phaseOne, t.columns); err != nil {
		return AllocationResult{}, err
	}
	feasible := t.objective(phaseOne).Sign() == 0
	costs := phaseOne
	if feasible {
		if err = t.removeArtificial(ctx, len(m.edges)+len(m.assets)); err != nil {
			return AllocationResult{}, err
		}
		costs = make([]big.Rat, t.columns)
		for j, e := range m.edges {
			costs[j].Set(e.cost)
		}
		if err = t.minimize(ctx, costs, len(m.edges)+len(m.assets)); err != nil {
			return AllocationResult{}, err
		}
	}
	result := AllocationResult{Currency: m.currency, Feasible: feasible, Cost: "0", Certificate: Certificate{Capacity: map[string]dec.Exact{}, Coverage: map[string]dec.Exact{}}}
	dual := t.dual(costs, len(m.edges), len(m.assets)+len(m.requirements))
	for i, v := range dual {
		exact, e := dec.ExactFromRat(&v)
		if e != nil {
			return AllocationResult{}, ErrAllocationLimit
		}
		if i < len(m.assets) {
			result.Certificate.Capacity[m.assets[i].ID] = exact
		} else {
			result.Certificate.Coverage[m.requirements[i-len(m.assets)].AgreementID] = exact
		}
	}
	if feasible {
		values := t.solution(len(m.edges))
		for j, e := range m.edges {
			if values[j].Sign() == 0 {
				continue
			}
			used, e1 := dec.ExactFromRat(&values[j])
			posted, e2 := dec.ExactFromRat(new(big.Rat).Mul(&values[j], e.coverage))
			cost, e3 := dec.ExactFromRat(new(big.Rat).Mul(&values[j], e.cost))
			if e1 != nil || e2 != nil || e3 != nil {
				return AllocationResult{}, ErrAllocationLimit
			}
			result.Allocations = append(result.Allocations, Allocation{m.requirements[e.requirement].AgreementID, m.assets[e.asset].ID, used, posted, cost})
		}
		result.Cost, err = dec.ExactFromRat(t.objective(costs))
		if err != nil {
			return AllocationResult{}, ErrAllocationLimit
		}
	}
	if err = VerifyAllocation(assets, requirements, result); err != nil {
		return AllocationResult{}, err
	}
	return result, nil
}
