package custody

import (
	"errors"
	"math/big"
	"regexp"
	"strings"

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
	text, ok := ExactDecimalText(r)
	if !ok {
		return nil, false
	}
	exponent := int32(0)
	if dot := strings.IndexByte(text, '.'); dot >= 0 {
		exponent = -int32(len(text) - dot - 1)
		text = text[:dot] + text[dot+1:]
	}
	for len(text) > 1 && strings.HasSuffix(text, "0") {
		text = strings.TrimSuffix(text, "0")
		exponent++
	}
	coefficient, ok := new(big.Int).SetString(text, 10)
	if !ok || !coefficient.IsInt64() || exponent < -60 || exponent > 60 {
		return nil, false
	}
	return &commonpb.Decimal{Coefficient: coefficient.Int64(), Exponent: exponent}, true
}
