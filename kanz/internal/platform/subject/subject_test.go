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
	"strings"
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

// The PER-VENUE position subject (EXEC-M19a).
//
// A fund's exposure does not care which exchange holds the BTC — but EXECUTION does, and
// cannot work without it: a CLOSE must flatten what is held AT EACH VENUE. So the book is
// projected twice, and the two FACTs must never collide on a subject: one is the fund-level
// aggregate, the other is the holding at one venue.
func TestVenuePositionSubjectIsDistinctFromTheAggregate(t *testing.T) {
	agg := subject.PositionFor("acme", "fund-alpha", "BTC-USD")
	venue := subject.VenuePositionFor("acme", "fund-alpha", "XBIN", "BTC-USD")

	if want := "risk.position.venue.changed.acme.fund-alpha.XBIN.BTC-USD"; venue != want {
		t.Errorf("VenuePositionFor = %q, want %q", venue, want)
	}
	if agg == venue {
		t.Fatal("the aggregate and the per-venue FACT share a subject — on a COMPACTED stream one would overwrite the other")
	}
	// The aggregate's wildcard must NOT swallow the per-venue FACTs, or the compliance
	// monitor and the risk engine would fold each venue as though it were the whole fund.
	if strings.HasPrefix(venue, strings.TrimSuffix(subject.PositionAll, ">")) {
		t.Errorf("the per-venue subject %q is matched by the aggregate binding %q — risk and compliance would "+
			"receive per-venue FACTs and treat each as the fund's total", venue, subject.PositionAll)
	}
}

func TestVenuePositionAllIsWhatTheExecutionPlaneBinds(t *testing.T) {
	if subject.VenuePositionAll != "risk.position.venue.changed.>" {
		t.Errorf("VenuePositionAll = %q", subject.VenuePositionAll)
	}
}
