package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A TOKEN NOBODY IN THIS ESTATE CAN MINT IS A TOKEN NOBODY CAN PRESENT (#530).
//
// # What happened
//
// The gateway expected `iss` https://login.eighred.com. The identity service —
// this platform's own IdP, shipped for #364 — mints https://identity.kanz.internal.
// pkg/auth/oidc.go says the comparison is EXACT, so every token the estate could
// produce was refused by the only surface that accepts one. The deployed
// manifests had no working authenticated path at all, while each half was
// individually correct and individually reviewed.
//
// # Why it survived
//
// The gateway's issuer carried a well-argued comment explaining why it pointed at
// a host that does not resolve: identity had no manifest, no secret store, and
// nowhere to keep a durable signing key. That was TRUE ON THE DAY IT WAS WRITTEN
// (2026-08-13) and stopped being true two days later, when identity-deploy.yaml
// landed with exactly the mounted signing key the comment said it lacked. The
// comment even said "point this at the identity service when it ships" — and
// nobody went back to the line it pointed at.
//
// Dated evidence reads as current evidence. That is the failure mode this guard
// exists for, and a comment cannot guard against it because a comment is the
// thing that went stale.
//
// # What this checks
//
// Every issuer a deployed service EXPECTS (`*_OIDC_ISSUER`) is one some deployed
// service MINTS (`*_TOKEN_ISSUER`), or is named in federatedIssuers with the
// reason it comes from outside. It is a text comparison across two manifests,
// and it would have failed the day identity shipped.
//
// # What it deliberately does NOT check
//
// Reachability. An issuer can agree perfectly and still be unroutable — #530's
// second gap was a missing NetworkPolicy egress rule, and network_policy_coverage
// is where that belongs. Agreement and reachability are different properties and
// a guard that blurred them would report one when it meant the other.
func TestEveryExpectedOIDCIssuerIsMintedInThisEstate(t *testing.T) {
	root := moduleRoot(t)
	deployDir := filepath.Join(root, "infra", "deploy")

	expected := issuerEnvValues(t, deployDir, oidcIssuerRe)
	minted := issuerEnvValues(t, deployDir, tokenIssuerRe)

	// NON-VACUITY, BOTH DIRECTIONS. A regex that stops matching — the manifests
	// move to a ConfigMap, the env block changes shape — empties both maps, and
	// "every expected issuer is minted" becomes true of nothing.
	if len(expected) == 0 {
		t.Fatal("no *_OIDC_ISSUER found in any infra/deploy manifest — the scan is broken, not the " +
			"estate. The api-gateway has carried one since #457")
	}
	if len(minted) == 0 {
		t.Fatal("no *_TOKEN_ISSUER found in any infra/deploy manifest — the scan is broken, not the " +
			"estate. identity-deploy.yaml has carried one since #364")
	}

	mintedBy := map[string]string{}
	for file, issuers := range minted {
		for _, iss := range issuers {
			mintedBy[iss] = file
		}
	}

	var orphaned []string
	for file, issuers := range expected {
		for _, iss := range issuers {
			if _, ok := mintedBy[iss]; ok {
				continue
			}
			if _, ok := federatedIssuers[iss]; ok {
				continue
			}
			orphaned = append(orphaned, file+" expects "+iss)
		}
	}
	sort.Strings(orphaned)

	if len(orphaned) > 0 {
		var mintedList []string
		for iss, file := range mintedBy {
			mintedList = append(mintedList, iss+" (minted by "+file+")")
		}
		sort.Strings(mintedList)
		t.Errorf("%d service(s) expect an issuer nothing in this estate mints:\n  %s\n\n"+
			"Minted here:\n  %s\n\n"+
			"pkg/auth/oidc.go compares `iss` EXACTLY, so a service expecting an issuer no local IdP "+
			"produces refuses every token the estate can make — a total authentication outage in "+
			"which both halves look individually correct. That is #530: the gateway expected "+
			"login.eighred.com while identity minted identity.kanz.internal, and the justification "+
			"for the mismatch was a comment whose premise had expired.\n\n"+
			"Point the expectation at the issuer that is minted, or add it to federatedIssuers with "+
			"the external IdP it comes from — a deployment MAY legitimately federate, and this guard "+
			"refuses to guess which.",
			len(orphaned), strings.Join(orphaned, "\n  "), strings.Join(mintedList, "\n  "))
	}

	// DEAD-ENTRY ARM. An exemption that outlives the federation it describes is a
	// standing permission to mismatch, and the next reader takes it as evidence
	// that somebody checked.
	for iss := range federatedIssuers {
		found := false
		for _, issuers := range expected {
			for _, e := range issuers {
				if e == iss {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("federatedIssuers names %q, which no manifest expects any more — delete the "+
				"entry. A stale exemption reads as a decision somebody made about the estate as it "+
				"is now", iss)
		}
	}
}

// federatedIssuers are issuers deliberately minted OUTSIDE this estate.
//
// EMPTY, AND THAT IS THE CURRENT POSTURE. Every token this platform accepts is
// one it mints, which is what #364 built. An entry here is a deployment that
// federates to an external IdP; it must name which, because "some external
// provider" is not a thing anybody can verify later.
var federatedIssuers = map[string]string{}

var (
	// Matches `- { name: FOO_OIDC_ISSUER, value: "https://…" }` and the
	// multi-line form. The manifests use the flow style throughout.
	oidcIssuerRe  = regexp.MustCompile(`name:\s*([A-Z0-9_]*OIDC_ISSUER)\s*,\s*value:\s*"([^"]+)"`)
	tokenIssuerRe = regexp.MustCompile(`name:\s*([A-Z0-9_]*TOKEN_ISSUER)\s*,\s*value:\s*"([^"]+)"`)
)

// issuerEnvValues maps manifest filename -> issuer URLs matched by re.
func issuerEnvValues(t *testing.T, dir string, re *regexp.Regexp) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v — the guard cannot check what it cannot read", dir, err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		// COMMENTS STRIPPED. These manifests argue their own configuration at
		// length, and #530's whole shape was a comment quoting a hostname that
		// was no longer the right one. A guard that matched prose would have read
		// that argument as configuration.
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.Index(line, "#"); i >= 0 {
				line = line[:i]
			}
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				out[e.Name()] = append(out[e.Name()], m[2])
			}
		}
	}
	return out
}
