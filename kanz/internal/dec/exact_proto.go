package dec

import (
	"math/big"
	"strings"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// ParseProtoExact bounds work before parsing and refuses any value that the
// canonical Decimal cannot represent exactly. Missing input is not zero.
func ParseProtoExact(text string) (*commonpb.Decimal, bool) {
	r, err := Exact(text).Rat()
	if err != nil {
		return nil, false
	}
	return ToProtoExact(r)
}

// ToProtoExact preserves every digit or refuses. It never rounds, wraps, or
// substitutes zero for an unknown value.
func ToProtoExact(r *big.Rat) (*commonpb.Decimal, bool) {
	if r == nil || r.Num().BitLen() > 512 || r.Denom().BitLen() > 512 {
		return nil, false
	}
	places, exact := r.FloatPrec()
	if !exact || places > 128 {
		return nil, false
	}
	text := r.FloatString(places)
	if len(text) > 256 {
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
