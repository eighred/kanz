package subject_test

// The token sanitizer, promoted here on the second-consumer trigger: mandates
// (internal/compliance) and now positions (internal/risk/ingest) both build a subject
// out of ids somebody else chose.
//
// It matters because a NATS subject is DOT-DELIMITED and `*`/`>` are wildcards. An id
// carrying one of those does not fail — it silently changes what a subscription matches,
// which on a per-entity state stream means a consumer arms itself with the wrong entity's
// state, or with everybody's.

import (
	"testing"

	"github.com/kanz-eng/kanz/internal/platform/subject"
)

func TestTokenNeutralizesSubjectSyntax(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"acme", "acme"},
		{"fund-alpha", "fund-alpha"},
		{"BTC-USD", "BTC-USD"},

		// A dot would SPLIT one token into two, so `fund.alpha` would land the position
		// under a different (portfolio, instrument) pair than the one it belongs to.
		{"fund.alpha", "fund_alpha"},

		// `>` matches everything to the right and `*` matches one token: an id carrying
		// either would turn a subscription for ONE entity into a subscription for many.
		{"fund>", "fund_"},
		{"fund*", "fund_"},
		{"a b", "a_b"},

		// An empty id must still produce a token, or the subject collapses and two
		// different entities share one subject.
		{"", "_"},
	} {
		if got := subject.Token(tc.in); got != tc.want {
			t.Errorf("Token(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
