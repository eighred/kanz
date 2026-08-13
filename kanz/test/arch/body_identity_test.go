package arch

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A HANDLER THAT ACCEPTS A PERSON'S NAME MUST ALSO ESTABLISH WHO IS ASKING
// (#445).
//
// datamaster's pricing override decoded `actor` from the request body and wrote
// it, unchecked, into an append-only compliance record — while the gateway had
// already authenticated the caller and injected X-Kanz-Principal-Subject, which
// that service read for the TENANT and ignored for the IDENTITY. Any caller
// entitled to override could sign a colleague's name into the trail. Fixed in
// #444; this is what stops the next one.
//
// THE SWEEP THAT PRECEDED THIS GUARD FOUND NO OTHER INSTANCE, and the shape of
// the exceptions is therefore known rather than guessed — which is why the
// exemption list below is two entries and not fourteen. #445 deliberately filed
// the audit before the guard for exactly this reason: a guard written against a
// guessed exception set needs an allow-list long enough to become the thing
// opting out of the proof.
//
// WHAT IT CHECKS: a non-test file under services/ that BOTH registers a mutating
// HTTP route AND declares a JSON field naming a person must read the
// authenticated principal somewhere in that file.
//
// WHAT IT CANNOT CHECK: that the handler USES the principal for that field
// rather than merely reading it for something else. datamaster reads it for the
// tenant and, since #444, for the actor — a file could regress to the former and
// still pass. The behavioural tests in
// services/datamaster/internal/server/override_actor_test.go carry that half.
// What this forces is that a route accepting an identity cannot be written by
// someone who never thought about authentication at all.
//
// WHY MUTATING ROUTES ONLY: a read that takes a subject in its body is a query
// filter, not an attribution. The defect is writing somebody's name down.

// bodyIdentityScope is the tree searched.
const bodyIdentityScope = "services"

var (
	// bodyIdentityFieldRe matches a JSON struct tag naming a PERSON. Deliberately
	// not "id" or "name" — those are overwhelmingly the identity of a THING
	// (portfolio_id, instrument_id) and matching them would bury the signal.
	bodyIdentityFieldRe = regexp.MustCompile(
		"`json:\"(actor|approver|approved_by|decided_by|operator|principal|subject|user)\"`")
	// bodyIdentityMutatingRouteRe matches registration of a route that writes.
	bodyIdentityMutatingRouteRe = regexp.MustCompile(
		`Handle(Func)?\((?:authz\.[A-Za-z]+,\s*)?"(POST|PUT|PATCH|DELETE)\s`)
	// bodyIdentityPrincipalRe matches a CALL that establishes who is asking.
	//
	// THE TRAILING `(` IS LOAD-BEARING, and comments are stripped before this runs.
	// Without both, the guard matched PROSE: datamaster's server.go mentions
	// auth.RequireCallerTenantIs twice in comments, so removing every real call
	// still passed. Found by mutation, which is the only way that class of
	// weakness surfaces — the guard was green for the wrong reason and looked
	// exactly like a guard that was green for the right one.
	bodyIdentityPrincipalRe = regexp.MustCompile(
		`(PrincipalFromHeaders|PrincipalFromContext|RequireCallerTenant[A-Za-z]*)\(`)
	// bodyIdentityLineCommentRe strips // comments so prose cannot satisfy any of
	// the regexes above. Crude on purpose: a `//` inside a string literal would be
	// stripped too, which can only make this guard STRICTER, never weaker.
	bodyIdentityLineCommentRe = regexp.MustCompile(`(?m)//.*$`)
)

// bodyIdentityExempt maps a module-relative file to the reason it may accept an
// identity without reading a principal, and what retires the entry.
//
// BOTH ENTRIES ARE PRE-AUTHENTICATION, and that is the only argument this list
// accepts. A route whose PURPOSE is to establish who someone is cannot require
// that it already be established; the subject in its body is a claim being
// TESTED against a credential, not an attribution being recorded.
var bodyIdentityExempt = map[string]string{
	"identity/internal/server/server.go": "PRE-AUTHENTICATION. /login and /invites/redeem are where a " +
		"subject is established — the body's subject is a claim verified against a credential or an " +
		"invite token, not a name written down on somebody's behalf. Requiring a principal here " +
		"would mean requiring a login to log in.",
	"web-bff/internal/server/server.go": "PRE-AUTHENTICATION, same as identity's and for the same " +
		"reason: /auth/login, /auth/redeem and /auth/logout front the identity service's routes for " +
		"the browser. The subject is verified upstream against a credential; this tier holds the " +
		"session cookie, it does not attribute an action to a person.",
}

func TestAMutatingRouteThatTakesAnIdentityReadsThePrincipal(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	seenExempt := map[string]bool{}
	scanned, withIdentity := 0, 0

	for _, gf := range goFilesUnder(t, filepath.Join(root, bodyIdentityScope)) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		scanned++
		body := bodyIdentityLineCommentRe.ReplaceAllString(string(gf.body), "")
		if !bodyIdentityFieldRe.MatchString(body) {
			continue
		}
		// A file with no mutating route is not attributing anything: an output
		// struct (datamaster's pricing.Override MarshalJSON) or an outbound client
		// request carries these names legitimately.
		if !bodyIdentityMutatingRouteRe.MatchString(body) {
			continue
		}
		withIdentity++

		if reason, ok := bodyIdentityExempt[gf.rel]; ok {
			seenExempt[gf.rel] = true
			t.Logf("%s: exempt — %s", gf.rel, reason)
			continue
		}
		if !bodyIdentityPrincipalRe.MatchString(body) {
			offenders = append(offenders, gf.rel)
		}
	}

	// NON-VACUITY, BOTH HALVES. A broken scan finds no files and passes; a broken
	// field regex finds no identities and passes just as quietly.
	if scanned < 50 {
		t.Fatalf("scanned only %d non-test files under %s — the walk is broken, not the estate",
			scanned, bodyIdentityScope)
	}
	if withIdentity < 3 {
		t.Fatalf("found %d mutating handler(s) declaring an identity field — expected at least 3 "+
			"(datamaster's override plus the two pre-auth surfaces). The regexes stopped matching "+
			"and this guard is asserting nothing", withIdentity)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d mutating handler(s) accept a person's name and never establish who is asking: %v.\n"+
			"The gateway authenticates the caller and injects X-Kanz-Principal-Subject; a handler "+
			"that ignores it and takes the name from the request instead lets one credential sign "+
			"another person's name into whatever it writes. That is #444 exactly. Read the "+
			"principal and refuse a body that names anyone else, or add an argued entry to "+
			"bodyIdentityExempt.", len(offenders), offenders)
	}

	// DEAD-ENTRY ARM: an exemption for a file that no longer matches has outlived
	// its repair and would wave through a future handler in that file.
	for rel, reason := range bodyIdentityExempt {
		if !seenExempt[rel] {
			t.Errorf("exemption for %q (%s) matches no mutating handler declaring an identity "+
				"field — delete it", rel, reason)
		}
	}
}
