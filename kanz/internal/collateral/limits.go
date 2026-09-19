package collateral

import (
	"math/big"
	"sort"
	"strings"
)

func (m *allocationModel) addLimits(limits []AllocationLimit) error {
	if len(limits) > 64 {
		return ErrAllocationLimit
	}
	m.limits = append([]AllocationLimit(nil), limits...)
	sort.Slice(m.limits, func(i, j int) bool { return m.limits[i].ID < m.limits[j].ID })
	edges := map[[2]string]int{}
	for j, e := range m.edges {
		edges[[2]string{m.assets[e.asset].ID, m.requirements[e.requirement].AgreementID}] = j
	}
	for i, l := range m.limits {
		if l.ID == "" || len(l.ID) > 256 || strings.TrimSpace(l.ID) != l.ID || (i > 0 && m.limits[i-1].ID == l.ID) || len(l.Terms) > MaxAllocationEdges {
			return ErrAllocationInput
		}
		maximum, err := allocationNumber(l.Maximum)
		if err != nil {
			return err
		}
		weights := make([]big.Rat, len(m.edges))
		seen := map[int]bool{}
		for _, term := range l.Terms {
			j, ok := edges[[2]string{term.AssetID, term.AgreementID}]
			if !ok || seen[j] {
				return ErrAllocationInput
			}
			seen[j] = true
			v, err := allocationNumber(term.Weight)
			if err != nil {
				return err
			}
			weights[j].Set(v)
		}
		m.limitValues = append(m.limitValues, maximum)
		m.limitWeights = append(m.limitWeights, weights)
	}
	return nil
}
