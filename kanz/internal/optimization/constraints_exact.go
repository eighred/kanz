package optimization

import (
	"context"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
)

// ExactConstraintSet is the financial feasible region. Solver approximations
// are advisory; acceptance compares the resulting weights with these originals.
type ExactConstraintSet struct {
	LongOnly    bool
	MinWeight   dec.Exact
	MaxWeight   dec.Exact
	Bounds      map[string]ExactWeightBound
	SectorCaps  map[string]dec.Exact
	MaxTurnover dec.Exact
}

type ExactWeightBound struct{ Min, Max dec.Exact }

func constraintRat(value dec.Exact, fallback int64) (*big.Rat, error) {
	if value == "" {
		return big.NewRat(fallback, 1), nil
	}
	r, err := value.Rat()
	if err != nil || r.Sign() < 0 || r.Cmp(big.NewRat(1, 1)) > 0 {
		return nil, ErrExactProposal
	}
	return r, nil
}

func (c *ExactConstraintSet) solver(instruments []string) (*ConstraintSet, error) {
	if c == nil {
		return nil, nil
	}
	// Short financing and cash are not represented by this v2 contract.
	if !c.LongOnly || len(c.Bounds) > MaxProposalInstruments || len(c.SectorCaps) > MaxProposalInstruments {
		return nil, ErrExactProposal
	}
	lo, err := constraintRat(c.MinWeight, 0)
	if err != nil {
		return nil, err
	}
	hi, err := constraintRat(c.MaxWeight, 1)
	if err != nil || lo.Cmp(hi) > 0 {
		return nil, ErrExactProposal
	}
	if _, err := constraintRat(c.MaxTurnover, 1); err != nil {
		return nil, err
	}
	for bucket, cap := range c.SectorCaps {
		if bucket == "" || len(bucket) > 256 || cap == "" {
			return nil, ErrExactProposal
		}
		if _, err := constraintRat(cap, 1); err != nil {
			return nil, err
		}
	}
	out := &ConstraintSet{LongOnly: true, Bounds: make(map[string]WeightBound)}
	sumLo, sumHi := new(big.Rat), new(big.Rat)
	known := make(map[string]bool, len(instruments))
	for _, id := range instruments {
		known[id] = true
		l, h := lo, hi
		if b, ok := c.Bounds[id]; ok {
			if b.Min == "" || b.Max == "" {
				return nil, ErrExactProposal
			}
			l, err = constraintRat(b.Min, 0)
			if err != nil {
				return nil, err
			}
			h, err = constraintRat(b.Max, 1)
			if err != nil {
				return nil, err
			}
		}
		if l.Cmp(h) > 0 {
			return nil, ErrExactProposal
		}
		sumLo.Add(sumLo, l)
		sumHi.Add(sumHi, h)
		lf, _ := l.Float64()
		hf, _ := h.Float64()
		out.Bounds[id] = WeightBound{Min: lf, Max: hf}
	}
	for id := range c.Bounds {
		if !known[id] {
			return nil, ErrExactProposal
		}
	}
	if sumLo.Cmp(big.NewRat(1, 1)) > 0 || sumHi.Cmp(big.NewRat(1, 1)) < 0 {
		return nil, ErrExactProposal
	}
	return out, nil
}

func (c *ExactConstraintSet) check(ctx context.Context, p ExactRebalanceProposal, classifier compliance.Classifier, asOf time.Time) error {
	if c == nil {
		return nil
	}
	lo, _ := constraintRat(c.MinWeight, 0)
	hi, _ := constraintRat(c.MaxWeight, 1)
	sectors := make(map[string]*big.Rat)
	for id, value := range p.Targets {
		w, _ := value.Rat()
		l, h := lo, hi
		if b, ok := c.Bounds[id]; ok {
			l, _ = b.Min.Rat()
			h, _ = b.Max.Rat()
		}
		if w.Cmp(l) < 0 || w.Cmp(h) > 0 {
			return ErrExactProposal
		}
		if len(c.SectorCaps) > 0 && w.Sign() != 0 {
			if classifier == nil {
				return ErrExactProposal
			}
			a, ok := classifier.Classify(ctx, id, asOf)
			if !ok || a.Sector == "" {
				return ErrExactProposal
			}
			if sectors[a.Sector] == nil {
				sectors[a.Sector] = new(big.Rat)
			}
			sectors[a.Sector].Add(sectors[a.Sector], w)
		}
	}
	for sector, value := range c.SectorCaps {
		cap, _ := value.Rat()
		if total := sectors[sector]; total != nil && total.Cmp(cap) > 0 {
			return ErrExactProposal
		}
	}
	limit, _ := constraintRat(c.MaxTurnover, 1)
	turnover, _ := p.Turnover.Rat()
	if turnover.Cmp(limit) > 0 {
		return ErrExactProposal
	}
	return ctx.Err()
}
