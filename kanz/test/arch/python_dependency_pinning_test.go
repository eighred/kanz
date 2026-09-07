package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A SIGNATURE OVER A BUILD NOBODY CAN REPRODUCE IS SIGNED EVIDENCE OF NOTHING.
//
// release.yml's trivy/cosign/SBOM chain is proven — it has succeeded on tags
// v0.1.0 and v0.2.0, and the images are in Rekor. That makes this gap worse, not
// better: the pipeline was attesting an artifact whose contents could not be
// derived from the commit it claimed to be built from. kanz-py/Dockerfile:39-40
// installed the inference service's runtime dependencies as floating ranges —
//
//	pip install --no-cache-dir "nats-py>=2.7.0" "aiokafka>=0.10.0" \
//	  "protobuf>=5.29" "grpcio>=1.62"
//
// — with no lockfile and no hashes. Two consecutive builds of the same commit
// install different code, and both get a valid signature. An aiokafka with a
// breaking API arrives with no PR, no diff, and a green release. Every Go image
// in this estate builds against a checksummed go.sum; this one had nothing.
//
// THE SECOND HALF WAS THE DIVERGENCE. Line 38 copied kanz-py/pyproject.toml into
// the image and nothing ever installed from it, while lines 39-40 restated the
// same four dependencies by hand — the `secret()` shape AGENTS.md warns about
// ("17 services each had their own, and 15 were wrong"), applied to the
// dependency set of the service that scores risk models. Two lists of the same
// thing cannot be kept in step by care.
//
// WHY --require-hashes AND NOT MERELY `==`. An `==` pin names a version; a hash
// names an artifact. PyPI versions are not immutable in the way a git SHA is —
// a yank plus re-upload, or a compromised maintainer account, changes what
// `==1.2.3` resolves to while every pin in the tree still reads correct. It also
// forces the TRANSITIVE closure to be written down: `--require-hashes` refuses
// to install anything not listed, so a new indirect dependency becomes a
// reviewable diff instead of arriving silently. That is the same argument
// TestWorkflowActionsArePinnedToSHA makes for actions and
// TestCIToolInstallsArePinned makes for `go install`, in the one ecosystem
// neither of them can see.
//
// SCOPE IS THE WHOLE REPOSITORY, not the Go module — the same reason
// TestEveryDockerfileBaseImageComesFromTheMirror gives at
// baseimage_mirror_test.go's SCOPE note. The only Python image in the estate
// lives outside kanz/, so a guard scoped to the Go tree would assert less than
// its name claims and would be blind to the twenty-seventh Dockerfile that
// reaches for pip.
//
// NOT CI's OWN pip installs, and the gap is recorded rather than left implied.
// .github/workflows/kanz-ci.yml:514 installs the Python test dependencies
// unpinned (`pip install grpcio-tools pytest pytest-asyncio nats-py aiokafka`).
// That is the same class of defect one layer over: it decides what the TEST
// result means, not what the image contains, so it belongs with
// TestCIToolInstallsArePinned's population and not this one. This guard reads
// Dockerfiles — what gets SHIPPED and SIGNED — and says so in its name.
//
// NOT THE BASE IMAGE. #241 pairs this with "a mutable base tag", and that half
// is deliberately NOT enforced here: `grep -rn "@sha256" --include=Dockerfile*`
// returns zero across this repository, so no Dockerfile pins its base by digest.
// That is estate-wide work on 26 images, not a Python question, and folding it
// into this guard would mean landing this one red or exempting all 26 — either
// of which makes both invariants weaker to express.
func TestNoDockerfileInstallsPythonPackagesWithoutHashes(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))

	dockerfiles := dockerfilePaths(t, repoRoot)
	// NON-VACUITY, FLOOR ONE. A walk that finds no Dockerfiles passes every
	// assertion below no matter how many unpinned installs exist.
	if len(dockerfiles) == 0 {
		t.Fatalf("found zero Dockerfiles under %s — the scanner is broken, not the estate", repoRoot)
	}

	var problems []string
	seenExempt := map[string]bool{}
	installs := 0

	for _, df := range dockerfiles {
		raw, err := os.ReadFile(df.abs)
		if err != nil {
			t.Fatalf("read %s: %v", df.abs, err)
		}
		// Normalize CRLF for the same reason TestWorkflowActionsArePinnedToSHA
		// does: a Windows checkout (core.autocrlf=true) hands these over with
		// CRLF, and a scan that behaves differently here than on CI is a guard
		// people learn to ignore.
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")

		for _, cmd := range dockerfileCommands(body) {
			if !pipInstallRe.MatchString(cmd.text) {
				continue
			}
			installs++

			bare, hashed := classifyPipInstall(cmd.text)
			if hashed && len(bare) == 0 {
				continue
			}
			if _, ok := pipInstallsPendingHashPinning[df.rel]; ok {
				seenExempt[df.rel] = true
				continue
			}

			var why string
			switch {
			case len(bare) > 0 && !hashed:
				why = fmt.Sprintf("names package(s) on the command line (%s) and passes no --require-hashes",
					strings.Join(bare, ", "))
			case len(bare) > 0:
				why = fmt.Sprintf("passes --require-hashes but still names package(s) on the command line (%s), "+
					"which are installed unhashed", strings.Join(bare, ", "))
			default:
				why = "installs from a requirements file without --require-hashes, so pip verifies nothing"
			}
			problems = append(problems, fmt.Sprintf(
				"%s:%d %s\n      %s", df.rel, cmd.line, why, collapse(cmd.text)))
		}
	}

	// NON-VACUITY, FLOOR TWO, and it is the one that matters. Floor one only
	// trips if the whole walk breaks; this trips if the COMMAND PARSER stops
	// matching — a reformat to `python -m pip install`, a heredoc, a move into a
	// shell script the Dockerfile invokes. Zero recognised installs would
	// otherwise be reported as a clean estate, which is the exact failure this
	// guard exists to prevent, one level up.
	//
	// If the Python image genuinely leaves this estate, this fails LOUDLY and the
	// correct response is to delete the guard — not to soften the floor. "Nothing
	// configured" and "checked, and fine" must never look the same.
	if installs == 0 {
		t.Fatal("matched no `pip install` command in any Dockerfile — the parser is broken, not " +
			"the estate (this guard would otherwise pass vacuously). If the Python image was " +
			"removed from the repository, delete this test.")
	}

	// DEAD-ENTRY CHECK. An exemption naming a Dockerfile that no longer performs
	// an unpinned install is stale — the pinning landed, or the file went away.
	// Left in place it protects nothing, reads as load-bearing, and silently
	// re-opens the hole for the next unpinned install added to that same file.
	// Same shape as baseImagesPendingMirror and mutableTagExempt.
	var dead []string
	for file := range pipInstallsPendingHashPinning {
		if !seenExempt[file] {
			dead = append(dead, file)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		problems = append(problems, fmt.Sprintf(
			"pipInstallsPendingHashPinning exempts %d Dockerfile(s) with no unpinned pip install left: %s "+
				"— delete the entry; a stale exemption re-opens the hole it was documenting",
			len(dead), strings.Join(dead, ", ")))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("unpinned Python dependency installs (%d):\n\n  %s\n\n"+
			"Every `pip install` in a Dockerfile must read its packages from a hash-pinned "+
			"requirements file and pass --require-hashes, so the image built from a commit is "+
			"determined BY that commit. Without it the release pipeline signs and attests an "+
			"artifact whose contents cannot be reproduced — a valid signature over an unknown "+
			"build. Generate the lock (see kanz-py/requirements.txt's header for the command) "+
			"or add the file to pipInstallsPendingHashPinning with the OPEN issue that removes it.",
			len(problems), strings.Join(problems, "\n  "))
	}
	t.Logf("%d pip install command(s) across %d Dockerfile(s), all hash-pinned", installs, len(dockerfiles))
}

// pipInstallsPendingHashPinning is a DEFAULT-DENY allow-list: a Dockerfile whose
// `pip install` is not hash-pinned fails unless it is named here with a reason
// and the OPEN issue that removes it.
//
// IT IS EMPTY, AND THAT IS THE POINT. Both installs in kanz-py/Dockerfile — the
// grpcio-tools builder stage and the runtime stage — read from a hash-pinned
// requirements file. An entry here is a regression, not a state to settle into.
//
// KEYED ON THE DOCKERFILE, NOT ON file:line, and deliberately coarse: a
// line-keyed exemption would silently move to a different install when the file
// is edited, and a package-keyed one would invite splitting one unpinned install
// into two to keep each below notice. Exempting a whole file is a bigger,
// louder thing to write down, which is the correct pressure.
//
// If an entry is ever added, name an OPEN issue and check it is still open. The
// dead-entry check above fires when an exempt file stops having an unpinned
// install — never when the issue meant to retire it is closed with the work
// unshipped. baseImagesPendingMirror learned that one the hard way: its entries
// named #55, #55 was closed with the rewrite undone, and the exposure went on
// being "recorded" indefinitely, which reads at a glance exactly like being
// managed.
var pipInstallsPendingHashPinning = map[string]string{}

// pipInstallRe matches a pip install invocation in any of its spellings:
// `pip install`, `pip3 install`, `python -m pip install`, `uv pip install`. It
// deliberately does NOT match `pip download` or `pip wheel` — those fetch, they
// do not put code into the image.
var pipInstallRe = regexp.MustCompile(`\bpip[0-9.]*\s+install\b`)

// pipValueFlags are pip options that consume the NEXT argument. Without this
// list their values (a URL, a directory, a platform tag) would be read as bare
// package names and reported as unpinned installs — a false positive is how a
// guard gets weakened instead of the Dockerfile getting fixed.
var pipValueFlags = map[string]bool{
	"-r": true, "--requirement": true,
	"-c": true, "--constraint": true,
	"-i": true, "--index-url": true,
	"-e": true, "--editable": true,
	"-t": true, "--target": true,
	"-f": true, "--find-links": true,
	"--extra-index-url": true, "--no-binary": true, "--only-binary": true,
	"--platform": true, "--python-version": true, "--implementation": true,
	"--abi": true, "--prefix": true, "--root": true, "--src": true,
	"--upgrade-strategy": true, "--progress-bar": true, "--cache-dir": true,
	"--log": true, "--timeout": true, "--retries": true, "--proxy": true,
	"--report": true, "--config-settings": true, "--install-option": true,
	"--global-option": true, "--trusted-host": true, "--client-cert": true,
	"--cert": true, "--exists-action": true, "--no-build-isolation-package": true,
}

// classifyPipInstall reads one shell command containing a pip install and
// returns the package specs named directly on the command line, and whether
// --require-hashes was passed.
//
// A bare spec is the finding: anything pip installs that did not come through
// `-r <file>` is, by construction, not hash-checked — `--require-hashes` refuses
// to run at all in that combination, so an install naming packages directly can
// never be reproducible.
//
// Scanning stops at a shell separator so a `pip install ... && python -m
// something` does not report `python` as a package. `-e .` is treated as a
// value flag rather than a bare spec on purpose: an editable install of the
// local tree is reported through the --require-hashes arm (pip rejects the
// combination), where the message is about hashes rather than about a stray
// package name.
func classifyPipInstall(cmd string) (bare []string, requireHashes bool) {
	fields := strings.Fields(cmd)

	// Find the `install` that follows a pip invocation, and read only the
	// arguments belonging to THAT command.
	start := -1
	for i := 1; i < len(fields); i++ {
		if fields[i] != "install" {
			continue
		}
		prev := strings.Trim(fields[i-1], `"'`)
		if base := prev[strings.LastIndexAny(prev, "/\\")+1:]; strings.HasPrefix(base, "pip") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, false
	}

	for i := start; i < len(fields); i++ {
		tok := fields[i]
		switch tok {
		case "&&", "||", ";", "|", "\\":
			return bare, requireHashes
		}
		if strings.HasPrefix(tok, "-") {
			name := tok
			if eq := strings.Index(tok, "="); eq > 0 {
				name = tok[:eq]
			} else if pipValueFlags[tok] {
				i++ // its value belongs to the flag, not to the package list
			}
			if name == "--require-hashes" {
				requireHashes = true
			}
			continue
		}
		bare = append(bare, strings.Trim(tok, `"'`))
	}
	return bare, requireHashes
}

// dockerfileCommand is one LOGICAL Dockerfile instruction — continuation lines
// joined — together with the line the instruction starts on, which is what a
// failure message must name for the reader to find it.
type dockerfileCommand struct {
	line int
	text string
}

// dockerfileCommands joins backslash-continued lines into logical instructions
// and drops comments.
//
// Reading physical lines would miss every real install in this repository:
// kanz-py/Dockerfile's runtime install spans two lines, so a per-line scan sees
// `RUN pip install --no-cache-dir \` with no packages after it and concludes
// there is nothing to check. Comment lines are dropped — including comment lines
// INSIDE a continuation, which Docker permits — so that the prose above a RUN
// cannot satisfy or trip the guard. Matching the OPERATION and not a mention of
// it is the same rule TestCIContainerImagesAreNotLatest records; this file's own
// doc comment quotes an unpinned pip install, and would otherwise match itself
// were it ever scanned.
func dockerfileCommands(body string) []dockerfileCommand {
	var out []dockerfileCommand
	lines := strings.Split(body, "\n")

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		start := i
		var parts []string
		for {
			cur := strings.TrimSpace(lines[i])
			if strings.HasSuffix(cur, "\\") {
				parts = append(parts, strings.TrimSpace(strings.TrimSuffix(cur, "\\")))
				// Advance past any comment lines sitting inside the continuation.
				for i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "#") {
					i++
				}
				if i+1 >= len(lines) {
					break
				}
				i++
				continue
			}
			parts = append(parts, cur)
			break
		}
		out = append(out, dockerfileCommand{line: start + 1, text: strings.Join(parts, " ")})
	}
	return out
}

// collapse squeezes a joined instruction back to one readable line for a
// failure message. A guard nobody can act on from its output is a guard that
// gets skipped.
var whitespaceRun = regexp.MustCompile(`\s+`)

func collapse(s string) string {
	s = whitespaceRun.ReplaceAllString(s, " ")
	if len(s) > 160 {
		return s[:157] + "..."
	}
	return s
}

// ONE LIST, OR IT IS NOT PINNED — IT IS PINNED SOMEWHERE ELSE TOO.
//
// The hash-pinned lock closes the reproducibility hole, but on its own it
// re-creates the divergence it replaced: pyproject.toml declares what the
// distribution depends on, requirements.txt says what the image installs, and
// nothing makes the second follow the first. Adding a dependency to pyproject
// and forgetting the lock is silent — the image builds, reproducibly, without
// it, and the service fails at import time on a pod nobody is watching.
//
// So the lock is DERIVED, and this is what makes "derived" mean something:
// every runtime dependency declared in kanz-py/pyproject.toml must appear in
// kanz-py/requirements.txt as an `==` pin, or be named in
// dependenciesNotInstalledInTheImage with the reason it is not.
//
// It compares NAMES, not version ranges. Resolving a PEP 440 specifier in Go
// would mean a second, worse implementation of pip's resolver living in a test —
// and the failure this guard exists to catch is a dependency that is ABSENT from
// the lock, not one pinned outside its declared range. Regenerating the lock
// with the command in its header is what checks the ranges, using the resolver
// that will actually do the install.
func TestInferenceLockCoversEveryDeclaredDependency(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	pyDir := filepath.Join(repoRoot, "kanz-py")

	declared := declaredPythonDependencies(t, filepath.Join(pyDir, "pyproject.toml"))
	// NON-VACUITY. An empty parse — a reformatted pyproject, a renamed table —
	// would satisfy every assertion below while checking nothing.
	if len(declared) == 0 {
		t.Fatal("parsed zero dependencies from kanz-py/pyproject.toml — the parser is broken, " +
			"not the manifest (this guard would otherwise pass vacuously)")
	}

	locked := lockedPythonPackages(t, filepath.Join(pyDir, "requirements.txt"))
	if len(locked) == 0 {
		t.Fatal("parsed zero `name==version` pins from kanz-py/requirements.txt — the parser is " +
			"broken, or the lock is empty (this guard would otherwise pass vacuously)")
	}

	var problems []string
	seenExempt := map[string]bool{}
	for _, name := range declared {
		if locked[name] != "" {
			continue
		}
		if _, ok := dependenciesNotInstalledInTheImage[name]; ok {
			seenExempt[name] = true
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s is declared in kanz-py/pyproject.toml but has no `==` pin in kanz-py/requirements.txt "+
				"— the image would build reproducibly WITHOUT it, and the service would fail at import "+
				"on a pod nobody is watching. Regenerate the lock (see its header) or, if the image "+
				"genuinely must not install it, add it to dependenciesNotInstalledInTheImage with the reason.",
			name))
	}

	// DEAD-ENTRY CHECK, same shape as pipInstallsPendingHashPinning above: an
	// exclusion naming a package pyproject no longer declares is stale, and the
	// next reader would take it for a live decision.
	for name, reason := range dependenciesNotInstalledInTheImage {
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, name+": excluded in dependenciesNotInstalledInTheImage with no reason")
			continue
		}
		if !seenExempt[name] {
			problems = append(problems, name+
				": in dependenciesNotInstalledInTheImage but kanz-py/pyproject.toml no longer declares it "+
				"(or the lock now pins it) — dead entry, remove it")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("kanz-py/requirements.txt has drifted from kanz-py/pyproject.toml (%d):\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
	t.Logf("%d declared dependency/ies, %d locked package(s), no drift", len(declared), len(locked))
}

// dependenciesNotInstalledInTheImage names a runtime dependency declared in
// pyproject.toml that the image deliberately does NOT pip-install, with why.
//
// This is not a hole in the pinning — it is the one case where "installed from
// PyPI" is the wrong answer entirely, and saying so out loud is the point.
var dependenciesNotInstalledInTheImage = map[string]string{
	"kanz-schemas": "the Python schema SDK is GENERATED, NOT PUBLISHED (EVT-15a) — kanz-py/Dockerfile's " +
		"builder stage runs grpc_tools.protoc over kanz-schemas/proto/ and copies the result to " +
		"/app/gen, which PYTHONPATH picks up as a sibling namespace root. Installing a PyPI " +
		"distribution of this name would either 404 the build or, worse, resolve to somebody " +
		"else's package. There is nothing to pin because there is no artifact to fetch.",
}

// pep503Normalize is PyPI's own name-equality rule: compare `nats_py`, `nats-py`
// and `NATS.PY` as the same project. Without it this guard would report a
// dependency missing from a lock that pins it under the other spelling — a false
// failure whose obvious fix is to weaken the guard.
var pep503Separators = regexp.MustCompile(`[-_.]+`)

func pep503Normalize(name string) string {
	return pep503Separators.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
}

// declaredPythonDependencies reads the `dependencies` array from pyproject.toml's
// [project] table and returns the normalized project names.
//
// Hand-parsed rather than pulled through a TOML library: this test needs three
// lines of one array, and adding a dependency to the Go module to read it would
// be a worse trade than the twenty lines below. The parse is deliberately strict
// about where it starts and stops so it cannot silently read the
// [project.optional-dependencies] test extras instead and pass by checking the
// wrong list.
func declaredPythonDependencies(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	inProject, inDeps := false, false
	var names []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			// Any new table ends the array; [project.optional-dependencies] in
			// particular must not be read as if it were the runtime set.
			inProject = trimmed == "[project]"
			inDeps = false
			continue
		}
		if !inProject {
			continue
		}
		if !inDeps {
			if !strings.HasPrefix(trimmed, "dependencies") || !strings.Contains(trimmed, "=") {
				continue
			}
			inDeps = true
			trimmed = trimmed[strings.Index(trimmed, "=")+1:]
			trimmed = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trimmed), "["))
		}
		if strings.HasPrefix(trimmed, "]") {
			inDeps = false
			continue
		}
		// STRIP THE COMMENT BEFORE SPLITTING ON COMMAS, or a comma inside one
		// becomes a dependency. The per-piece `#` check below only catches a
		// comment that survives the split intact: adding
		//
		//   # Pure Python, no transitive runtime dependencies of its own.
		//
		// beside an entry made this guard report a missing pin for a package
		// named "no", and the obvious-looking fix is to delete the comment —
		// i.e. a correct manifest turning a guard red, which is how guards get
		// weakened instead of repaired. #241 hit it on the first edit.
		trimmed = stripTOMLComment(trimmed)
		for _, spec := range strings.Split(trimmed, ",") {
			spec = strings.TrimSpace(strings.Trim(strings.TrimSpace(spec), `"'`))
			if spec == "" || strings.HasPrefix(spec, "#") {
				continue
			}
			// Cut the version specifier / extras / environment marker off the name.
			if cut := strings.IndexAny(spec, "<>=!~;[ "); cut > 0 {
				spec = spec[:cut]
			}
			if spec != "" {
				names = append(names, pep503Normalize(spec))
			}
		}
	}
	sort.Strings(names)
	return names
}

// THE PARSER MUST BE PROVEN ON A SHAPE, BECAUSE A FALSE POSITIVE HERE READS AS A
// DRIFTED LOCK.
//
// TestInferenceLockCoversEveryDeclaredDependency reports a name that has no pin.
// If the parser invents a name, that report is indistinguishable from a real
// missing pin — and the fastest way to make it green is to delete the comment
// that produced it, which teaches the next contributor that annotating a
// dependency breaks the build.
func TestPyprojectDependencyParserReadsCommentedEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pyproject.toml")
	body := `[build-system]
requires = ["setuptools>=68"]

[project]
name = "kanz-bus"
dependencies = [
    "nats-py>=2.7.0",
    # The metrics surface. Pure Python, no transitive deps of its own.
    "prometheus-client>=0.20",
    "grpcio>=1.62",  # the interactive path
]

[project.optional-dependencies]
test = [
    "pytest>=8.0",
]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := declaredPythonDependencies(t, path)
	want := []string{"grpcio", "nats-py", "prometheus-client"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("declaredPythonDependencies = %v, want %v\n\n"+
			"A name that is not in the [project] dependencies array — 'no' from the comment's "+
			"comma, or 'pytest' from the optional-dependencies table — makes this guard demand a "+
			"pin for a package the image never installs.", got, want)
	}
}

// stripTOMLComment cuts a line at its first UNQUOTED '#'.
//
// Quote-aware rather than a plain Cut: PEP 508 lets a requirement carry a URL
// with a fragment ("pkg @ https://host/w.whl#sha256=..."), and those live inside
// the quoted spec. Nothing in kanz-py uses that form today, which is exactly why
// a naive cut would sit here working until the day one did.
func stripTOMLComment(line string) string {
	inQuote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == '#':
			return strings.TrimSpace(line[:i])
		}
	}
	return line
}

// lockedPythonPackages reads `name==version` pins from a pip requirements file,
// returning normalized name -> version. Hash lines (`--hash=sha256:...`) and
// continuations are skipped; only the requirement lines matter here.
func lockedPythonPackages(t *testing.T, path string) map[string]string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — the lock the Dockerfile installs from must exist", path, err)
	}

	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		eq := strings.Index(trimmed, "==")
		if eq <= 0 {
			continue
		}
		name := pep503Normalize(trimmed[:eq])
		version := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed[eq+2:]), "\\"))
		if name != "" && version != "" {
			out[name] = version
		}
	}
	return out
}
