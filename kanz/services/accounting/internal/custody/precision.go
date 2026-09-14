package custody

import (
	"errors"
	"github.com/eighred/kanz/internal/dec"
	"math/big"
	"regexp"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

var ErrUnverifiedPrecision = errors.New("custody: exact values unavailable; replay source data and reconcile")
var storedNumber = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+|/[1-9][0-9]*)?$`)

func boundedRat(r *big.Rat) bool {
	return r != nil && r.Num().BitLen() <= 512 && r.Denom().BitLen() <= 512
}

func exactStored(r *big.Rat) (string, error) {
	if !boundedRat(r) {
		return "", ErrUnverifiedPrecision
	}
	return r.RatString(), nil
}

func optionalStored(r *big.Rat) (string, error) {
	if r == nil {
		return "", nil
	}
	return exactStored(r)
}

func parseStored(s string) (*big.Rat, error) {
	if len(s) > 400 || !storedNumber.MatchString(s) {
		return nil, ErrUnverifiedPrecision
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || !boundedRat(r) {
		return nil, ErrUnverifiedPrecision
	}
	return r, nil
}

// ExactDecimalText refuses missing, nonterminating and oversized values. No
// financial value crosses a float or fixed-scale rounding operation.
func ExactDecimalText(r *big.Rat) (string, bool) {
	if !boundedRat(r) {
		return "", false
	}
	places, exact := r.FloatPrec()
	if !exact || places > 128 {
		return "", false
	}
	text := r.FloatString(places)
	return text, len(text) <= 256
}

func exactProto(r *big.Rat) (*commonpb.Decimal, bool) {
	return dec.ToProtoExact(r)
}
