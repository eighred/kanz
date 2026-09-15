package collateral

import "math/big"

// VerifyAllocation does not trust the solver's basis, pivot history or objective.
// It checks the original capacity/coverage equations and dual inequalities.
// Primal/dual objective equality certifies global optimality. For infeasibility,
// y*A<=0 and y*b>0 contradict any nonnegative feasible original variables.
func VerifyAllocation(assets []Asset, requirements []Requirement, result AllocationResult) error {
	m, err := buildAllocationModel(assets, requirements)
	if err != nil {
		return err
	}
	if result.Currency != m.currency || len(result.Certificate.Capacity) != len(m.assets) || len(result.Certificate.Coverage) != len(m.requirements) || len(result.Allocations) > len(m.edges) {
		return ErrAllocationCertificate
	}
	capacity := make([]*big.Rat, len(m.assets))
	coverage := make([]*big.Rat, len(m.requirements))
	dualValue := new(big.Rat)
	for i, a := range m.assets {
		v, err := result.Certificate.Capacity[a.ID].Rat()
		if err != nil || v.Sign() > 0 {
			return ErrAllocationCertificate
		}
		capacity[i] = v
		dualValue.Add(dualValue, new(big.Rat).Mul(v, m.available[i]))
	}
	for i, r := range m.requirements {
		v, err := result.Certificate.Coverage[r.AgreementID].Rat()
		if err != nil {
			return ErrAllocationCertificate
		}
		coverage[i] = v
		dualValue.Add(dualValue, new(big.Rat).Mul(v, m.need[i]))
	}
	edgeByID := map[[2]string]allocationEdge{}
	for _, e := range m.edges {
		bound := new(big.Rat).Add(capacity[e.asset], new(big.Rat).Mul(coverage[e.requirement], e.coverage))
		cost := new(big.Rat)
		if result.Feasible {
			cost.Set(e.cost)
		}
		if bound.Cmp(cost) > 0 {
			return ErrAllocationCertificate
		}
		edgeByID[[2]string{m.assets[e.asset].ID, m.requirements[e.requirement].AgreementID}] = e
	}
	statedCost, err := allocationNumber(result.Cost)
	if err != nil {
		return ErrAllocationCertificate
	}
	if !result.Feasible {
		if len(result.Allocations) != 0 || statedCost.Sign() != 0 || dualValue.Sign() <= 0 {
			return ErrAllocationCertificate
		}
		return nil
	}
	used := make([]big.Rat, len(m.assets))
	posted := make([]big.Rat, len(m.requirements))
	total := new(big.Rat)
	seen := map[[2]string]bool{}
	for _, a := range result.Allocations {
		key := [2]string{a.AssetID, a.AgreementID}
		e, ok := edgeByID[key]
		if !ok || seen[key] {
			return ErrAllocationCertificate
		}
		seen[key] = true
		value, e1 := allocationNumber(a.UsedValue)
		post, e2 := allocationNumber(a.PostedValue)
		cost, e3 := allocationNumber(a.Cost)
		if e1 != nil || e2 != nil || e3 != nil || value.Sign() <= 0 {
			return ErrAllocationCertificate
		}
		if post.Cmp(new(big.Rat).Mul(value, e.coverage)) != 0 || cost.Cmp(new(big.Rat).Mul(value, e.cost)) != 0 {
			return ErrAllocationCertificate
		}
		used[e.asset].Add(&used[e.asset], value)
		posted[e.requirement].Add(&posted[e.requirement], post)
		total.Add(total, cost)
	}
	for i := range used {
		if used[i].Cmp(m.available[i]) > 0 {
			return ErrAllocationCertificate
		}
	}
	for i := range posted {
		if posted[i].Cmp(m.need[i]) != 0 {
			return ErrAllocationCertificate
		}
	}
	if total.Cmp(statedCost) != 0 || total.Cmp(dualValue) != 0 {
		return ErrAllocationCertificate
	}
	return nil
}
