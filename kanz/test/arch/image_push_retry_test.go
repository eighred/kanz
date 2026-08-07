package arch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// THE IMAGE BUILD MUST NOT PUSH, AND THE PUSH MUST BE RETRIED VISIBLY (#320).
//
// kanz-build reddened main on roughly two runs in five: the image BUILT and the
// PUSH was refused by GHCR — a different random 3 of 26 services every time,
// never the same service twice, green on an identical re-run. No code is
// involved, and every occurrence cost a full diagnosis before it could be
// dismissed. That is the actual expense: a red that is not a code break trains
// the reflex to re-run first and read second, which is how a genuine failure
// eventually gets re-run into a merge.
//
// WHAT THIS GUARDS, AND WHY IT IS THE SPLIT RATHER THAN THE RETRY. The dangerous
// version of this fix is a retry wrapped around a combined build-and-push step,
// because it would silently retry COMPILE FAILURES — turning a deterministic red
// into three slow reds, and eventually into someone raising the attempt count
// until something passes. The safety property is that build and push are
// separate steps, so the retry can only ever see a registry failure. That is
// what this guard pins: `docker/build-push-action` must not push.
//
// It also pins the VISIBILITY half, which #320 is emphatic about — "a silent
// retry that hides the rate is a worse outcome than the current noise", because
// its own acceptance ("ten consecutive clean runs") is unanswerable if a retried
// run cannot be told apart from a first-attempt one. So the push step must go
// through the script that counts and reports.
func TestImageWorkflowSeparatesBuildFromPush(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	path := filepath.Join(repoRoot, ".github", "workflows", "build.yml")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read build.yml: %v — the guard cannot check what it cannot read", err)
	}
	src := string(b)

	// NON-VACUITY: if the action is gone or renamed, this guard would otherwise
	// find no `push:` line and pass over a workflow it no longer understands.
	if !strings.Contains(src, "docker/build-push-action") {
		t.Fatal("build.yml no longer uses docker/build-push-action. If the image build moved, MOVE " +
			"THIS GUARD WITH IT — it exists to keep a retry from ever wrapping a compile failure")
	}

	// `push: true`, or the old `push: ${{ github.event_name == 'push' }}`, both
	// mean the action itself publishes — and a retry around that step would
	// retry the build too.
	pushTrue := regexp.MustCompile(`(?m)^\s*push:\s*(true|\$\{\{)`)
	if m := pushTrue.FindString(src); m != "" {
		t.Errorf("build.yml has %q on the build action.\n\n"+
			"The build step must NOT push (#320). Build and push are separate so the push retry can "+
			"never see a build failure — retrying a compile error is the genuinely dangerous version "+
			"of this change, and it degrades into raising the attempt count until something passes.",
			strings.TrimSpace(m))
	}

	if !strings.Contains(src, "push-with-retry.sh") {
		t.Error("build.yml does not push through .github/scripts/push-with-retry.sh.\n\n" +
			"A bare `docker push` would fix nothing: #320's acceptance is ten consecutive clean runs, " +
			"which is unanswerable unless a retried run is distinguishable from a first-attempt one. " +
			"The script is what counts the retries and reports them.")
	}
}

// THE SCRIPT EXISTS, IS EXECUTABLE, AND ITS LOGIC ACTUALLY WORKS.
//
// The guard above only proves the workflow REFERENCES the script. These run it,
// against a stub `docker` that fails on demand — which is the only way to assert
// the three properties that matter without waiting for GHCR to misbehave.
func TestPushRetryScriptBehaviour(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH (%v): the retry script's behaviour cannot be exercised here", err)
	}
	repoRoot := filepath.Dir(moduleRoot(t))
	script := filepath.Join(repoRoot, ".github", "scripts", "push-with-retry.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("push-with-retry.sh is missing: %v", err)
	}

	cases := []struct {
		name string
		// failures is how many times the stub refuses before succeeding;
		// -1 means it never succeeds.
		failures    int
		wantErr     bool
		wantRetries int
	}{
		// The ordinary case: nothing wrong, nothing retried, nothing reported.
		// This is the one that keeps the retry from becoming invisible padding.
		{name: "first attempt succeeds", failures: 0, wantErr: false, wantRetries: 0},
		// The #320 flake: refused once, succeeds on the retry, run stays green
		// AND the retry is counted.
		{name: "one refusal then success", failures: 1, wantErr: false, wantRetries: 1},
		{name: "three refusals then success", failures: 3, wantErr: false, wantRetries: 3},
		// THE PROPERTY THAT KEEPS THIS HONEST: a push refused on every attempt is
		// a real authorization failure and must still fail the job. A retry that
		// eventually gives up quietly would convert a revoked token into a green
		// build that published nothing.
		{name: "always refused still fails", failures: -1, wantErr: true, wantRetries: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := writeDockerStub(t, dir, tc.failures)
			outFile := filepath.Join(dir, "gh_output")

			cmd := exec.Command(bash, script, "ghcr.io/eighred/example:sha-abc")
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(),
				"DOCKER="+stub,
				"MAX_ATTEMPTS=4",
				"RETRY_BASE_DELAY=0", // no real sleeping in a test
				"GITHUB_OUTPUT="+outFile,
				"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"),
			)
			out, err := cmd.CombinedOutput()

			if tc.wantErr && err == nil {
				t.Fatalf("script SUCCEEDED though every push attempt was refused.\n\n"+
					"A persistent refusal is an expired token or a revoked scope, not the #320 flake, "+
					"and must go red — otherwise a build publishes nothing and reports success.\n%s", out)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("script failed after %d recoverable refusal(s): %v\n%s", tc.failures, err, out)
			}

			gotRetries := readRetries(t, outFile, tc.wantErr)
			if gotRetries != tc.wantRetries {
				t.Errorf("reported retries=%d, want %d.\n\n"+
					"#320's acceptance is ten consecutive clean runs, which is only measurable if the "+
					"count is right — an undercount makes a degrading registry look healthy.\n%s",
					gotRetries, tc.wantRetries, out)
			}

			// The count must be VISIBLE in the run, not only in a file.
			if tc.wantRetries > 0 && !strings.Contains(string(out), "::warning") {
				t.Errorf("a retried push emitted no ::warning annotation — the flake becomes invisible "+
					"exactly when it stops being fatal, which #320 calls a worse outcome than the "+
					"current noise.\n%s", out)
			}
			if tc.wantRetries == 0 && !tc.wantErr && strings.Contains(string(out), "::warning") {
				t.Errorf("a clean first-attempt push emitted a warning — that makes the signal "+
					"meaningless.\n%s", out)
			}
		})
	}
}

// EVERY TAG IS PUSHED, NOT JUST THE FIRST.
//
// The real invocation always passes three — `type=sha`, `type=ref,event=branch`
// and `latest` — as a NEWLINE-separated list from the metadata action, splatted
// unquoted so the shell splits it. A loop that stopped after the first tag, or a
// quoting slip that treated all three as one argument, would publish the image
// under one tag and silently leave `latest` pointing at the previous build. That
// is invisible in a green run and shows up as a deploy that pulls stale code.
func TestPushRetryScriptPushesEveryTag(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH (%v)", err)
	}
	repoRoot := filepath.Dir(moduleRoot(t))
	script := filepath.Join(repoRoot, ".github", "scripts", "push-with-retry.sh")

	dir := t.TempDir()
	stub := writeDockerStub(t, dir, 1) // one refusal, to exercise retry + multi-tag together
	pushed := filepath.Join(dir, "pushed")

	tags := []string{
		"ghcr.io/eighred/audit:sha-abc1234",
		"ghcr.io/eighred/audit:main",
		"ghcr.io/eighred/audit:latest",
	}
	cmd := exec.Command(bash, append([]string{script}, tags...)...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"DOCKER="+stub,
		"MAX_ATTEMPTS=4",
		"RETRY_BASE_DELAY=0",
		"PUSHED_LOG="+pushed,
		"GITHUB_OUTPUT="+filepath.Join(dir, "gh_output"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	b, err := os.ReadFile(pushed)
	if err != nil {
		t.Fatalf("read pushed log: %v\n%s", err, out)
	}
	got := string(b)
	for _, tag := range tags {
		if !strings.Contains(got, tag) {
			t.Errorf("tag %q was never pushed.\n\npushed:\n%s\n\nAn image published under some of its "+
				"tags but not `latest` is invisible in a green run and surfaces as a deploy pulling "+
				"the previous build.", tag, got)
		}
	}
}

// writeDockerStub creates a fake `docker` that refuses `push` the first n times.
// n < 0 refuses forever. It counts attempts in a file beside itself, so the count
// survives across the separate process invocations the script makes.
func writeDockerStub(t *testing.T, dir string, failures int) string {
	t.Helper()
	counter := filepath.Join(dir, "attempts")
	stub := filepath.Join(dir, "docker-stub.sh")

	body := fmt.Sprintf(`#!/usr/bin/env bash
n=0
[ -f %[1]q ] && n=$(cat %[1]q)
n=$((n+1))
echo "$n" > %[1]q
failures=%[2]d
if [ "$failures" -lt 0 ] || [ "$n" -le "$failures" ]; then
  # The third of #320's four observed spellings — the one a grep for the literal
  # "403 Forbidden" misses, which is why the script keys on the outcome instead.
  echo 'denied: permission_denied: Error from intermediary with HTTP status code 403 "Forbidden"' >&2
  exit 1
fi
[ -n "${PUSHED_LOG:-}" ] && echo "$2" >> "$PUSHED_LOG"
echo "pushed"
`, counter, failures)

	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatalf("write docker stub: %v", err)
	}
	return stub
}

// readRetries pulls retries=N out of the GITHUB_OUTPUT file the script writes.
func readRetries(t *testing.T, path string, tolerateMissing bool) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if tolerateMissing && os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read GITHUB_OUTPUT: %v", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "retries="); ok {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
				t.Fatalf("retries value %q is not a number: %v", v, err)
			}
			return n
		}
	}
	t.Fatalf("no retries= line in GITHUB_OUTPUT:\n%s", b)
	return 0
}

// The script must be executable, or the workflow step fails on a permission
// error that names the path and not the cause. Git tracks this bit; on Windows
// checkouts it is not meaningful, so the assertion is scoped to where it is.
func TestPushRetryScriptIsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode is not meaningful on a Windows checkout; git's index bit is asserted in CI")
	}
	repoRoot := filepath.Dir(moduleRoot(t))
	info, err := os.Stat(filepath.Join(repoRoot, ".github", "scripts", "push-with-retry.sh"))
	if err != nil {
		t.Fatalf("stat push-with-retry.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("push-with-retry.sh is not executable (%v) — the workflow step would fail with a "+
			"permission error naming the path rather than the cause", info.Mode())
	}
}
