package subject

import (
	"fmt"
	"strings"
)

// EmptyToken is the token an empty id maps to.
//
// It has to be a token NO non-empty id can produce, or the empty case is the same
// collision one level quieter. The sentinel used to be "_", which is exactly what an id
// that IS an underscore tokenized to — so a holding with a blank instrument id and a
// holding of the instrument `_` shared one compacted subject and one overwrote the other.
//
// Token emits `%` ONLY as the lead of a two-hex-digit escape, so `%` followed by anything
// that is not a hex digit is unreachable by construction. That is what makes this safe,
// and it is why the underscore is load-bearing rather than decorative.
const EmptyToken = "%_EMPTY"

const upperhex = "0123456789ABCDEF"

// safeTokenByte reports whether b may ride a subject token verbatim.
//
// Printable ASCII, minus the four bytes that are not ours to spend:
//
//	.  the token DELIMITER. A dot does not corrupt an id, it SPLITS it — `VOD.L` makes
//	   the position land under a different (portfolio, instrument) pair than the one it
//	   belongs to, and refdata's own test says a RIC carries a dot by design.
//	*  one-token wildcard, and
//	>  match-the-rest wildcard. Measured against nats-server 2.14.5 these are LITERAL
//	   inside a token and only wildcards as a whole token — but an id that IS ">" would
//	   be one, and escaping them unconditionally costs nothing a real id spends.
//	%  the escape character itself. Without escaping it the encoding is not injective:
//	   the id `%2E` and the id `.` would produce the same token.
//
// Everything outside 0x21..0x7e is escaped rather than trusted, because the broker is not
// the backstop it looks like. Measured on the same server: it REFUSES a space, tab, LF and
// CR in a subject, and ACCEPTS NUL, 0x1f and 0x7f. A control byte therefore reaches a
// compacted subject silently, and this function is the only thing that decides otherwise.
func safeTokenByte(b byte) bool {
	if b < 0x21 || b > 0x7e {
		return false
	}
	switch b {
	case '.', '*', '>', '%':
		return false
	}
	return true
}

// Token makes an id safe as a single NATS subject token — INJECTIVELY (#999).
//
// TWO IDS MUST NEVER PRODUCE ONE TOKEN. This used to REPLACE `.`, `*`, `>` and ` ` with
// `_`, which is a many-to-one map, and the estate holds ids on both sides of it:
//
//	VOD.L            -> VOD_L    a RIC (internal/refdata says a dot-bearing id is by design)
//	VOD_L            -> VOD_L    the underscore shape MBS_A / OPT_A / LIQUID_FAST use
//	AAPL US Equity   -> AAPL_US_Equity   datamaster's DefaultIDResolver falls FIGI -> ISIN ->
//	                                     CUSIP -> Symbol, so a vendor row with none of the
//	                                     first three yields the Bloomberg ticker verbatim
//
// What that cost: these subjects are COMPACTED CURRENT-STATE streams (POSITION and MANDATE
// both keep one message per subject, forever). Two instruments on one subject means one
// instrument's current position OVERWRITES the other's, and DeliverLastPerSubject — how a
// booting consumer learns the whole book in one read — returns one of them and silence
// about the other. That is the partial-book defect EXEC-M20 fixed, one level down, and it
// feeds the compliance monitor's in-memory book: a fund holding an instrument its mandate
// FORBIDS looks compliant, because the holding is simply not there. Nothing errors.
//
// The encoding is percent-escaping, the same shape internal/refdata already uses to put an
// instrument id in a URL path — one hostile-input rule, spelled the same way in both
// places. An id built from letters, digits, `-` and `_` TOKENIZES TO ITSELF, which is what
// keeps this repair from being a migration of the whole book: only ids that were being
// corrupted move, and those had no correct subject to lose. See Detoken for the way back.
func Token(s string) string {
	if s == "" {
		return EmptyToken
	}
	escapes := 0
	for i := 0; i < len(s); i++ {
		if !safeTokenByte(s[i]) {
			escapes++
		}
	}
	if escapes == 0 {
		// The overwhelming majority, and the reason nothing has to be migrated: the
		// subject this id already rides does not move.
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2*escapes)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if safeTokenByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

// Detoken is Token's inverse: the id the platform was handed, back out of the subject it
// rides.
//
// It exists for two reasons, and the second is the load-bearing one:
//
//  1. An operator reading `risk.position.changed.acme.fund-a.VOD%2EL` off the stream can
//     get to `VOD.L` without decoding hex by eye.
//  2. It makes Token's injectivity PROVABLE rather than sampled. If Detoken(Token(x)) == x
//     for every x, then no two ids can share a token — which is the entire property this
//     package exists to hold. See FuzzTokenIsInjective.
//
// It is STRICT about what it accepts: only what Token can emit. Uppercase hex, no bare
// `%`, nothing outside the safe byte set. A subject that does not decode is an operator's
// error to see, not a plausible-looking wrong id to act on.
func Detoken(tok string) (string, error) {
	if tok == EmptyToken {
		return "", nil
	}
	if tok == "" {
		return "", fmt.Errorf("subject: empty token — Token never emits one (an empty id is %q)", EmptyToken)
	}
	var b strings.Builder
	b.Grow(len(tok))
	for i := 0; i < len(tok); {
		c := tok[i]
		if c != '%' {
			if !safeTokenByte(c) {
				return "", fmt.Errorf("subject: token %q carries byte 0x%02x at %d, which Token never emits", tok, c, i)
			}
			b.WriteByte(c)
			i++
			continue
		}
		if i+2 >= len(tok) {
			return "", fmt.Errorf("subject: token %q ends in a truncated escape at %d", tok, i)
		}
		hi, okHi := unhex(tok[i+1])
		lo, okLo := unhex(tok[i+2])
		if !okHi || !okLo {
			return "", fmt.Errorf("subject: token %q has a malformed escape %q at %d (Token emits UPPERCASE hex)",
				tok, tok[i:i+3], i)
		}
		b.WriteByte(hi<<4 | lo)
		i += 3
	}
	return b.String(), nil
}

// unhex decodes one UPPERCASE hex digit. Lowercase is refused deliberately: Token emits
// only uppercase, so accepting both would make Detoken accept tokens that never came from
// here — and the round-trip would stop being the proof of injectivity it is used as.
func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
