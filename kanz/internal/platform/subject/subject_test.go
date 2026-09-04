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

	"github.com/eighred/kanz/internal/platform/subject"
)

func TestTokenNeutralizesSubjectSyntax(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"acme", "acme"},
		{"fund-alpha", "fund-alpha"},
		{"BTC-USD", "BTC-USD"},

		// A dot would SPLIT one token into two, so `fund.alpha` would land the position
		// under a different (portfolio, instrument) pair than the one it belongs to.
		// It is ESCAPED rather than replaced: `fund.alpha` and `fund_alpha` are two
		// portfolios and must not share one compacted subject (#999).
		{"fund.alpha", "fund%2Ealpha"},

		// `>` matches everything to the right and `*` matches one token, but only as a
		// WHOLE token — measured against nats-server 2.14.5, both are literal inside a
		// token. They are escaped anyway: an id that IS ">" would be a wildcard, and
		// escaping costs nothing an id actually spends.
		{"fund>", "fund%3E"},
		{"fund*", "fund%2A"},
		{"fund_", "fund_"},

		// A space is the one byte nats-server REFUSES outright.
		{"a b", "a%20b"},
		{"a_b", "a_b"},

		// The escape character is itself escaped, or the encoding is not injective:
		// the id `%2E` and the id `.` would otherwise produce the same token.
		{"%2E", "%252E"},

		// An empty id must still produce a token, or the subject collapses — and the
		// sentinel must be one no non-empty id can reach. `%` is only ever emitted as
		// the lead of a two-hex-digit escape, so `%_` is unreachable.
		{"", subject.EmptyToken},
		{"_", "_"},
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
