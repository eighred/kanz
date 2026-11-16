package retrieval

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Claims use the tool's decimal/scientific notation, optionally with thousands
// separators. Signs and exponents stay in the token reported to the caller.
var decimalClaim = regexp.MustCompile(`^[+-]?(?:(?:[0-9]+|[0-9]{1,3}(?:,[0-9]{3})+)(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func parseNumericClaim(token string) (float64, bool) {
	normalized := strings.ReplaceAll(token, "−", "-")
	if !decimalClaim.MatchString(normalized) {
		return 0, false
	}
	normalized = strings.ReplaceAll(normalized, ",", "")
	v, err := strconv.ParseFloat(normalized, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	// A nonzero mantissa rounded out of the float domain is unavailable, not
	// the small literal zero. Actual signed zero remains the existing policy's 0.
	if v == 0 {
		for _, r := range normalized {
			if r == 'e' || r == 'E' {
				break
			}
			if r >= '1' && r <= '9' {
				return 0, false
			}
		}
	}
	return v, true
}

func claimSign(r rune) bool { return r == '+' || r == '-' || r == '−' }
func claimIdentifier(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func nonfiniteClaim(token string) bool {
	token = strings.TrimLeft(token, "+-−")
	return strings.EqualFold(token, "nan") || strings.EqualFold(token, "inf") || strings.EqualFold(token, "infinity") || token == "∞"
}

// numericTokens keeps maximal numeric-looking claims. An invalid exponent is
// reported whole instead of salvaging its coefficient or small exponent digits.
// Letter-adjacent identifiers (VaR99, PF-99, 42bps) remain identifiers. Standalone
// nonfinite words and numeric radix/underscore spellings are explicit refusals.
func numericTokens(s string) []string {
	rs := []rune(s)
	var out []string
	for i := 0; i < len(rs); {
		start := i
		if unicode.IsLetter(rs[i]) || rs[i] == '_' {
			for i < len(rs) && (claimIdentifier(rs[i]) || rs[i] == '-' && i+1 < len(rs) && claimIdentifier(rs[i+1])) {
				i++
			}
			word := string(rs[start:i])
			if nonfiniteClaim(word) {
				out = append(out, word)
			}
			continue
		}
		if claimSign(rs[i]) {
			// In a range or arithmetic expression, the intervening sign is a
			// delimiter. Exponent signs are consumed inside the number below.
			if i > 0 && (unicode.IsDigit(rs[i-1]) || rs[i-1] == '%' || rs[i-1] == ')') {
				i++
				continue
			}
			for i < len(rs) && claimSign(rs[i]) {
				i++
			}
		}
		if i >= len(rs) || !(unicode.IsDigit(rs[i]) || rs[i] == '∞' || unicode.IsLetter(rs[i]) && i > start || rs[i] == '.' && i+1 < len(rs) && unicode.IsDigit(rs[i+1]) && (i == 0 || rs[i-1] != '.')) {
			i = start + 1
			continue
		}
		for i < len(rs) {
			r := rs[i]
			if i+1 < len(rs) && unicode.IsLetter(rs[i+1]) && (r == ',' || r == '.' && rs[i+1] != 'e' && rs[i+1] != 'E') {
				break // sentence/list punctuation, not a numeric suffix
			}
			if claimIdentifier(r) || r == '.' || r == ',' || r == '∞' {
				i++
				continue
			}
			if claimSign(r) && i > start && (rs[i-1] == 'e' || rs[i-1] == 'E' || claimSign(rs[i-1])) {
				i++
				continue
			}
			break
		}
		token := strings.TrimRight(string(rs[start:i]), ".,")
		if start > 0 && claimIdentifier(rs[start-1]) {
			continue
		}
		body := strings.TrimLeft(token, "+-−")
		// Other letter suffixes are ordinary identifiers, not partial numbers.
		// Radix prefixes and underscores look numeric but are outside this grammar.
		radix := strings.HasPrefix(strings.ToLower(body), "0x") || strings.HasPrefix(strings.ToLower(body), "0b") || strings.HasPrefix(strings.ToLower(body), "0o")
		identifier := false
		var firstLetter rune
		for _, r := range body {
			if unicode.IsLetter(r) {
				if firstLetter == 0 {
					firstLetter = r
				}
				if r != 'e' && r != 'E' {
					identifier = true
				}
			}
		}
		exponentAttempt := firstLetter == 'e' || firstLetter == 'E'
		if identifier && !exponentAttempt && !radix && !nonfiniteClaim(token) {
			continue
		}
		out = append(out, token)
	}
	return out
}
