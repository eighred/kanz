package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ONBOARD-M1: the golden path must actually COMPOSE the infrastructure
// lifecycle script, not just describe doing so.
//
// The root cause of ONBOARD-M1 was that NOTHING COMPARED THE TWO SCRIPTS.
// infra/onboarding/provision-tenant.sh (the CLIENT golden path an operator
// runs to bring a new tenant online) and infra/tenancy/tenantctl.sh (the
// INFRASTRUCTURE lifecycle script — the sole owner of identity, broker
// isolation, the Postgres role, and the gateway quota) drifted apart until
// the golden path claimed "no per-tenant stream needed" and separately wrote
// its own copy of the gateway quota ConfigMap. SEC-M3 made the first claim
// FALSE and DANGEROUS: the production broker now requires a client SVID
// mapped to a NATS account, so a tenant onboarded without tenantctl.sh's
// broker step gets no account and cannot publish at all — silently, because
// nothing dialed the broker to find out.
//
// Task 1 fixed both defects by making the golden path COMPOSE tenantctl.sh as
// its `infra` step instead of reimplementing or omitting pieces of it. This
// file is the guard that keeps that fix from decaying: a comment saying
// "tenantctl.sh owns quota" rots the day nobody rereads it; a test does not.
//
// Both scripts now legitimately DISCUSS the composition in prose comments —
// provision-tenant.sh's header explains that tenantctl.sh owns quota and
// names RATE_PER_SEC/BURST/MAX_IN_FLIGHT as knobs that pass through to it. A
// guard that grepped the whole file text would be tripped by that legitimate
// prose the moment someone documents the design more thoroughly. So the two
// checks that assert something IS or IS NOT actually DONE (invokes
// tenantctl.sh; does not write the quota ConfigMap) look only at "live"
// lines — lines a shell would execute, with full-line comments stripped —
// the same live-code-vs-commentary distinction that made the NATS identity
// guard use AST instead of grep for Go. The retired-claim check is the
// deliberate exception: "no per-tenant stream needed" is a false statement
// about the ARCHITECTURE, and would be exactly as false and exactly as
// dangerous resurrected in a comment as in a live line, so it is checked
// against the raw file text, comments included.
const (
	provisionTenantRelPath = "infra/onboarding/provision-tenant.sh"
	tenantctlRelPath       = "infra/tenancy/tenantctl.sh"

	// retiredGatewayQuotaCM is the ConfigMap tenantctl.sh's quota step
	// (MT-01e) owns exclusively. A second write to it from the golden path
	// is the duplicate-write half of ONBOARD-M1: two scripts fighting over
	// the same resource, with whichever ran last deciding the tenant's
	// budget.
	retiredGatewayQuotaCM = "api-gateway-quotas"

	// retiredNoStreamClaim is the specific false statement SEC-M3
	// invalidated: the golden path once claimed no per-tenant NATS
	// resource was needed. SEC-M3 requires every dialer to present an SVID
	// mapped to a NATS account (created by tenantctl.sh's broker step), so
	// a tenant onboarded without it cannot publish. Pinned verbatim so the
	// specific sentence cannot return.
	retiredNoStreamClaim = "no per-tenant stream needed"
)

// readOnboardingScript reads a script under the module root, normalizing
// CRLF — git on a Windows checkout (core.autocrlf=true) hands these scripts
// back with CRLF line endings, and a line-based or substring match against
// raw "\n"-delimited content would then miscount lines or silently miss
// matches, the same failure mode deployability_test.go documents for
// Dockerfiles. A missing, unreadable, or empty script is a FAILURE here, not
// a skip: a guard that quietly no-ops when a script disappears would not
// have caught ONBOARD-M1 either, since the defect there was silence, not a
// loud error.
func readOnboardingScript(t *testing.T, root, rel string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — this guard cannot verify the onboarding composition without it; "+
			"restore the script or fix the path", rel, err)
	}
	content := strings.ReplaceAll(string(b), "\r\n", "\n")
	if strings.TrimSpace(content) == "" {
		t.Fatalf("%s is empty — the golden path composition cannot be checked against nothing", rel)
	}
	return content
}

// liveLines returns every line of a shell script that is not blank and not
// a full-line comment (trimmed text starting with "#"). Both onboarding
// scripts legitimately discuss the composition in prose comments (see the
// package doc above), so any check that asserts something IS or IS NOT
// actually done must look only at lines the shell would execute — otherwise
// a sentence explaining the design trips a check meant to catch the design
// being violated. This repository's shell scripts use full-line comments
// only (no trailing "code # comment"), so stripping lines whose trimmed text
// starts with "#" is sufficient; it is not a general shell-comment parser.
func liveLines(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// executedLines narrows liveLines further to lines the shell can actually
// run as a COMMAND, excluding `echo` status lines. provision-tenant.sh
// prints a status line naming the step it is about to run — e.g. `echo
// "... (tenantctl.sh onboard)"` — right before the line that actually runs
// it, so a plain substring search over liveLines is fooled the same way a
// comment would fool it: the echo'd text mentions tenantctl.sh whether or
// not the invocation beneath it still exists. This is what makes the
// invocation check trustworthy rather than vacuous — deleting the real
// invocation line while leaving its status echo behind must still fail.
func executedLines(content string) []string {
	var out []string
	for _, line := range liveLines(content) {
		if strings.HasPrefix(strings.TrimSpace(line), "echo") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// TestProvisionTenantInvokesTenantctl asserts the composition Task 1 built
// is real, not aspirational: the golden path must contain a live line that
// actually runs tenantctl.sh, not merely a comment naming it. tenantctl.sh
// itself must also exist and be readable — invoking a script that is not
// there is not a composition, it is a dangling reference.
func TestProvisionTenantInvokesTenantctl(t *testing.T) {
	root := moduleRoot(t)
	content := readOnboardingScript(t, root, provisionTenantRelPath)
	readOnboardingScript(t, root, tenantctlRelPath) // must exist; failure above if not

	for _, line := range executedLines(content) {
		if strings.Contains(line, "tenantctl.sh") {
			return
		}
	}
	t.Fatalf("%s has no live (non-comment) line invoking tenantctl.sh — the golden path must "+
		"COMPOSE the infrastructure lifecycle script as its `infra` step (see Task 1), not just "+
		"describe the composition in prose. Add a step that runs `tenantctl.sh onboard`, or a "+
		"tenant provisioned this way gets no NATS account under SEC-M3 and cannot publish",
		provisionTenantRelPath)
}

// TestProvisionTenantDoesNotWriteGatewayQuota asserts tenantctl.sh remains
// the sole owner of the api-gateway-quotas ConfigMap (MT-01e). A re-added
// write from the golden path is the duplicate-write half of ONBOARD-M1: two
// scripts racing to set the same tenant's rate limit.
func TestProvisionTenantDoesNotWriteGatewayQuota(t *testing.T) {
	root := moduleRoot(t)
	content := readOnboardingScript(t, root, provisionTenantRelPath)

	for _, line := range executedLines(content) {
		if strings.Contains(line, retiredGatewayQuotaCM) {
			t.Fatalf("%s has a live line touching %s: %q — tenantctl.sh is the sole owner of the "+
				"gateway quota ConfigMap (MT-01e). Remove this write from the golden path; its "+
				"`infra` step (tenantctl.sh onboard) already sets the tenant's quota, and a second "+
				"writer means whichever script runs last decides the tenant's budget",
				provisionTenantRelPath, retiredGatewayQuotaCM, strings.TrimSpace(line))
		}
	}
}

// TestProvisionTenantDoesNotClaimNoStreamNeeded pins the specific false
// statement SEC-M3 invalidated so it cannot return — in code or in prose.
// Unlike the two checks above, this one deliberately does NOT restrict
// itself to live lines: "no per-tenant stream needed" is wrong about the
// architecture regardless of whether it sits in a comment or a running
// command, so a comment resurrecting it is exactly as dangerous as code
// that does, and must fail the build too.
func TestProvisionTenantDoesNotClaimNoStreamNeeded(t *testing.T) {
	root := moduleRoot(t)
	content := readOnboardingScript(t, root, provisionTenantRelPath)

	if strings.Contains(strings.ToLower(content), retiredNoStreamClaim) {
		t.Fatalf("%s contains the retired claim %q — SEC-M3 made this false: the production broker "+
			"requires a client SVID mapped to a NATS account, so a tenant onboarded without "+
			"tenantctl.sh's broker step gets no account and cannot publish. This check intentionally "+
			"covers comments as well as executed lines — remove the statement entirely, it does not "+
			"belong in this file in any form",
			provisionTenantRelPath, retiredNoStreamClaim)
	}
}
