package arch

import (
	"bytes"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE IDENTITY SERVICE MUST NOT TRUST THE GATEWAY'S PRINCIPAL HEADERS (#364).
//
// # The platform rule, and the one service it does not cover
//
// CLAUDE.md states the arrangement every other upstream relies on:
//
//	The gateway is the sole identity authority: it authenticates and injects
//	X-Kanz-Principal-*; upstreams trust those headers, which is sound ONLY
//	because a NetworkPolicy makes the gateway their only reachable caller.
//
// services/identity is the exception, and it is a permanent one. /login and
// /invites/redeem exist to be reached by people who hold no token — that is the
// entire purpose of the service — so it cannot be placed behind that policy. The
// premise that makes header-trust sound is therefore FALSE here, and only here.
//
// # What that means concretely
//
// A X-Kanz-Principal-Subject header arriving at this service is a string the
// caller typed. A route that read it for authorisation would let anyone who can
// reach the pod mint an invitation carrying any role they name, in any tenant —
// kanz-operator included. The provisioning route instead verifies a bearer
// token's SIGNATURE with the key this service already holds.
//
// # Why a guard rather than a comment
//
// The header helpers are the obvious way to identify a caller on this platform;
// they are used correctly in a dozen services and copying one here would look
// like consistency. It would also be silent: the route would work, the tests
// would pass, and the hole opens only for a caller who bypasses the gateway,
// which no test simulates by default. That is the shape this repository keeps
// paying for, and CLAUDE.md's answer is the standing one — an invariant worth
// keeping is a guard, not a paragraph.
func TestIdentityServiceTrustsNoPrincipalHeaders(t *testing.T) {
	root := moduleRoot(t)

	// The ways this platform reads a gateway-injected principal. Any of them
	// appearing in services/identity means the premise above was assumed.
	forbidden := []string{
		"PrincipalFromHeaders",
		"PrincipalFromContext",
		"HeaderPrincipalSubject",
		"HeaderPrincipalTenant",
		"HeaderPrincipalRoles",
		"X-Kanz-Principal",
	}

	// NON-VACUITY, half one: the scan must actually reach Go files. A walk that
	// matched nothing would pass this test on a service that trusted every header.
	var scanned int
	// NON-VACUITY, half two: the forbidden tokens must be real — each has to
	// appear SOMEWHERE in the module, or this guard is testing for typos.
	found := map[string]bool{}

	// THE SCAN READS CODE, NOT PROSE. Each file is parsed WITHOUT comments and
	// re-printed, so what is searched is identifiers and string literals only.
	//
	// A guard that a comment can trip is a guard people reword their way around,
	// and the reasoning for this rule necessarily NAMES the headers it forbids —
	// this very file does, and so does provision.go. Matching on raw source would
	// make explaining the rule the thing that violates it.
	walk := func(dir string, record func(path, code string)) {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			fset := token.NewFileSet()
			// No parser.ParseComments: the AST carries no comments, so printing it
			// back yields the file's code alone.
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			if err := format.Node(&buf, fset, file); err != nil {
				return err
			}
			record(path, buf.String())
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// Establish that the tokens exist elsewhere in the module, so a rename cannot
	// quietly turn this guard into a check for nothing.
	for _, dir := range []string{"services", "pkg", "internal"} {
		walk(dir, func(path, code string) {
			if strings.Contains(path, string(filepath.Separator)+"identity"+string(filepath.Separator)) {
				return
			}
			for _, tk := range forbidden {
				if strings.Contains(code, tk) {
					found[tk] = true
				}
			}
		})
	}
	for _, tk := range forbidden {
		if !found[tk] {
			t.Fatalf("%q appears NOWHERE else in the module — this guard is scanning for a string that "+
				"no longer exists, so it would pass on an identity service that trusted every header. "+
				"Rename it here to whatever replaced it.", tk)
		}
	}

	// The rule itself, over PRODUCTION code only.
	//
	// _test.go is excluded deliberately, and the exclusion is not a weakening: the
	// test that matters most here SETS these headers on purpose, to prove the
	// provisioning route ignores them
	// (TestProvision_APrincipalHeaderIsNotAnIdentity). Forbidding the string in
	// tests would forbid demonstrating the very property this guard exists to
	// keep. A test that trusted a header would still be harmless on its own — the
	// hole only opens if PRODUCTION code reads one, which is what is scanned.
	var problems []string
	walk("services/identity", func(path, code string) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for _, tk := range forbidden {
			if strings.Contains(code, tk) {
				problems = append(problems, rel+" references "+tk)
			}
		}
	})
	if scanned < 3 {
		t.Fatalf("scanned only %d Go files under services/identity — the walk is broken, not the "+
			"service", scanned)
	}
	if len(problems) > 0 {
		t.Errorf("services/identity reads a gateway-injected principal header:\n  %s\n\n"+
			"This service is NOT behind the NetworkPolicy that makes those headers trustworthy — "+
			"/login and /invites/redeem must be reachable by callers holding no token, which is why "+
			"the service exists. A header arriving here is a string the caller typed, so reading one "+
			"for authorisation lets anyone who can reach the pod provision an account with any role "+
			"in any tenant. Authenticate the bearer token instead (identity.Signer.Verify).",
			strings.Join(problems, "\n  "))
	}
}
