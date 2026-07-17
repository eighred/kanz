package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
// guard use AST instead of grep for Go.
//
// A first version of the invocation check narrowed further by name-blacklisting
// `echo` lines, on the theory that the only false-positive risk was the status
// line provision-tenant.sh prints before the real invocation. Two reviewers
// showed that was false-green: swap the real invocation for
// `printf "would run tenantctl.sh onboard\n"` — or a `log_step` call, a heredoc
// body, or a bare assignment naming the script — and the blacklist doesn't
// touch any of them, because none of them is spelled "echo". A blacklist can
// only ever exclude the one non-invocation spelling its author thought of. The
// fix is to stop blacklisting non-invocations and instead require a positive
// match on what an invocation actually looks like: a shell (`bash`/`sh`)
// running tenantctl.sh with the `onboard` argument — see
// tenantctlInvocationPattern. A `printf`, `log_step`, or quoted string that
// merely mentions "tenantctl.sh onboard" cannot match that pattern, because
// none of them is preceded by a shell invoking it; no echo-blacklist is needed
// on top, and keeping one beside the positive match would just be a second,
// weaker mechanism doing nothing the first doesn't already do correctly.
//
// The retired-claim check is the deliberate exception: "no per-tenant stream
// needed" is a false statement about the ARCHITECTURE, and would be exactly as
// false and exactly as dangerous resurrected in a comment as in a live line,
// so it is checked against the raw file text, comments included.
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

// tenantctlInvocationPattern matches a live line that actually RUNS
// tenantctl.sh with the onboard argument, as opposed to a line that merely
// mentions those words. It requires a shell (bash or sh) invoking a path
// ending in tenantctl.sh with the onboard argument — `\b` on both ends of the
// shell-name alternation so it matches "bash" as a whole word (not the "sh"
// inside some unrelated word) and so a prefix like `TENANT="$TENANT" ` before
// `bash` does not stop the match. It deliberately does NOT match a `printf`,
// `log_step`, heredoc body, or quoted string naming "tenantctl.sh onboard" —
// none of those is a shell invoking the script, so none of them is preceded
// by "bash "/"sh " immediately before the path. That is what makes this a
// positive assertion of "this line runs the script" rather than a blacklist
// of one non-invocation spelling: it needs no echo-exclusion alongside it,
// because nothing that merely prints or assigns can satisfy it.
var tenantctlInvocationPattern = regexp.MustCompile(`\b(?:bash|sh)\s+\S*tenantctl\.sh\s+onboard\b`)

// TestProvisionTenantInvokesTenantctl asserts the composition Task 1 built
// is real, not aspirational: the golden path must contain a live line that
// actually runs tenantctl.sh, not merely a comment or status message naming
// it. tenantctl.sh itself must also exist and be readable — invoking a
// script that is not there is not a composition, it is a dangling reference.
func TestProvisionTenantInvokesTenantctl(t *testing.T) {
	root := moduleRoot(t)
	content := readOnboardingScript(t, root, provisionTenantRelPath)
	readOnboardingScript(t, root, tenantctlRelPath) // must exist; failure above if not

	for _, line := range liveLines(content) {
		if tenantctlInvocationPattern.MatchString(line) {
			return
		}
	}
	t.Fatalf("%s has no live line matching %q — the golden path must COMPOSE the infrastructure "+
		"lifecycle script as its `infra` step (see Task 1) by actually RUNNING it, not just print or "+
		"name it. Add a step that runs `bash ../tenancy/tenantctl.sh onboard` (a status echo naming "+
		"the step does not satisfy this — the line must invoke the script), or a tenant provisioned "+
		"this way gets no NATS account under SEC-M3 and cannot publish",
		provisionTenantRelPath, tenantctlInvocationPattern.String())
}

// TestProvisionTenantDoesNotWriteGatewayQuota asserts tenantctl.sh remains
// the sole owner of the api-gateway-quotas ConfigMap (MT-01e). A re-added
// write from the golden path is the duplicate-write half of ONBOARD-M1: two
// scripts racing to set the same tenant's rate limit.
func TestProvisionTenantDoesNotWriteGatewayQuota(t *testing.T) {
	root := moduleRoot(t)
	content := readOnboardingScript(t, root, provisionTenantRelPath)

	for _, line := range liveLines(content) {
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

	// Case-insensitive on both sides, deliberately: this is a prose match, and
	// prose capitalizes ("No per-tenant stream needed." at a sentence start)
	// in ways code never does. Lowercasing only content while relying on
	// retiredNoStreamClaim happening to already be lowercase is a silent trap
	// the day someone edits that constant to start with a capital; lowering
	// both sides makes the case-insensitivity an explicit, self-maintaining
	// property of the comparison instead of an accident of the constant's
	// current spelling.
	if strings.Contains(strings.ToLower(content), strings.ToLower(retiredNoStreamClaim)) {
		t.Fatalf("%s contains the retired claim %q — SEC-M3 made this false: the production broker "+
			"requires a client SVID mapped to a NATS account, so a tenant onboarded without "+
			"tenantctl.sh's broker step gets no account and cannot publish. This check intentionally "+
			"covers comments as well as executed lines — remove the statement entirely, it does not "+
			"belong in this file in any form",
			provisionTenantRelPath, retiredNoStreamClaim)
	}
}

// probeTenant is an obviously-fake tenant name for the runtime guard below —
// it must never collide with a real tenant, and its distinctiveness lets the
// REFUSED-message assertion pin the exact tenant name tenantctl.sh echoes
// back, not just the word "REFUSED" in isolation.
const probeTenant = "onboard-m3-runtime-guard-probe"

// TestTenantctlOnboardRefusesWithoutPrerequisites is the RUNTIME counterpart
// to the static guards above. Every other check in this file reads the
// scripts as TEXT — grep with extra steps. That is exactly why ONBOARD-M1
// (and M3) could exist in the first place: tenantctl.sh, pre-Task-1, parsed
// perfectly fine and STILL printed "tenant X onboarded" after silently
// falling back to a "static mode" log line for NATS (no nsc/NATS_OPERATOR)
// and silently skipping the Postgres role (no ADMIN_DATABASE_URL). No static
// check — however sophisticated — can see a script lie about what it did at
// runtime; only running it can. That is the entire reason the ONBOARD epic
// exists: nobody had ever run these two scripts together and watched what
// they actually did.
//
// So this test executes `bash tenantctl.sh onboard` for real, against a
// deliberately narrow environment, and pins the three properties Task 1's
// preflight (see tenantctl.sh's preflight()) exists to guarantee:
//
//  1. it exits non-zero, and specifically 2 (REFUSED) — not some other
//     non-zero code that would mean the script failed for the WRONG reason
//     (e.g. 127 "no such file", which would mean this test isn't even
//     exercising the real script);
//  2. the output names the prerequisites that are actually missing in a
//     clean environment (no NATS_OPERATOR, no TENANTCTL_MANUAL_* escape
//     declared) — ADMIN_DATABASE_URL is deliberately NOT among them: as of
//     Task 1 that credential binds only the offboard purge path (see
//     TestTenantctlOffboardPurgeRefusesWithoutAdminDSN), and the negative
//     assertion further down this function pins its absence here; and
//  3. the word "onboarded" NEVER appears — that is the literal lie
//     ONBOARD-M3 fixed (finish() printing success, or a PARTIAL line naming
//     the tenant as "onboarded", while steps were silently skipped).
//
// (3) is the load-bearing assertion: (1) and (2) establish the script ran
// and refused for the documented reason, but (3) is what makes a future
// regression that reintroduces ANY path to the success/partial line —
// whether by neutering preflight or restoring a silent skip inside
// onboard_nats — fail this test, because that is precisely the lie a
// paying tenant would receive.
//
// Because preflight runs before onboard_identity (the first step that would
// ever call kubectl) and only ever shells out to `command -v`, a passing run
// of this test never reaches kubectl, psql, or nsc — nothing here touches a
// cluster or a database. That property depends entirely on preflight
// actually running first, which is exactly what this test verifies; it is
// not separately enforced by, say, skipping when kubectl is unreachable —
// doing that would make the guard vacuous on any CI box that lacks a
// cluster, which is the opposite of what this test is for.
//
// Belt-and-suspenders on top of that: cmd.Env below also pins KUBECONFIG to a
// path that cannot exist. This was not a hypothetical — the first version of
// this test omitted it and, while proving out the "preflight returns 0
// unconditionally" mutation below by hand, kubectl (invoked by the mutated
// onboard_identity) inherited a real ~/.kube/config anyway and created a
// namespace, ServiceAccounts, and a Job on the real kind-kanz-dryrun cluster
// reachable from this box — because bash on this host computes its own HOME
// at startup independent of whatever environment its parent process passed
// it, and kubectl then found that HOME's default kubeconfig. A correct
// preflight makes this moot in the passing case, but a regression is exactly
// what this test exists to catch, and a regression that also reaches a real
// cluster is strictly worse than one that doesn't. Pinning KUBECONFIG to a
// nonexistent path makes kubectl fail on `Stat` before it ever dials
// anything, whether preflight is correct or not.
func TestTenantctlOnboardRefusesWithoutPrerequisites(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH (%v) — this guard runs tenantctl.sh for real and has no other way to "+
			"do that; install bash (git-bash on Windows, or any POSIX bash on Linux/macOS CI) to enable it", err)
	}

	root := moduleRoot(t)
	readOnboardingScript(t, root, tenantctlRelPath) // must exist and be non-empty before we bother invoking it

	// The environment is built explicitly — never os.Environ() — because an
	// engineer who happens to have ADMIN_DATABASE_URL, NATS_OPERATOR, or a
	// TENANTCTL_MANUAL_* flag exported in their own shell would silently make
	// preflight succeed here and this test would stop proving anything about
	// a clean environment. Three variables are passed:
	//   - PATH, so bash can find its own shell builtins/coreutils, and so
	//     whatever `jq`/`kubectl`/`nsc` this box does or doesn't have on PATH
	//     is free to vary machine-to-machine — deliberately NOT asserted on
	//     below, since which of those happen to be installed here is not
	//     what this guard is pinning.
	//   - TENANT, set to an obviously-fake probe value.
	//   - KUBECONFIG, pinned to a path inside t.TempDir() that is never
	//     created. See the doc comment above for why this line exists: it is
	//     not optional. Without it, a regressed preflight lets kubectl fall
	//     back to whatever real kubeconfig this host's HOME resolves to.
	// Everything ONBOARD's preflight actually cares about beyond that
	// (NATS_OPERATOR, TENANTCTL_MANUAL_NATS) is simply absent, which is the
	// "deliberately empty environment" this test is required to exercise.
	// ADMIN_DATABASE_URL and TENANTCTL_MANUAL_DB are also absent here, but
	// that is incidental, not load-bearing: onboard opens no psql connection,
	// so preflight is indifferent to both on this path — see the negative
	// assertion below, which pins exactly that. They bind only the offboard
	// purge path (PURGE_ROWS=1); see
	// TestTenantctlOffboardPurgeRefusesWithoutAdminDSN for where they're
	// actually exercised. TENANT_DB_PASSWORD no longer exists anywhere in the
	// script at all.
	unreachableKubeconfig := filepath.Join(t.TempDir(), "kubeconfig-does-not-exist")
	cmd := exec.Command(bashPath, tenantctlRelPath, "onboard")
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"TENANT=" + probeTenant,
		"KUBECONFIG=" + unreachableKubeconfig,
	}
	out, err := cmd.CombinedOutput()
	output := string(out)

	if err == nil {
		t.Fatalf("tenantctl.sh onboard exited 0 against a bare PATH+TENANT environment — Task 1's "+
			"preflight must REFUSE when NATS_OPERATOR/ADMIN_DATABASE_URL are unset and no "+
			"TENANTCTL_MANUAL_* escape is declared, never silently succeed. Output:\n%s", output)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("tenantctl.sh onboard did not even start (%v) — this proves nothing about preflight; "+
			"fix the invocation (bash %q, dir %q) rather than the script", err, tenantctlRelPath, root)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("tenantctl.sh onboard exited %d, want 2 (REFUSED) — any other non-zero code (127 'no "+
			"such file', 3 PARTIAL, ...) means the script failed for the WRONG reason, not because "+
			"preflight refused a clean environment. Output:\n%s", exitErr.ExitCode(), output)
	}

	wantSubstrings := []string{
		"REFUSED: cannot onboard tenant '" + probeTenant + "'",
		"nsc on PATH + NATS_OPERATOR set",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(output, want) {
			t.Fatalf("tenantctl.sh onboard output missing %q — a clean environment (no NATS_OPERATOR, "+
				"no TENANTCTL_MANUAL_NATS escape) must name it as a missing prerequisite before "+
				"refusing. Output:\n%s", want, output)
		}
	}

	// Onboard performs NO database work: the per-tenant role this script used to
	// mint isolated nothing (every RLS policy keys only on the app.tenant_id GUC,
	// and FORCE RLS already binds the owner), and provision-tenant.sh's storage
	// step creates nothing per tenant by design. So preflight must NOT demand a
	// DB credential to onboard. Naming one here would be a refusal that lies
	// about its own cause — the ONBOARD-M3 defect inverted: that one lied about
	// success, this would lie about why it failed. Both mislead the operator.
	for _, unwanted := range []string{"ADMIN_DATABASE_URL", "TENANT_DB_PASSWORD"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("tenantctl.sh onboard refusal names %q — onboard opens no psql connection, so a "+
				"DB credential is not one of its prerequisites. The DB prereq belongs ONLY to the "+
				"offboard purge path (see TestTenantctlOffboardPurgeRefusesWithoutAdminDSN). "+
				"Output:\n%s", unwanted, output)
		}
	}

	// The load-bearing assertion (see doc comment above): no matter how
	// preflight regresses, "onboarded" reappearing in the output IS the M3
	// defect, full stop.
	if strings.Contains(strings.ToLower(output), "onboarded") {
		t.Fatalf("tenantctl.sh onboard output contains \"onboarded\" despite a non-zero, REFUSED exit — "+
			"this is the exact lie ONBOARD-M3 fixed: printing a success or PARTIAL-success line naming "+
			"the tenant as onboarded while required prerequisites are missing. Output:\n%s", output)
	}
}

// offboardProbeTenant is a distinct probe name from probeTenant above: this
// test exercises the OFFBOARD path, and sharing a constant would make a future
// failure message imply the two guards share a run or a tenant.
const offboardProbeTenant = "tenantctl-offboard-purge-guard-probe"

// TestTenantctlOffboardPurgeRefusesWithoutAdminDSN pins where the DB credential
// prerequisite lives after the per-tenant role was retired.
//
// The role (kanz_tenant_<tenant>) was deleted because it was dead in every
// sense: nothing connected as it (services take per-service DSNs from Vault,
// authorized by SPIFFE), it isolated nothing (every RLS policy keys only on
// current_setting('app.tenant_id'); current_user appears in no policy; FORCE
// RLS already binds the owner), and the password it minted was persisted to no
// secret store. Deleting it removed a credential and no boundary.
//
// That left exactly ONE path in this script that opens a psql connection:
// offboard with PURGE_ROWS=1. This test pins both halves of the resulting rule,
// because each half fails differently and both failures harm an operator:
//
//   - PURGE_ROWS=1 without ADMIN_DATABASE_URL must REFUSE (exit 2) and name the
//     credential. Losing this means a purge silently doing nothing while
//     reporting success — a departed tenant's rows kept forever, believed gone.
//   - PURGE_ROWS unset must NOT name a DB credential. Demanding one for a run
//     that touches no database is a refusal that lies about its cause.
//
// Like the onboard guard above, this runs the real script: cmd.Env is built
// explicitly (never os.Environ(), which would let an engineer's exported
// ADMIN_DATABASE_URL make this vacuous) and KUBECONFIG is pinned to a path that
// cannot exist (see that test's doc comment — kubectl reaching a real cluster
// from a guard was not hypothetical here).
func TestTenantctlOffboardPurgeRefusesWithoutAdminDSN(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH (%v) — this guard runs tenantctl.sh for real and has no other way to "+
			"do that; install bash (git-bash on Windows, or any POSIX bash on Linux/macOS CI) to enable it", err)
	}

	root := moduleRoot(t)
	readOnboardingScript(t, root, tenantctlRelPath) // must exist and be non-empty before we bother invoking it

	run := func(t *testing.T, purgeRows string) (string, int) {
		t.Helper()
		unreachableKubeconfig := filepath.Join(t.TempDir(), "kubeconfig-does-not-exist")
		cmd := exec.Command(bashPath, tenantctlRelPath, "offboard")
		cmd.Dir = root
		env := []string{
			"PATH=" + os.Getenv("PATH"),
			"TENANT=" + offboardProbeTenant,
			"KUBECONFIG=" + unreachableKubeconfig,
			// TENANTCTL_MANUAL_NATS keeps this test focused on the DB prereq:
			// without it, a missing NATS_OPERATOR refuses first and this guard
			// would pass even if the DB rule were deleted outright.
			"TENANTCTL_MANUAL_NATS=true",
		}
		if purgeRows != "" {
			env = append(env, "PURGE_ROWS="+purgeRows)
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("tenantctl.sh offboard did not even start (%v) — this proves nothing about preflight; "+
				"fix the invocation (bash %q, dir %q) rather than the script", err, tenantctlRelPath, root)
		}
		return string(out), exitErr.ExitCode()
	}

	t.Run("PURGE_ROWS=1 without ADMIN_DATABASE_URL refuses", func(t *testing.T) {
		output, code := run(t, "1")
		if code != 2 {
			t.Fatalf("tenantctl.sh offboard PURGE_ROWS=1 exited %d, want 2 (REFUSED) — PURGE_ROWS=1 is the "+
				"one path in this script that opens a psql connection, so a missing ADMIN_DATABASE_URL "+
				"(with no TENANTCTL_MANUAL_DB escape declared) must refuse before anything is touched. "+
				"Output:\n%s", code, output)
		}
		if !strings.Contains(output, "ADMIN_DATABASE_URL") {
			t.Fatalf("tenantctl.sh offboard PURGE_ROWS=1 refused without naming ADMIN_DATABASE_URL — the "+
				"operator asked for rows to be deleted and must be told exactly which credential is "+
				"missing, not merely that something is. Output:\n%s", output)
		}
	})

	t.Run("PURGE_ROWS unset does not demand a DB credential", func(t *testing.T) {
		output, _ := run(t, "")
		if strings.Contains(output, "ADMIN_DATABASE_URL") {
			t.Fatalf("tenantctl.sh offboard without PURGE_ROWS names ADMIN_DATABASE_URL — with rows kept "+
				"for audit (the default) this run opens no psql connection and has no DB prerequisite. "+
				"Demanding a credential for work that never happens is a refusal that misstates its "+
				"cause. Output:\n%s", output)
		}

		// The assertion above proves an ABSENCE (ADMIN_DATABASE_URL not named),
		// and an absence assertion is only meaningful if the run actually
		// reached the code that would have named it. A script that dies before
		// ever reaching preflight — exit 127, a syntax error, a bad invocation —
		// emits no such string either, and would pass this subtest vacuously
		// while proving nothing. This anchors on tenant-specific output instead,
		// so a dead-before-preflight run fails loudly here rather than passing
		// silently above. Two environments, one anchor: with jq absent (this
		// box, and any dev box without it) preflight itself refuses with
		// "REFUSED: cannot offboard tenant '<tenant>' — ...", naming the
		// tenant; with jq present (CI) preflight passes and offboard_quota
		// immediately logs ">> [quota] removing budget <tenant>", also naming
		// the tenant. Either way this string must appear if the run got past
		// process startup and into the script's real logic.
		if !strings.Contains(output, offboardProbeTenant) {
			t.Fatalf("tenantctl.sh offboard without PURGE_ROWS produced no output naming %q — this proves "+
				"the run never reached tenant-specific output (preflight's refusal or offboard_quota's "+
				"log line), so the ADMIN_DATABASE_URL absence check above may have passed vacuously "+
				"because the script died before ever reaching preflight, not because the DB rule is "+
				"correct. Output:\n%s", offboardProbeTenant, output)
		}
	})
}

// verifyProbeTenant is a second, distinct probe name from probeTenant above —
// this test exercises provision-tenant.sh's verify step, not tenantctl.sh's
// onboard step, and giving it its own constant keeps a future failure
// message from implying the two runtime guards share a tenant or a script.
const verifyProbeTenant = "onboard-m2-verify-gate-probe"

// TestProvisionTenantVerifyRefusesWithoutBearerTokens is the RUNTIME
// counterpart to ONBOARD-M2 Task 1, in the same spirit as
// TestTenantctlOnboardRefusesWithoutPrerequisites above: a static guard can
// only confirm the verify step's shell text PARSES; it cannot see that the
// step LIES at runtime. That is exactly what this gate did for the entire
// life of the project — before Task 1, the "verify cross-tenant isolation"
// step sent no bearer token at all and read an X-Kanz-Tenant header the
// gateway never even looks at, so every run printed "isolation holds"
// unconditionally. Nobody noticed, because no tenant had ever been
// onboarded through this path. A comment saying "this step now checks real
// tokens" is exactly as capable of silently rotting back to that state as
// the composition Task 1 fixed in TestProvisionTenantInvokesTenantctl above;
// only executing the script proves the refusal is real.
//
// This test runs `sh provision-tenant.sh` with STEP=verify and a
// deliberately bare environment carrying neither VERIFY_TOKEN nor
// VERIFY_TOKEN_OTHER, and pins three properties of verify_preflight (see the
// script's header comment and verify_preflight function):
//
//  1. it exits non-zero, and specifically 2 (REFUSED) — not some other
//     non-zero code that would mean the script failed for the WRONG reason
//     (127 "no such file", a shell parse error, ...), which would prove
//     nothing about the gate itself;
//  2. the output names BOTH missing prerequisites — VERIFY_TOKEN and
//     VERIFY_TOKEN_OTHER — so a regression that checks only one of the two
//     tokens still fails this test; and
//  3. the string "isolation holds" NEVER appears — that is the literal lie
//     this gate existed to stop telling: the pre-Task-1 script printed
//     exactly that line after probing with no identity and reading a 401 as
//     success.
//
// (3) is the load-bearing assertion, mirroring the "onboarded" check on
// tenantctl.sh above: (1) and (2) establish the script ran and refused for
// the documented reason, but (3) is what fails a future regression that
// reintroduces ANY path to "isolation holds" printing without both tokens
// present — whether by neutering verify_preflight, moving its call after
// the curl probes, or reintroducing the old unauthenticated-401-as-pass
// logic.
//
// STEP=verify gates off steps 1-4 entirely (see the step() helper), so a
// passing run of this test never reaches the storage check, tenantctl.sh,
// the policy/seed steps, or (within the verify step itself) the curl calls —
// verify_preflight runs and exits 2 before any of GW/curl is touched. That
// property depends on verify_preflight actually running first inside the
// verify step, which is exactly what this test verifies; it is not
// separately enforced by skipping when a cluster or gateway is unreachable —
// doing that would make the guard vacuous on any CI box without one, which
// is the opposite of what this test is for.
//
// Belt-and-suspenders on top of that, matching the tenantctl.sh guard above:
// KUBECONFIG is pinned to a path inside t.TempDir() that is never created.
// This step never calls kubectl at all (steps 1 and 2, the only ones that
// would, are STEP-gated off), so this is defense in depth against a future
// regression that moves the verify_preflight call, not a property this
// specific step currently depends on — but the tenantctl.sh guard's history
// (a real kind-kanz-dryrun cluster was mutated by accident once already,
// because bash recomputes HOME independent of the env passed to it) is
// reason enough to pin it here unconditionally rather than reason about
// whether the current script needs it.
func TestProvisionTenantVerifyRefusesWithoutBearerTokens(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not on PATH (%v) — this guard runs provision-tenant.sh for real and has no other way "+
			"to do that; install sh (git-bash on Windows, or any POSIX sh on Linux/macOS CI) to enable it", err)
	}

	root := moduleRoot(t)
	readOnboardingScript(t, root, provisionTenantRelPath) // must exist and be non-empty before we bother invoking it

	// Built explicitly, never os.Environ() — an engineer who happens to have
	// VERIFY_TOKEN or VERIFY_TOKEN_OTHER exported in their own shell would
	// silently make verify_preflight succeed here, and this test would stop
	// proving anything about a bare environment. Four variables are passed:
	//   - PATH, so sh can find its own shell builtins/coreutils.
	//   - TENANT, set to an obviously-fake probe value.
	//   - STEP=verify, so only step 5 runs — steps 1-4 (storage check,
	//     tenantctl.sh, policy, seed) never execute.
	//   - KUBECONFIG, pinned to a path inside t.TempDir() that is never
	//     created — see the doc comment above for why this is pinned even
	//     though this step does not currently call kubectl.
	// VERIFY_TOKEN and VERIFY_TOKEN_OTHER are simply absent, which is the
	// "deliberately bare environment" this test is required to exercise.
	unreachableKubeconfig := filepath.Join(t.TempDir(), "kubeconfig-does-not-exist")
	cmd := exec.Command(shPath, provisionTenantRelPath)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"TENANT=" + verifyProbeTenant,
		"STEP=verify",
		"KUBECONFIG=" + unreachableKubeconfig,
	}
	out, err := cmd.CombinedOutput()
	output := string(out)

	if err == nil {
		t.Fatalf("provision-tenant.sh (STEP=verify) exited 0 against an environment with no VERIFY_TOKEN "+
			"or VERIFY_TOKEN_OTHER — Task 1's verify_preflight must REFUSE when either bearer token is "+
			"unset, never silently proceed to probe or declare isolation held. Output:\n%s", output)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("provision-tenant.sh (STEP=verify) did not even start (%v) — this proves nothing about "+
			"verify_preflight; fix the invocation (sh %q, dir %q) rather than the script",
			err, provisionTenantRelPath, cmd.Dir)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("provision-tenant.sh (STEP=verify) exited %d, want 2 (REFUSED) — any other non-zero code "+
			"(1 FATAL from a curl probe that should never have run, 127 'no such file', ...) means the "+
			"script failed for the WRONG reason, not because verify_preflight refused a bare environment. "+
			"Output:\n%s", exitErr.ExitCode(), output)
	}

	wantSubstrings := []string{
		"REFUSED: cannot verify cross-tenant isolation for '" + verifyProbeTenant + "'",
		"VERIFY_TOKEN (bearer token for tenant",
		"VERIFY_TOKEN_OTHER (bearer token for any OTHER existing tenant",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(output, want) {
			t.Fatalf("provision-tenant.sh (STEP=verify) output missing %q — a bare environment (no "+
				"VERIFY_TOKEN, no VERIFY_TOKEN_OTHER) must name BOTH as missing prerequisites before "+
				"refusing. Output:\n%s", want, output)
		}
	}

	// The load-bearing assertion (see doc comment above): no matter how
	// verify_preflight regresses, "isolation holds" reappearing in the
	// output IS the pre-Task-1 defect — the gate proving nothing and lying
	// about it — full stop.
	if strings.Contains(output, "isolation holds") {
		t.Fatalf("provision-tenant.sh (STEP=verify) output contains \"isolation holds\" despite a "+
			"non-zero, REFUSED exit — this is the exact lie ONBOARD-M2 Task 1 fixed: the verify step "+
			"must never claim isolation holds when it never even ran a probe. Output:\n%s", output)
	}
}

// storageProbeTenant is a third, distinct probe name — this test exercises
// provision-tenant.sh's storage step (STEP=storage), not tenantctl.sh's
// onboard step or the verify step's bearer-token gate, and giving it its own
// constant keeps a future failure message from implying this guard shares a
// tenant or a script with either of the other two runtime guards.
const storageProbeTenant = "onboard-m4-storage-diagnosis-probe"

// TestProvisionTenantStorageDistinguishesCheckFailureFromRLSOff is the RUNTIME
// counterpart to ONBOARD-M4 Task 1, in the same spirit as the two runtime
// guards above: a static guard can confirm the storage step's shell text
// captures `out` instead of discarding it into `>/dev/null`, but it cannot see
// that `psql -tAc` exits 0 on a query that legitimately returns zero rows —
// that is a runtime fact about a program this repo does not own. Before
// Task 1, the storage step piped that query straight to `>/dev/null` and
// branched on `$? ` alone, so "kubectl/psql could not even run" and "the
// query ran fine and FORCE RLS is off" were the exact same observable event:
// both exit 0, so both PASSED. A tenant provisioned against an unreachable
// database or pod would have sailed through step 1 believing RLS was active
// when nothing had actually been checked.
//
// This test executes `sh provision-tenant.sh` with STEP=storage, TENANT set
// to an obviously-fake probe, and KUBECONFIG pinned to a path that cannot
// exist — so `kubectl exec` (and therefore `psql`) cannot run at all. That is
// deliberately the "check could not be run" branch (line 142), not the "ran
// fine, found nothing" branch (line 143): this box has no live kanz-risk/
// kanz-books pods to query in the first place, so the only way to reach step
// 1 without a real cluster is to make the check itself fail to execute, which
// is exactly the failure mode Task 1 exists to separate from "RLS is off".
//
// Pinned properties:
//
//  1. non-zero exit — specifically 1, matching the storage step's `exit 1`
//     on the could-not-run branch (line 142). Any other exit code would mean
//     this test reached a different failure path than the one Task 1 fixed.
//  2. the output says the RLS check could not be run — Task 1's diagnostic
//     text distinguishing "could not run" from "the check failed".
//  3. the output does NOT contain "FORCE RLS not active" — that is the
//     misdiagnosis Task 1 retired: a pre-Task-1 script (or a regression that
//     restores `>/dev/null` piping) cannot reach line 143's message from a
//     kubectl/psql failure, because line 143 only runs after `out` is
//     successfully captured. Its presence here would mean the could-not-run
//     branch stopped being taken and the vacuous form is back.
//
// (3) is the load-bearing assertion, mirroring "onboarded" and "isolation
// holds" in the two guards above: (1) and (2) establish the script ran and
// took the could-not-run branch, but (3) is what fails a future regression
// that reintroduces ANY path from a kubectl/psql failure to the RLS-off
// message — whether by restoring `out="$(... )" >/dev/null` and collapsing
// both branches back into one `||`, or by any other means of losing the
// captured output.
//
// KUBECONFIG is pinned to a path inside t.TempDir() that is never created,
// exactly as in the two runtime guards above, and for the identical reason: a
// real kind cluster (kind-kanz-dryrun) is reachable from this box and is the
// user's, and an earlier agent mutated it by accident proving out a mutation
// of the tenantctl.sh guard, because bash recomputes HOME at startup
// independent of whatever environment its parent process passed it, so
// kubectl found that HOME's default kubeconfig despite an env that never
// included it. This guard never reaches namespace/ServiceAccount/Job
// creation the way that one could — `kubectl exec` against a pinned,
// nonexistent KUBECONFIG fails on `Stat` before dialing anything — but the
// pin costs nothing and removes any dependence on this box's ambient
// kubeconfig for a passing result. No kubectl-skip-guard is added alongside
// it: skipping when kubectl is unreachable would make this test vacuous on
// any CI box without one, which is the opposite of what it is for — the
// KUBECONFIG pin is what keeps the test deterministic AND off the real
// cluster, not a substitute for actually running the script.
func TestProvisionTenantStorageDistinguishesCheckFailureFromRLSOff(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not on PATH (%v) — this guard runs provision-tenant.sh for real and has no other way "+
			"to do that; install sh (git-bash on Windows, or any POSIX sh on Linux/macOS CI) to enable it", err)
	}

	root := moduleRoot(t)
	readOnboardingScript(t, root, provisionTenantRelPath) // must exist and be non-empty before we bother invoking it

	// Built explicitly, never os.Environ() — see the identical rationale on
	// the two runtime guards above: an engineer's own shell could carry state
	// (e.g. an already-exported KUBECONFIG pointing at a real cluster) that
	// would change what this test actually exercises. Four variables are
	// passed:
	//   - PATH, so sh can find its own shell builtins/coreutils, and so
	//     whatever `kubectl` this box does or doesn't have on PATH is free to
	//     vary machine-to-machine.
	//   - TENANT, set to an obviously-fake probe value.
	//   - STEP=storage, so only step 1 runs — steps 2-5 (tenantctl.sh,
	//     policy, seed, verify) never execute.
	//   - KUBECONFIG, pinned to a path inside t.TempDir() that is never
	//     created, so `kubectl exec` cannot reach any cluster, real or
	//     otherwise — see the doc comment above.
	//   - RISK_DB_NAME / BOOKS_DB_NAME, set to arbitrary placeholders. The
	//     storage step refuses (exit 2) before any kubectl runs if either is
	//     unset (it will not guess a database name — see provision-tenant.sh's
	//     header); this test exercises the DIFFERENT failure mode below it
	//     (kubectl/psql cannot run at all), so both must be set to even reach
	//     that branch.
	unreachableKubeconfig := filepath.Join(t.TempDir(), "kubeconfig-does-not-exist")
	cmd := exec.Command(shPath, provisionTenantRelPath)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"TENANT=" + storageProbeTenant,
		"STEP=storage",
		"KUBECONFIG=" + unreachableKubeconfig,
		"RISK_DB_NAME=probe-risk-db",
		"BOOKS_DB_NAME=probe-books-db",
	}
	out, err := cmd.CombinedOutput()
	output := string(out)

	if err == nil {
		t.Fatalf("provision-tenant.sh (STEP=storage) exited 0 with KUBECONFIG pinned to a nonexistent "+
			"path — kubectl exec cannot possibly have succeeded, so the RLS check could not have run; "+
			"the storage step must FATAL when the check itself fails to execute, never exit 0. Output:\n%s",
			output)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("provision-tenant.sh (STEP=storage) did not even start (%v) — this proves nothing about "+
			"the storage step's diagnosis; fix the invocation (sh %q, dir %q) rather than the script",
			err, provisionTenantRelPath, cmd.Dir)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("provision-tenant.sh (STEP=storage) exited %d, want 1 (the storage step's could-not-run "+
			"FATAL) — any other non-zero code (127 'no such file', ...) means this test failed to start "+
			"or reach the check for the WRONG reason, not because kubectl/psql failed to run against a "+
			"pinned, nonexistent KUBECONFIG. Output:\n%s", exitErr.ExitCode(), output)
	}

	if !strings.Contains(output, "could not run the RLS check") {
		t.Fatalf("provision-tenant.sh (STEP=storage) output missing \"could not run the RLS check\" — "+
			"with kubectl unable to reach any cluster (KUBECONFIG pinned to a nonexistent path), Task 1's "+
			"storage step must say the CHECK could not be run, not stay silent about why it failed or "+
			"claim a verdict it never reached. Output:\n%s", output)
	}

	// The load-bearing assertion (see doc comment above): no matter how the
	// storage step regresses, "FORCE RLS not active" reappearing in output
	// alongside a kubectl/psql failure IS the pre-Task-1 defect — line 143's
	// message can only be reached today after `out` is successfully
	// captured, so its presence here means a regression (most plausibly
	// restoring `out="$(...)" >/dev/null` and collapsing both branches back
	// into the single vacuous `||`) has reintroduced the exact misdiagnosis
	// Task 1 retired: treating "the check could not run" as "RLS is off".
	if strings.Contains(output, "FORCE RLS not active") {
		t.Fatalf("provision-tenant.sh (STEP=storage) output contains \"FORCE RLS not active\" despite "+
			"kubectl/psql having no reachable cluster to query — this is the exact misdiagnosis ONBOARD-M4 "+
			"Task 1 retired: a check that never ran is not a verdict that RLS is off. Output:\n%s", output)
	}
}

// serviceMigrationsRelPath maps a step-1 cluster name — as it appears on the
// left of provision-tenant.sh's `case "$c" in kanz-risk) tables="..." ;;
// kanz-books) tables="..." ;; esac` — to the migrations directory that is the
// SOURCE of the FORCE-RLS table list for that cluster. These are the only two
// clusters ONBOARD-M5 Task 1's storage step checks.
//
// This is deliberately the only place either name is written down for this
// test: the expected table lists themselves are never hardcoded here (that
// would be a THIRD copy of the same fact the migrations and the script
// already each declare once) — they are parsed out of both sources below and
// compared.
var serviceMigrationsRelPath = map[string]string{
	"kanz-risk":  "services/risk-engine/migrations",
	"kanz-books": "services/accounting/migrations",
}

// scriptCaseTablesPattern matches one `kanz-risk) tables="..." ;;` /
// `kanz-books) tables="..." ;;` arm of provision-tenant.sh's case statement.
// It captures the service name and the space-separated table list literal,
// without assuming which tables are in it — the whole point of this guard is
// that the expected set comes from the migrations, not from a list an
// engineer typed into this test.
var scriptCaseTablesPattern = regexp.MustCompile(`(kanz-risk|kanz-books)\)\s+tables="([^"]*)"`)

// scriptForceRLSTables parses provision-tenant.sh's step-1 case statement and
// returns, per cluster name, the table list it declares. A cluster whose arm
// is missing or whose tables="" is empty is absent from the returned map —
// callers must treat that as a failure (see the non-vacuous requirement),
// not silently skip the comparison.
func scriptForceRLSTables(content string) map[string][]string {
	out := map[string][]string{}
	for _, m := range scriptCaseTablesPattern.FindAllStringSubmatch(content, -1) {
		service, list := m[1], m[2]
		if fields := strings.Fields(list); len(fields) > 0 {
			out[service] = fields
		}
	}
	return out
}

// forEachArrayForcePattern matches a `FOREACH t IN ARRAY ARRAY[...] ... END
// LOOP` block — the form services/risk-engine/migrations/0002_tenant_rls.sql
// and services/accounting/migrations/0001_ledger.sql both use to declare
// their FORCE-RLS table sets, one array literal driving a dynamic
// `EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t)` rather than
// one static `ALTER TABLE x FORCE ROW LEVEL SECURITY` per table. Capture
// group 1 is the array literal's contents; group 2 is the loop body, which
// the caller must additionally confirm contains a FORCE ROW LEVEL SECURITY
// statement — a FOREACH loop over the same array shape that never forces RLS
// (e.g. one that only enables it) must not be credited as declaring these
// tables FORCE-RLS.
//
// Non-greedy up to the nearest "END LOOP" rather than a whitespace class
// matching arbitrary text unboundedly — each migration file here has exactly
// one such loop, and stopping at the first END LOOP keeps this from
// accidentally swallowing unrelated SQL that follows in the same file.
var forEachArrayForcePattern = regexp.MustCompile(`(?is)FOREACH\s+\w+\s+IN\s+ARRAY\s+ARRAY\s*\[([^\]]+)\](.*?)END\s+LOOP`)

// forceRLSStmtPattern matches the FORCE ROW LEVEL SECURITY clause itself,
// used both to gate a FOREACH loop body (see forEachArrayForcePattern above)
// and to find static per-table statements
// (directForceAlterTablePattern below). `\s+` throughout, not a literal
// single space: the actual SQL in this repo is `FORCE  ROW LEVEL SECURITY`
// with TWO spaces (services/risk-engine/migrations/0002_tenant_rls.sql,
// services/accounting/migrations/0001_ledger.sql and 0003_venue_account_scope.sql)
// — a pattern hardcoding one space between FORCE and ROW would match none of
// them and this guard would silently parse an empty set from every
// migration, which the non-vacuous check exists to catch but a sloppier
// regex should not have to rely on that safety net to be noticed.
var forceRLSStmtPattern = regexp.MustCompile(`(?i)FORCE\s+ROW\s+LEVEL\s+SECURITY`)

// directForceAlterTablePattern matches a static `ALTER TABLE x FORCE ROW
// LEVEL SECURITY` naming its table directly, as opposed to the dynamic
// `EXECUTE format(..., %I, t)` form inside a FOREACH loop. This is the form
// services/accounting/migrations/0003_venue_account_scope.sql uses for
// ledger_entries — a table ALSO declared via the FOREACH form in 0001. Its
// %I placeholder does not match `\w+` (the `%` is not a word character), so
// this pattern never double-matches the dynamic form's EXECUTE line.
var directForceAlterTablePattern = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)\s+FORCE\s+ROW\s+LEVEL\s+SECURITY`)

// arrayLiteralElemPattern extracts each single-quoted element of a
// `ARRAY['a', 'b', 'c']` literal.
var arrayLiteralElemPattern = regexp.MustCompile(`'([^']+)'`)

// migrationForceRLSTables returns the set (deduplicated, sorted) of table
// names that migrationsRelDir's *.sql files FORCE row-level security on,
// across both the FOREACH-array form and the static ALTER TABLE form. A
// table declared both ways (ledger_entries: FOREACH in 0001, static ALTER in
// 0003) contributes exactly once — the guard against exactly the "same
// table, counted twice, becomes a phantom mismatch" failure mode the brief
// calls out.
func migrationForceRLSTables(t *testing.T, root, migrationsRelDir string) []string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(migrationsRelDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v — this guard cannot verify the FORCE-RLS table set without the "+
			"migrations that declare it", migrationsRelDir, err)
	}

	seen := map[string]bool{}
	var tables []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			tables = append(tables, name)
		}
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s/%s: %v", migrationsRelDir, e.Name(), err)
		}
		content := strings.ReplaceAll(string(b), "\r\n", "\n")

		for _, m := range forEachArrayForcePattern.FindAllStringSubmatch(content, -1) {
			arrayLiteral, body := m[1], m[2]
			if !forceRLSStmtPattern.MatchString(body) {
				continue // a FOREACH ARRAY[...] loop that never forces RLS isn't this declaration
			}
			for _, em := range arrayLiteralElemPattern.FindAllStringSubmatch(arrayLiteral, -1) {
				add(em[1])
			}
		}
		for _, m := range directForceAlterTablePattern.FindAllStringSubmatch(content, -1) {
			add(m[1])
		}
	}

	sort.Strings(tables)
	return tables
}

// TestProvisionTenantStorageTablesMatchMigrations guards against the exact
// defect family this whole epic is about: Task 1 (ONBOARD-M5) made step 1
// name its tenant-scoped tables explicitly instead of a vacuous `limit 1` —
// but it hardcoded those names in provision-tenant.sh's case statement, and
// the migrations that actually FORCE row-level security on those tables
// declare the same fact a second time, independently. Nothing compared the
// two. A migration adding (or renaming, or dropping) a tenant-scoped table
// would leave step 1 silently certifying a stale set: it would keep passing,
// keep printing that isolation is active, while a real table gained or lost
// FORCE RLS with no test failing either way. That is the identical shape of
// ONBOARD-M1 (two scripts disagreeing on the broker), SEC-M3 (code and
// broker config disagreeing), and SEC-M4 (3 of 22 Dockerfiles bumped): one
// fact, two independent copies, nothing checking they agree.
//
// Both sides are parsed from source — never hardcoded here — so this test
// itself cannot become a third, independently-drifting copy of the list.
func TestProvisionTenantStorageTablesMatchMigrations(t *testing.T) {
	root := moduleRoot(t)
	scriptContent := readOnboardingScript(t, root, provisionTenantRelPath)
	scriptTables := scriptForceRLSTables(scriptContent)

	services := make([]string, 0, len(serviceMigrationsRelPath))
	for service := range serviceMigrationsRelPath {
		services = append(services, service)
	}
	sort.Strings(services)

	for _, service := range services {
		migrationsRelDir := serviceMigrationsRelPath[service]

		script := scriptTables[service]
		if len(script) == 0 {
			t.Fatalf("%s declares no (or an empty) tables= list for case %q — step 1 cannot verify "+
				"isolation for a cluster it names no tables for; this is a FAILURE, not a vacuous pass, "+
				"because an empty expected set would make any comparison trivially satisfied",
				provisionTenantRelPath, service)
		}

		migrated := migrationForceRLSTables(t, root, migrationsRelDir)
		if len(migrated) == 0 {
			t.Fatalf("no FORCE ROW LEVEL SECURITY table found under %s — either the migrations that "+
				"force RLS for %q moved/changed shape and this parser no longer finds them, or MT-01d "+
				"was never actually deployed there; either way this is a FAILURE, not a vacuous pass, "+
				"because an empty migrated set would make step 1's check trivially satisfiable no "+
				"matter what it names", migrationsRelDir, service)
		}

		scriptSet := map[string]bool{}
		for _, tb := range script {
			scriptSet[tb] = true
		}
		migSet := map[string]bool{}
		for _, tb := range migrated {
			migSet[tb] = true
		}

		var onlyInScript, onlyInMigrations []string
		for tb := range scriptSet {
			if !migSet[tb] {
				onlyInScript = append(onlyInScript, tb)
			}
		}
		for tb := range migSet {
			if !scriptSet[tb] {
				onlyInMigrations = append(onlyInMigrations, tb)
			}
		}
		sort.Strings(onlyInScript)
		sort.Strings(onlyInMigrations)

		if len(onlyInScript) > 0 || len(onlyInMigrations) > 0 {
			t.Fatalf("case %q FORCE-RLS table sets disagree between %s (tables=%v) and %s (FORCE "+
				"RLS on %v): script names %v that the migrations do not FORCE (step 1 is checking a "+
				"stale/wrong table) and the migrations FORCE %v that the script never checks (a real "+
				"tenant-scoped table step 1 would silently NOT verify) — update whichever side is "+
				"behind so step 1 certifies exactly the tables MT-01d actually forces RLS on, no more "+
				"and no less",
				service, provisionTenantRelPath, script, migrationsRelDir, migrated,
				onlyInScript, onlyInMigrations)
		}
	}
}
