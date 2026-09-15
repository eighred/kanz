package collateral

import (
	"errors"
	"math/big"

	"github.com/eighred/kanz/internal/dec"
)

// ExactCSATerms are contractual monetary amounts in Currency. Empty fields are
// unknown, including Rounding; an explicit zero disables rounding.
type ExactCSATerms struct {
	Currency                                                string
	Threshold, MinimumTransfer, IndependentAmount, Rounding dec.Exact
}

type ExactMargin struct {
	Currency           string
	Required, Movement dec.Exact
}

var ErrMarginInput = errors.New("collateral: invalid exact margin inputs")

// CalculateMargin uses the collateral holder's perspective: positive exposure
// is owed to that holder, positive movement increases collateral held, negative
// movement returns it. Exposure may be negative; held collateral cannot be.
// Initial margin is a separately approved model result, not a recomputation of
// a venue's margin state. Every amount must be in the agreement currency.
//
// Required = max(exposure-threshold,0)+independent amount+initial margin.
// MTA applies before rounding. Deliveries round up; returns round towards zero
// so rounding cannot return collateral the holder does not possess or push the
// holder below the calculated requirement.
func CalculateMargin(exposure, held, initial dec.Exact, terms ExactCSATerms) (ExactMargin, error) {
	if len(terms.Currency) != 3 {
		return ExactMargin{}, ErrMarginInput
	}
	for _, c := range terms.Currency {
		if c < 'A' || c > 'Z' {
			return ExactMargin{}, ErrMarginInput
		}
	}
	values := []dec.Exact{exposure, held, initial, terms.Threshold, terms.MinimumTransfer, terms.IndependentAmount, terms.Rounding}
	r := make([]*big.Rat, len(values))
	for i, v := range values {
		var err error
		r[i], err = v.Rat()
		if err != nil || (i != 0 && r[i].Sign() < 0) {
			return ExactMargin{}, ErrMarginInput
		}
	}
	required := new(big.Rat).Sub(r[0], r[3])
	if required.Sign() < 0 {
		required.SetInt64(0)
	}
	required.Add(required, r[5]).Add(required, r[2])
	movement := new(big.Rat).Sub(required, r[1])
	if new(big.Rat).Abs(movement).Cmp(r[4]) < 0 {
		movement.SetInt64(0)
	}
	if r[6].Sign() > 0 {
		units := new(big.Rat).Quo(movement, r[6])
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(units.Num(), units.Denom(), rem)
		if rem.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
		movement.Mul(new(big.Rat).SetInt(q), r[6])
	}
	req, err := dec.ExactFromRat(required)
	if err != nil {
		return ExactMargin{}, ErrMarginInput
	}
	move, err := dec.ExactFromRat(movement)
	if err != nil {
		return ExactMargin{}, ErrMarginInput
	}
	return ExactMargin{Currency: terms.Currency, Required: req, Movement: move}, nil
}
