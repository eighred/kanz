package arch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// EVERY WORKFLOW SCRIPT MUST BE EXECUTABLE IN GIT'S INDEX.
//
// THIS GUARD USED TO READ THE FILESYSTEM AND SKIP ON WINDOWS, and that is exactly
// why it did not catch the failure that prompted rewriting it: warm-bases.sh was
// committed 100644, every one of the 26 image jobs died with
//
//	.github/scripts/warm-bases.sh: Permission denied
//	##[error]Process completed with exit code 126
//
// and the guard had passed locally by skipping. `chmod +x` on a Windows checkout
// changes nothing git records, so the filesystem was never the thing to check.
//
// THE INDEX BIT IS. It is what CI checks out, it is meaningful on every platform,
// and it is set with `git update-index --chmod=+x <path>`. So this reads git
// rather than the working tree, covers EVERY script in the directory rather than
// one named file, and does not skip anywhere.
func TestEveryWorkflowScriptIsExecutableInGit(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	out, err := exec.Command("git", "-C", repoRoot, "ls-files", "-s", ".github/scripts").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v — this guard reads the INDEX, not the working tree, because the "+
			"working tree's mode is not what CI checks out", err)
	}

	var checked int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasSuffix(fields[3], ".sh") {
			continue
		}
		checked++
		if fields[0] != "100755" {
			t.Errorf("%s is mode %s in git's index, want 100755.\n\n"+
				"A workflow step running it fails with `Permission denied` and exit 126 — an error "+
				"that names the path and not the cause, on every job at once. Fix with:\n"+
				"    git update-index --chmod=+x %s\n\n"+
				"chmod alone does NOT do this on a Windows checkout, which is how this shipped.",
				fields[3], fields[0], fields[3])
		}
	}
	// NON-VACUITY. If the directory moves, this would otherwise pass having
	// checked nothing.
	if checked < 2 {
		t.Fatalf("found %d shell scripts under .github/scripts — the workflows run more than that, "+
			"so this guard is looking in the wrong place", checked)
	}
}

// THE PULL HALF OF #320 (2026-08-11).
//
// push-with-retry.sh covers a refused PUSH. build.yml's own comments record that
// it structurally cannot cover a refused PULL — it "wraps the PUSH", while a base
// image is fetched inside the build. main went red on exactly that: image
// (wealth), a 403 on a distroless-static blob, with kanz-ci green and the
// Dockerfile unchanged.
//
// warm-bases.sh closes it, and it is safe to retry for the SAME structural reason
// the push retry is: it builds a Dockerfile consisting of the base image and
// nothing else, so no compile error can reach it. These guards keep both halves
// of that true — the step exists, and the script cannot quietly stop failing.

func TestTheImageWorkflowWarmsItsBasesBeforeBuilding(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatalf("read build.yml: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "warm-bases.sh") {
		t.Fatal("build.yml no longer warms its base images. A base-image 403 then fails the build " +
			"step directly, where it CANNOT be retried without also retrying compile failures — " +
			"which is the one thing #320's push retry was careful never to do (#320)")
	}

	// ORDER IS THE WHOLE POINT. Warming after the build would pull the bases the
	// build has already failed on.
	warm := strings.Index(src, "warm-bases.sh")
	build := stepIndex(src, "docker/build-push-action")
	if warm < 0 || build < 0 || warm > build {
		t.Fatalf("the warm step runs AFTER the build (warm@%d, build@%d) — it must precede it, or "+
			"it warms a cache nothing will read", warm, build)
	}
	// And after the login: these bases are private, so an unauthenticated warm
	// would fail every run rather than occasionally.
	login := stepIndex(src, "docker/login-action")
	if login < 0 || login > warm {
		t.Fatalf("the warm step runs BEFORE the registry login (login@%d, warm@%d) — every base here "+
			"is private, so that fails permanently rather than flakily", login, warm)
	}
}

// stepIndex locates a workflow STEP by its `uses:` line, never by the bare
// action name.
//
// THE BARE NAME MATCHES THE PROSE. build.yml explains its own ordering at
// length, so "docker/setup-buildx-action" and "docker/build-push-action" both
// appear in comments HUNDREDS of lines above the steps they describe. An
// ordering check anchored on the bare name compares a comment's position against
// a step's — which fired as a false failure the moment a fourth ordering guard
// was added, and would just as happily have passed while the real steps were in
// the wrong order.
func stepIndex(src, action string) int {
	return strings.Index(src, "uses: "+action+"@")
}

// THE THIRD REGISTRY PULL, AND THE ONE BOTH RETRIES SAID THEY COULD NOT COVER.
//
// build.yml pins the buildx BUILDER at ghcr.io/eighred/base/buildkit, which is
// private like every other mirrored base, and setup-buildx-action pulls it while
// creating the builder. That pull had no retry, and warm-bases.sh named the gap
// in its own header while declining to close it: "Retrying that means replacing
// the action with a scripted create+bootstrap … Left alone deliberately."
//
// It bit on 2026-08-18, PR #547, image (venue-okx):
//
//	#1 [internal] booting buildkit
//	#1 pulling image ghcr.io/eighred/base/buildkit:buildx-stable-1
//	#1 ERROR: Error response from daemon: manifest unknown
//
// One job of thirty; the other twenty-nine pulled the same tag in the same run,
// so the tag was there and the registry was not. It fails at buildx step #1 —
// before any step this repo wrote — so neither existing retry could see it, and
// the diagnosis costs a full reading of a red that is not a break.
//
// WHAT CLOSED IT IS SMALLER THAN WHAT WAS DECLINED. The action is untouched and
// the ordering it depends on is untouched: the image is pulled BEFORE the action
// runs, so the create+bootstrap finds it already in the daemon and pulls nothing.
// It is safe to retry for the same structural reason the other two are — the step
// pulls one image and builds nothing, so no compile failure can reach it.
func TestTheImageWorkflowWarmsItsBuilderBeforeCreatingIt(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatalf("read build.yml: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "warm-builder.sh") {
		t.Fatal("build.yml no longer warms the buildx builder image. setup-buildx-action then " +
			"pulls it unretried, and a registry hiccup reddens the job at step #1 — upstream of " +
			"every step this repo wrote, so neither the push retry nor warm-bases.sh can see it")
	}

	// ORDER IS THE WHOLE POINT, TWICE OVER.
	warm := strings.Index(src, "warm-builder.sh")
	buildx := stepIndex(src, "docker/setup-buildx-action")
	if warm < 0 || buildx < 0 || warm > buildx {
		t.Fatalf("the builder warm runs AFTER setup-buildx-action (warm@%d, buildx@%d) — the action "+
			"has already done the pull being protected, so this warms nothing", warm, buildx)
	}
	// And after the login, for the reason build.yml's own comment gives about the
	// buildx step: the builder image is PRIVATE, so an unauthenticated pull fails
	// every run rather than occasionally.
	login := stepIndex(src, "docker/login-action")
	if login < 0 || login > warm {
		t.Fatalf("the builder warm runs BEFORE the registry login (login@%d, warm@%d) — the builder "+
			"image is private, so that is a permanent failure wearing a flake's clothes", login, warm)
	}

	// THE WARMED IMAGE MUST BE THE ONE THE BUILDER ACTUALLY USES. Two literals
	// that must agree is exactly how this silently stops working: driver-opts
	// moves to a new tag, the warm keeps pulling the old one, and the retry
	// protects an image nothing boots.
	const builderImage = "ghcr.io/eighred/base/buildkit:buildx-stable-1"
	if !strings.Contains(src, "image="+builderImage) {
		t.Fatalf("build.yml's driver-opts no longer pins %s — this guard and the warm step are "+
			"pinned to it, so they are now protecting an image the builder does not use", builderImage)
	}
	if !strings.Contains(src, "warm-builder.sh "+builderImage) {
		t.Fatalf("the warm step does not pull %s — driver-opts and the warm have drifted apart, "+
			"and the pull being retried is not the pull that boots the builder", builderImage)
	}
}

func TestWarmBuilderScriptBehaviour(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH (%v): the retry script's behaviour cannot be exercised here", err)
	}
	repoRoot := filepath.Dir(moduleRoot(t))
	script := filepath.Join(repoRoot, ".github", "scripts", "warm-builder.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("warm-builder.sh is missing: %v", err)
	}

	for _, tc := range []struct {
		name        string
		failures    int
		wantErr     bool
		wantRetries int
	}{
		{name: "first attempt succeeds", failures: 0, wantErr: false, wantRetries: 0},
		{name: "one refusal then success", failures: 1, wantErr: false, wantRetries: 1},
		// THE PROPERTY THAT KEEPS IT HONEST, and the mirror of the other two: a
		// pull refused on EVERY attempt is a real authorization or visibility
		// failure, not a flake, and must still fail the job. Giving up quietly
		// would boot a builder nobody can pull and call it green.
		{name: "always refused still fails", failures: -1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := writeDockerStub(t, dir, tc.failures)
			outFile := filepath.Join(dir, "gh_output")

			cmd := exec.Command(bash, script, "ghcr.io/eighred/base/buildkit:buildx-stable-1")
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(),
				"DOCKER="+stub,
				"MAX_ATTEMPTS=4",
				"RETRY_BASE_DELAY=0",
				"GITHUB_OUTPUT="+outFile,
			)
			out, err := cmd.CombinedOutput()
			if tc.wantErr && err == nil {
				t.Fatalf("a pull refused on every attempt reported SUCCESS — a revoked scope would "+
					"boot no builder and the job would go green anyway.\n%s", out)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("warm-builder.sh failed after %d refusal(s): %v\n%s", tc.failures, err, out)
			}
			if tc.wantErr {
				return
			}
			got, err := os.ReadFile(outFile)
			if err != nil {
				t.Fatalf("read GITHUB_OUTPUT: %v", err)
			}
			want := fmt.Sprintf("retries=%d", tc.wantRetries)
			if !strings.Contains(string(got), want) {
				t.Errorf("GITHUB_OUTPUT = %q, want it to contain %q — #320 turns on a RATE, and a "+
					"silent retry hides the rate it exists to measure", got, want)
			}
		})
	}
}

func TestWarmBasesScriptBehaviour(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH (%v): the retry script's behaviour cannot be exercised here", err)
	}
	repoRoot := filepath.Dir(moduleRoot(t))
	script := filepath.Join(repoRoot, ".github", "scripts", "warm-bases.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("warm-bases.sh is missing: %v", err)
	}
	dockerfile := filepath.Join(repoRoot, "kanz", "services", "wealth", "Dockerfile")

	for _, tc := range []struct {
		name        string
		failures    int
		wantErr     bool
		wantRetries int
	}{
		{name: "first attempt succeeds", failures: 0, wantErr: false, wantRetries: 0},
		{name: "one refusal then success", failures: 1, wantErr: false, wantRetries: 1},
		// THE PROPERTY THAT KEEPS THIS HONEST, and the mirror of the push side: a
		// pull refused on EVERY attempt is a real visibility or authorization
		// failure — the packages are private — and must still fail the job. A
		// retry that gives up quietly would turn a revoked scope into a green
		// build of an image nobody could pull.
		{name: "always refused still fails", failures: -1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := writeDockerStub(t, dir, tc.failures)
			outFile := filepath.Join(dir, "gh_output")

			cmd := exec.Command(bash, script, dockerfile)
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(),
				"DOCKER="+stub,
				"MAX_ATTEMPTS=4",
				"RETRY_BASE_DELAY=0",
				"GITHUB_OUTPUT="+outFile,
				"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"),
			)
			out, err := cmd.CombinedOutput()

			if tc.wantErr {
				if err == nil {
					t.Fatalf("script SUCCEEDED though every pull was refused.\n\n"+
						"A base image that cannot be pulled at all is not the #320 flake — it is a "+
						"private package the job has lost access to, and reporting success would "+
						"build nothing while looking green.\n\n%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("script failed after %d refusal(s), which the retry should have absorbed: "+
					"%v\n\n%s", tc.failures, err, out)
			}
			if got := readRetries(t, outFile, false); got != tc.wantRetries {
				t.Errorf("reported retries = %d, want %d.\n\n"+
					"The COUNT is what keeps the flake visible once it stops being fatal — a silent "+
					"retry removes the evidence that the registry is degrading (#320).",
					got, tc.wantRetries)
			}
		})
	}
}

// A STAGE NAME IS NOT AN IMAGE. A multi-stage Dockerfile's `FROM build AS test`
// names an earlier stage; asking a registry for it fails for a reason that is not
// a flake, and the retry would then spend four attempts on it before failing the
// job. Pinned because the exclusion is a parsing rule, and parsing rules rot.
func TestWarmBasesSkipsStageReferences(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH (%v)", err)
	}
	repoRoot := filepath.Dir(moduleRoot(t))
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte(
		"FROM ghcr.io/eighred/base/golang:1.26.5 AS build\nRUN true\n"+
			"FROM build AS test\n"+
			"FROM ghcr.io/eighred/base/distroless-static:nonroot\nCOPY --from=build /app /app\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stub := writeDockerStub(t, dir, 0)
	cmd := exec.Command(bash, filepath.Join(repoRoot, ".github", "scripts", "warm-bases.sh"), dockerfile)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "DOCKER="+stub, "RETRY_BASE_DELAY=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("warm-bases failed: %v\n\n%s", err, out)
	}
	if strings.Contains(string(out), "warming build ") {
		t.Errorf("warmed the stage alias 'build' as if it were an image:\n\n%s", out)
	}
	for _, want := range []string{"golang:1.26.5", "distroless-static:nonroot"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("did not warm %s — every external base must be pulled here, or the one that is "+
				"missed is the one that 403s in the build:\n\n%s", want, out)
		}
	}
}
