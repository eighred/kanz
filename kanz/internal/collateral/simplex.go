package collateral

import (
	"context"
	"math/big"
)

// Exact two-phase primal simplex. The original identity columns are retained
// after phase I to recover dual certificates even when redundant rows disappear.
// Bland's entering/leaving rule prevents cycling; explicit work/bit limits and
// context cancellation bound resource use without misreporting infeasibility.
type allocationTableau struct {
	rows            [][]big.Rat
	basis           []int
	columns, pivots int
}

const maxAllocationPivots = 10000
const maxAllocationBits = 8192

func newAllocationTableau(m *allocationModel) *allocationTableau {
	n := len(m.edges)
	a := len(m.assets)
	slacks := a + len(m.limits)
	r := len(m.requirements)
	t := &allocationTableau{columns: n + slacks + r, basis: make([]int, slacks+r), rows: make([][]big.Rat, slacks+r)}
	for i := range t.rows {
		t.rows[i] = make([]big.Rat, t.columns+1)
		t.basis[i] = n + i
		t.rows[i][n+i].SetInt64(1)
		if i < a {
			t.rows[i][t.columns].Set(m.available[i])
		} else if i < slacks {
			t.rows[i][t.columns].Set(m.limitValues[i-a])
		} else {
			t.rows[i][t.columns].Set(m.need[i-slacks])
		}
	}
	for j, e := range m.edges {
		t.rows[e.asset][j].SetInt64(1)
		t.rows[slacks+e.requirement][j].Set(e.coverage)
		for k := range m.limits {
			t.rows[a+k][j].Set(&m.limitWeights[k][j])
		}
	}
	return t
}
func (t *allocationTableau) objective(cost []big.Rat) *big.Rat {
	sum := new(big.Rat)
	for i, b := range t.basis {
		sum.Add(sum, new(big.Rat).Mul(&cost[b], &t.rows[i][t.columns]))
	}
	return sum
}
func (t *allocationTableau) dual(cost []big.Rat, offset, count int) []big.Rat {
	dual := make([]big.Rat, count)
	for j := range dual {
		for i, b := range t.basis {
			dual[j].Add(&dual[j], new(big.Rat).Mul(&cost[b], &t.rows[i][offset+j]))
		}
	}
	return dual
}
func (t *allocationTableau) solution(n int) []big.Rat {
	values := make([]big.Rat, n)
	for i, b := range t.basis {
		if b < n {
			values[b].Set(&t.rows[i][t.columns])
		}
	}
	return values
}
func (t *allocationTableau) minimize(ctx context.Context, cost []big.Rat, enterLimit int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entering := -1
		for j := 0; j < enterLimit; j++ {
			reduced := new(big.Rat).Set(&cost[j])
			for i, b := range t.basis {
				reduced.Sub(reduced, new(big.Rat).Mul(&cost[b], &t.rows[i][j]))
			}
			if reduced.Sign() < 0 {
				entering = j
				break
			}
		}
		if entering < 0 {
			return nil
		}
		leaving := -1
		var ratio *big.Rat
		for i, row := range t.rows {
			if row[entering].Sign() <= 0 {
				continue
			}
			candidate := new(big.Rat).Quo(&row[t.columns], &row[entering])
			if leaving < 0 || candidate.Cmp(ratio) < 0 || (candidate.Cmp(ratio) == 0 && t.basis[i] < t.basis[leaving]) {
				leaving = i
				ratio = candidate
			}
		}
		// Valid inputs have bounded asset capacities and a nonnegative phase-I cost.
		// An unbounded direction therefore indicates an internal certificate failure.
		if leaving < 0 {
			return ErrAllocationCertificate
		}
		if err := t.pivot(ctx, leaving, entering); err != nil {
			return err
		}
	}
}
func (t *allocationTableau) pivot(ctx context.Context, row, column int) error {
	if t.pivots >= maxAllocationPivots {
		return ErrAllocationLimit
	}
	t.pivots++
	pivot := new(big.Rat).Set(&t.rows[row][column])
	for j := range t.rows[row] {
		t.rows[row][j].Quo(&t.rows[row][j], pivot)
	}
	for i := range t.rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i != row {
			factor := new(big.Rat).Set(&t.rows[i][column])
			if factor.Sign() != 0 {
				for j := range t.rows[i] {
					t.rows[i][j].Sub(&t.rows[i][j], new(big.Rat).Mul(factor, &t.rows[row][j]))
				}
			}
		}
		for j := range t.rows[i] {
			v := &t.rows[i][j]
			if v.Num().BitLen() > maxAllocationBits || v.Denom().BitLen() > maxAllocationBits {
				return ErrAllocationLimit
			}
		}
	}
	t.basis[row] = column
	return nil
}
func (t *allocationTableau) removeArtificial(ctx context.Context, limit int) error {
	for i := 0; i < len(t.rows); {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.basis[i] < limit {
			i++
			continue
		}
		if t.rows[i][t.columns].Sign() != 0 {
			return ErrAllocationCertificate
		}
		entering := -1
		for j := 0; j < limit; j++ {
			if t.rows[i][j].Sign() != 0 {
				entering = j
				break
			}
		}
		if entering >= 0 {
			if err := t.pivot(ctx, i, entering); err != nil {
				return err
			}
			i++
		} else {
			t.rows = append(t.rows[:i], t.rows[i+1:]...)
			t.basis = append(t.basis[:i], t.basis[i+1:]...)
		}
	}
	return nil
}
