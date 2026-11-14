package server

import (
	"errors"
	"math/big"
	"strings"

	"github.com/eighred/kanz/internal/dec"
)

// The text budget matches dec.Exact's external-input budget. Its parser cannot
// be substituted here: HTTP historically accepts bounded SetString formats,
// including scientific notation, radix prefixes and fractions.
const numericTextBytes = 400

// A 400-byte mantissa has at most 1600 binary digits. An exponent this large
// cannot leave a nonzero result inside ExactFromRat's 512-bit domain, even after
// the radix point cancels part of it. Keep expansion work bounded BEFORE parsing;
// ExactFromRat, not this conservative work ceiling, owns the result domain.
const numericExpansionExponent = 8 * numericTextBytes

var errNumericBounds = errors.New("accounting: numeric input unavailable or outside supported bounds")

func parseRat(s string) (*big.Rat, error) {
	if !numericWorkBound(s) {
		return nil, errNumericBounds
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, errNumericBounds
	}
	if _, err := dec.ExactFromRat(r); err != nil {
		return nil, errNumericBounds
	}
	return r, nil
}

// numericWorkBound does not parse financial values or decide syntax. It bounds
// the work SetString may do; SetString still validates the complete literal.
func numericWorkBound(s string) bool {
	if s == "" || len(s) > numericTextBytes {
		return false
	}
	if strings.Contains(s, "/") {
		// Both integer operands are already text-bounded. Reduction may produce
		// an admitted result even when the individual operands exceed 512 bits.
		return true
	}
	mantissa := strings.TrimPrefix(strings.TrimPrefix(s, "+"), "-")
	markers := "eEpP"
	if strings.HasPrefix(mantissa, "0x") || strings.HasPrefix(mantissa, "0X") {
		markers = "pP" // e/E are hexadecimal digits, not exponent markers.
	}
	index := strings.IndexAny(mantissa, markers)
	if index < 0 {
		return true
	}
	exponent := mantissa[index+1:]
	mantissa = mantissa[:index]
	if len(mantissa) >= 2 && mantissa[0] == '0' && strings.ContainsRune("xXbBoO", rune(mantissa[1])) {
		mantissa = mantissa[2:]
	}
	if strings.Trim(mantissa, "0._") == "" {
		// SetString returns zero before expanding an exponent. Preserve that
		// bounded path; its exponent and mantissa syntax are still checked there.
		return true
	}
	exponent = strings.TrimPrefix(strings.TrimPrefix(exponent, "+"), "-")
	value := 0
	for _, ch := range exponent {
		if ch == '_' {
			continue // SetString owns underscore placement validation.
		}
		if ch < '0' || ch > '9' {
			return false
		}
		value = value*10 + int(ch-'0')
		if value > numericExpansionExponent {
			return false
		}
	}
	return true
}
