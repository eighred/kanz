// Package retrieval is the copilot's grounding layer (COPILOT-01c): every datum
// a tool returns carries a Citation tracing it to its source event in the
// AUDIT-01 lineage / LIN-01 catalog, and the grounding check verifies the
// model's answer only asserts numbers it was actually given. Together they
// answer "where did this number come from" and block hallucinated measures.
package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Citation traces a value in a copilot answer back to its source — the governed
// source event (AUDIT-01) and the lineage node (LIN-01) it was derived from.
type Citation struct {
	// SourceEventID is the FACT/event the value was computed from (AUDIT-01).
	SourceEventID string
	// PortfolioID / AsOf scope the citation.
	PortfolioID string
	AsOf        string
	// LineageNode is the LIN-01 catalog node id, resolved via the Catalog seam.
	LineageNode string
}

func (c Citation) String() string {
	n := c.LineageNode
	if n == "" {
		n = c.SourceEventID
	}
	return fmt.Sprintf("[%s @ %s ⟵ %s]", c.PortfolioID, c.AsOf, n)
}

// Catalog is the LIN-01 lineage seam: it resolves a source event id to its
// catalog node so a citation names the governed dataset, not just an opaque
// event id. The production impl queries the lineage service graph; the default
// resolves to the event id itself.
type Catalog interface {
	Resolve(ctx context.Context, sourceEventID string) (node string, ok bool)
}

// IdentityCatalog is the dependency-free default — the node IS the event id.
type IdentityCatalog struct{}

// Resolve returns the event id as its own node.
func (IdentityCatalog) Resolve(_ context.Context, sourceEventID string) (string, bool) {
	return sourceEventID, sourceEventID != ""
}

// GroundingResult reports whether an answer is fully grounded in its citations.
type GroundingResult struct {
	Grounded bool
	// Ungrounded are numeric tokens in the answer that match no cited value —
	// candidate hallucinated measures.
	Ungrounded []string
}

// CheckGrounding verifies every numeric value asserted in answer appears among
// the values the tools actually returned (citedValues). A number in the answer
// that no tool produced is an ungrounded (potentially hallucinated) measure.
// Years and small integer ordinals are ignored — the check targets measure-like
// magnitudes, not prose. citedValues is the union of every value the cited tool
// readings returned.
func CheckGrounding(answer string, citedValues []float64) GroundingResult {
	res := GroundingResult{Grounded: true}
	for _, tok := range numericTokens(answer) {
		v, err := strconv.ParseFloat(tok, 64)
		if err != nil {
			continue
		}
		if isAllowedLiteral(v) {
			continue
		}
		if !matchesAny(v, citedValues) {
			res.Grounded = false
			res.Ungrounded = append(res.Ungrounded, tok)
		}
	}
	sort.Strings(res.Ungrounded)
	return res
}

// matchesAny reports whether v is within a small relative tolerance of any cited
// value (the answer may round 0.0734 to 7.3% etc.).
func matchesAny(v float64, cited []float64) bool {
	for _, c := range cited {
		if approxEqual(v, c) {
			return true
		}
		// allow a percentage rendering of a ratio (0.073 ⇄ 7.3).
		if approxEqual(v, c*100) || approxEqual(v*100, c) {
			return true
		}
	}
	return false
}

func approxEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	scale := absf(a)
	if absf(b) > scale {
		scale = absf(b)
	}
	if scale < 1 {
		scale = 1
	}
	return d <= 0.01*scale
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// isAllowedLiteral skips integers small enough to be prose (counts, years) so
// the grounding check doesn't flag "the top 3 exposures" or "in 2026".
func isAllowedLiteral(v float64) bool {
	if v != float64(int64(v)) {
		return false // non-integer ⇒ a real measure, must be grounded
	}
	iv := int64(v)
	if iv >= 0 && iv <= 12 {
		return true // small ordinal/count
	}
	if iv >= 1900 && iv <= 2100 {
		return true // a year
	}
	return false
}

// numericTokens extracts standalone numeric substrings (handling %, commas) from
// s. A digit run touching a letter on either side is NOT numeric — it is part of
// an identifier (e.g. the "99" in the measure name "VaR99"), so it is discarded.
// This keeps the grounding check from flagging measure names as ungrounded
// claims.
func numericTokens(s string) []string {
	rs := []rune(s)
	var out []string
	var cur strings.Builder
	contaminated := false
	start := 0
	flush := func(end int) {
		defer cur.Reset()
		if cur.Len() == 0 {
			return
		}
		// A letter immediately before the run or immediately after taints it.
		before := start > 0 && isLetter(rs[start-1])
		after := end < len(rs) && isLetter(rs[end])
		if contaminated || before || after {
			return
		}
		out = append(out, strings.Trim(cur.String(), "."))
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r >= '0' && r <= '9' || r == '.':
			if cur.Len() == 0 {
				start = i
				contaminated = false
			}
			cur.WriteRune(r)
		case r == ',':
			// thousands separator inside a number — drop it, keep the run going
		default:
			flush(i)
		}
	}
	flush(len(rs))
	return out
}

func isLetter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}
