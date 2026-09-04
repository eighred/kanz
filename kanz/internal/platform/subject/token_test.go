package subject_test

// TWO INSTRUMENTS MUST NOT SHARE ONE COMPACTED SUBJECT (#999).
//
// Token used to REPLACE `.`, `*`, `>` and ` ` with `_`, which is a many-to-one map, and
// the estate holds ids on both sides of it: `VOD.L` (a RIC) and `VOD_L` (the underscore
// shape MBS_A / OPT_A / LIQUID_FAST already use) both became `VOD_L`. On a stream that
// keeps ONE message per subject, one instrument's current position overwrote the other's
// and DeliverLastPerSubject returned one of them and silence about the other — the same
// partial-book defect EXEC-M20 fixed, one level down, feeding the compliance monitor's
// in-memory book. A fund holding an instrument its mandate forbids looks compliant
// because the holding is not there.
//
// The tests below assert the property that repair rests on: Token is INJECTIVE. They are
// written as a round-trip through Detoken because a round-trip is a proof of injectivity
// rather than a sample of it — if Detoken(Token(x)) == x for every x, no two x can share
// a token.

import (
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/platform/subject"
)

// The exact pairs #999 was filed on, plus the empty-id sentinel, which was the same
// collision one level quieter: Token("") and Token("_") both used to be "_".
func TestTokenDoesNotCollideTwoDifferentIDs(t *testing.T) {
	for _, tc := range []struct{ a, b, why string }{
		{"VOD.L", "VOD_L",
			"a RIC against the underscore shape MBS_A/OPT_A/LIQUID_FAST already use"},
		{"AAPL US Equity", "AAPL_US_Equity",
			"datamaster's DefaultIDResolver falls back to the Bloomberg ticker verbatim"},
		{"a.b", "a_b", "the dot is the token delimiter"},
		{"a*b", "a_b", "`*` is a wildcard only as a WHOLE token, but an id may still carry one"},
		{"a>b", "a_b", "`>` likewise"},
		{"", "_", "an empty id must not share the sentinel with an id that IS an underscore"},
		{"", "", "sanity: the table's own comparison is not vacuous"},
	} {
		got, want := subject.Token(tc.a), subject.Token(tc.b)
		if tc.a == tc.b {
			if got != want {
				t.Errorf("Token is not deterministic for %q", tc.a)
			}
			continue
		}
		if got == want {
			t.Errorf("Token(%q) == Token(%q) == %q — two instruments share one compacted "+
				"subject, so one overwrites the other's current position (%s)",
				tc.a, tc.b, got, tc.why)
		}
	}
}

// The whole point of a token is that it is ONE token and carries no wildcard.
//
// Measured against a real nats-server (2.14.5): the server REFUSES a space, tab, LF and
// CR in a subject, and ACCEPTS NUL, 0x1f and 0x7f — so "the broker will catch it" is not
// true for control bytes, and the encoder has to.
func TestTokenIsOneLegalSubjectToken(t *testing.T) {
	for _, in := range []string{
		"", "_", ".", "..", "*", ">", " ", "a b", "VOD.L", "AAPL US Equity",
		"a\x00b", "a\tb", "a\nb", "a\rb", "a\x7fb", "a\x1fb", "üñî", "100%", "%2E", "%",
		strings.Repeat(".", 8),
	} {
		tok := subject.Token(in)
		if tok == "" {
			t.Errorf("Token(%q) is empty — the subject would collapse a token", in)
			continue
		}
		if strings.Contains(tok, ".") {
			t.Errorf("Token(%q) = %q contains a dot — that is TWO tokens, so the id lands "+
				"under a different (portfolio, instrument) pair than it belongs to", in, tok)
		}
		if tok == "*" || tok == ">" {
			t.Errorf("Token(%q) = %q IS a wildcard token — a subscription for one entity "+
				"becomes a subscription for many", in, tok)
		}
		for i := 0; i < len(tok); i++ {
			if b := tok[i]; b < 0x21 || b > 0x7e {
				t.Errorf("Token(%q) = %q carries byte 0x%02x at %d — outside printable ASCII, "+
					"which nats-server either refuses (whitespace) or accepts silently (NUL, 0x7f)",
					in, tok, b, i)
				break
			}
		}
	}
}

// A round trip is the injectivity proof. It also gives an operator reading a subject off
// the stream a way back to the id the platform was handed.
func TestTokenRoundTripsThroughDetoken(t *testing.T) {
	for _, in := range []string{
		"", "_", "-", "%", "%%", "%2E", "%2e", ".", "*", ">", " ", "a b",
		"VOD.L", "VOD_L", "AAPL US Equity", "AAPL_US_Equity", "BTC-USD", "MBS_A",
		"a\x00b", "üñî", "\x7f", "..>..*..", "acme", "fund-alpha",
	} {
		got, err := subject.Detoken(subject.Token(in))
		if err != nil {
			t.Errorf("Detoken(Token(%q)) errored: %v", in, err)
			continue
		}
		if got != in {
			t.Errorf("Detoken(Token(%q)) = %q — Token is lossy, so two ids can share a subject", in, got)
		}
	}
}

// THE ESCAPE MUST NOT MOVE THE IDS THAT WERE ALREADY SAFE.
//
// This is what keeps the repair from being a migration of the whole book: an id built
// from letters, digits, `-` and `_` tokenizes to itself, so its compacted subject does
// not change and its retained current state stays where every consumer already reads it.
// Only ids that were being CORRUPTED move, and those had no correct subject to lose.
func TestTokenLeavesOrdinaryIDsUnchanged(t *testing.T) {
	for _, in := range []string{
		"acme", "fund-alpha", "BTC-USD", "VOD_L", "MBS_A", "OPT_A", "LIQUID_FAST",
		"XBIN", "XOKX", "ETH-USDT", "US0378331005", "037833100", "BBG000B9XRY4",
	} {
		if got := subject.Token(in); got != in {
			t.Errorf("Token(%q) = %q — an id that was already a legal token moved, which "+
				"orphans its retained state on the compacted stream", in, got)
		}
	}
}

// Detoken rejects what Token cannot emit, so a malformed subject read off the stream is
// an error an operator sees rather than a plausible-looking wrong id.
func TestDetokenRefusesAMalformedToken(t *testing.T) {
	for _, in := range []string{"%", "%2", "%ZZ", "%2Z", "a%", "a%G0"} {
		if got, err := subject.Detoken(in); err == nil {
			t.Errorf("Detoken(%q) = %q, want an error", in, got)
		}
	}
}

// FuzzTokenIsInjective is the property the whole repair rests on, over arbitrary bytes
// rather than a table somebody remembered to extend.
func FuzzTokenIsInjective(f *testing.F) {
	for _, s := range []string{"", "_", "VOD.L", "VOD_L", "AAPL US Equity", "%2E", "\x00", "üñî", "*", ">"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		tok := subject.Token(in)
		for i := 0; i < len(tok); i++ {
			if b := tok[i]; b < 0x21 || b > 0x7e || b == '.' {
				t.Fatalf("Token(%q) = %q carries 0x%02x — not one legal subject token", in, tok, b)
			}
		}
		if tok == "*" || tok == ">" {
			t.Fatalf("Token(%q) = %q is a wildcard token", in, tok)
		}
		back, err := subject.Detoken(tok)
		if err != nil {
			t.Fatalf("Detoken(Token(%q) = %q): %v", in, tok, err)
		}
		if back != in {
			t.Fatalf("Detoken(Token(%q)) = %q — lossy, so some other id shares this subject", in, back)
		}
	})
}
