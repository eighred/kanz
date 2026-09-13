package dec

import (
	"errors"
	"math/big"
	"regexp"
)

// Exact is a bounded exact decimal or rational, encoded as JSON text. Its
// immutable string representation prevents a caller from mutating stored Rats.
// Empty is unavailable; it is never an implicit zero.
type Exact string

var ErrExact = errors.New("dec: exact value unavailable or outside supported bounds")
var exactSyntax = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+|/[1-9][0-9]*)?$`)

func (n Exact) Rat() (*big.Rat, error) {
	s := string(n)
	if len(s) > 400 || !exactSyntax.MatchString(s) {
		return nil, ErrExact
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Num().BitLen() > 512 || r.Denom().BitLen() > 512 {
		return nil, ErrExact
	}
	return r, nil
}

func ExactFromRat(r *big.Rat) (Exact, error) {
	if r == nil || r.Num().BitLen() > 512 || r.Denom().BitLen() > 512 {
		return "", ErrExact
	}
	places, finite := r.FloatPrec()
	if finite && places <= 128 {
		text := r.FloatString(places)
		if len(text) <= 256 {
			return Exact(text), nil
		}
	}
	return Exact(r.RatString()), nil
}

func ParseExact(text string) (Exact, error) {
	r, err := Exact(text).Rat()
	if err != nil {
		return "", err
	}
	return ExactFromRat(r)
}
