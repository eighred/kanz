package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A BUILD TAG THAT SHIPS BUT IS NEVER COMPILED HERE IS DEAD CODE THAT DEPLOYS.
//
// This has already happened twice in this repository, and both times the same
// way: an adapter behind a build tag went into the production image, no check on
// this repository ever compiled that tag, and the code rotted where nobody could
// see it. model_anthropic.go's own comment records one of them —
//
//	"This is what rotted: the adapter unmarshalled Input directly, which stopped
//	 compiling when the SDK widened the field, and nothing caught it because the
//	 `anthropic` build tag was never compiled by CI. The only copilot binary that
//	 built was the stub."
//
// — and kanz-ci.yml names `-tags redis` as the other.
//
// Both were fixed by adding a CI step, which fixes the instance and not the
// class: the next tag added to a Dockerfile is not covered by the step somebody
// wrote for the last one. This is the class. Every literal tag a Dockerfile
// builds with must also be compiled by kanz-ci, or the build fails HERE, in a
// check that runs on every PR, rather than months later in a binary nobody
// exercised.
//
// SCOPE IS LITERAL TAGS ONLY. Dockerfiles that take `-tags "${GOTAGS}"` (the
// venue images) parameterise the decision per build, so there is no fixed set to
// compare — their coverage is a different question, deliberately not answered
// here rather than answered badly.
//
// # COMPILED IS NOT LINTED, AND THIS GUARD USED TO CONFLATE THEM (#179)
//
// The first version checked `go build -tags` only, and passed while every file
// behind a tag escaped golangci-lint entirely: the action ran with no
// --build-tags, so it analysed the DEFAULT build. Both copilot adapters and the
// redis bus adapter were compiled, vetted and tested by CI and linted by
// nothing — and the fourth gate is not decorative, it is the one AGENTS.md
// records as rejecting code the other three accept. It was holding a live
// finding: model_anthropic.go discarded acc.Accumulate's error and served a
// partially assembled model message to a portfolio manager as analysis.
//
// So the rule is BOTH: a shipped tag must appear in a `go build -tags` step AND
// in the golangci-lint step's --build-tags. Adding a tag to the Dockerfile and
// to the tagged build step alone reopens the exact hole, and that half of it
// passes every other check on this repository.
func TestEveryShippedBuildTagIsCompiledAndLintedByCI(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	shipped := dockerfileBuildTags(t, root)
	// NON-VACUITY. If the scan finds no tagged Dockerfile, this guard passes no
	// matter what CI compiles — and the whole failure it exists for is a tag
	// nobody looked at. Two Dockerfiles carry literal tags today (copilot,
	// webhook-ingest); zero means the scanner broke, not that the estate is clean.
	if len(shipped) == 0 {
		t.Fatal("found no Dockerfile building with a literal -tags value — the scanner is broken, " +
			"not the estate (copilot and webhook-ingest both pin one)")
	}

	compiled := ciCompiledTags(t, repoRoot)
	if len(compiled) == 0 {
		t.Fatal("found no `go build -tags` in .github/workflows — either the tagged CI steps are " +
			"gone or the scanner is broken; both mean shipped tags are uncovered")
	}

	linted, lintSteps := ciLintedTags(t, repoRoot)
	// NON-VACUITY, THE SECOND SCAN — and the arm that matters most, because a
	// scanner finding nothing here would make this guard vouch for precisely the
	// state it exists to prevent: green while the tagged adapters go unlinted.
	// One golangci-lint step exists today (kanz-ci.yml); zero means it was
	// removed or renamed past this scan, not that the estate is clean.
	if lintSteps == 0 {
		t.Fatal("found no golangci-lint step in .github/workflows — either the fourth gate is gone " +
			"or this scanner no longer recognises it; both mean every shipped tag is unlinted")
	}
	if len(linted) == 0 {
		t.Fatalf("the %d golangci-lint step(s) in .github/workflows pass no --build-tags, so the "+
			"fourth gate sees only the default build — which is how the Anthropic adapter carried an "+
			"unchecked error return into a shipped image (#179)", lintSteps)
	}

	var problems []string
	for _, s := range shipped {
		for _, tag := range s.tags {
			if !compiled[tag] {
				problems = append(problems, fmt.Sprintf(
					"%s builds with -tags %q, which no workflow COMPILES\n\n"+
						"      A tag that ships without being compiled by any check on this repository is\n"+
						"      dead code that deploys — it has rotted that way twice already (the\n"+
						"      anthropic adapter, and -tags redis before EXEC-M22). Add %q to the tagged\n"+
						"      step in kanz-ci.yml, or stop shipping it.",
					s.file, tag, tag))
			}
			if !linted[tag] {
				problems = append(problems, fmt.Sprintf(
					"%s builds with -tags %q, which golangci-lint does not LINT\n\n"+
						"      Compiled is not linted. Every file behind this tag is invisible to the\n"+
						"      fourth gate — the one that rejects code build, vet and test all accept —\n"+
						"      and that gap shipped an unchecked error return in the Anthropic adapter\n"+
						"      (#179). Add %q to --build-tags on the golangci-lint step in kanz-ci.yml,\n"+
						"      or stop shipping it.",
					s.file, tag, tag))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("shipped build tags with no CI coverage:\n\n  %s", strings.Join(problems, "\n\n  "))
	}

	var names []string
	for _, s := range shipped {
		names = append(names, s.file+" ["+strings.Join(s.tags, ",")+"]")
	}
	sort.Strings(names)
	t.Logf("%d Dockerfile(s) with literal build tags, all compiled AND linted by CI: %s",
		len(shipped), strings.Join(names, "; "))
}

type shippedTags struct {
	file string
	tags []string
}

// literalTagsRe matches a `-tags` value that is a fixed list. The negative
// lookahead Go lacks is done by rejecting "$" after the fact — a ${GOTAGS}
// value is parameterised and out of scope (see the guard's doc).
var literalTagsRe = regexp.MustCompile(`-tags\s+"?([A-Za-z0-9_,$}{]+)"?`)

// dockerfileBuildTags returns every Dockerfile that builds with a LITERAL tag
// list, and the tags it names.
func dockerfileBuildTags(t *testing.T, root string) []shippedTags {
	t.Helper()
	var out []shippedTags

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "Dockerfile" {
			return nil
		}
		body, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		rel, _ := filepath.Rel(root, path)

		tagSet := map[string]bool{}
		for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			// Comments describe tags constantly — this file's own header does.
			// Matching prose would flag a Dockerfile for explaining itself.
			if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "go build") {
				continue
			}
			for _, m := range literalTagsRe.FindAllStringSubmatch(trimmed, -1) {
				if strings.ContainsAny(m[1], "${}") {
					continue // parameterised — out of scope
				}
				for _, tag := range strings.Split(m[1], ",") {
					if tag = strings.TrimSpace(tag); tag != "" {
						tagSet[tag] = true
					}
				}
			}
		}
		if len(tagSet) > 0 {
			tags := make([]string, 0, len(tagSet))
			for tag := range tagSet {
				tags = append(tags, tag)
			}
			sort.Strings(tags)
			out = append(out, shippedTags{file: filepath.ToSlash(rel), tags: tags})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for Dockerfiles: %v", root, err)
	}
	return out
}

// ciCompiledTags returns every tag some workflow passes to `go build -tags`.
//
// BUILD, NOT TEST OR VET. Compiling is the floor: it is what catches an adapter
// that stopped matching its SDK, which is the failure this guard exists for. A
// step that tests a tag necessarily builds it too, so the floor is the right
// thing to measure and the stricter question — is it tested? — is left to the
// judgement of whoever writes the step.
func ciCompiledTags(t *testing.T, repoRoot string) map[string]bool {
	t.Helper()
	out := map[string]bool{}

	for _, path := range workflowFiles(t, repoRoot) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "go build") {
				continue
			}
			for _, m := range literalTagsRe.FindAllStringSubmatch(trimmed, -1) {
				if strings.ContainsAny(m[1], "${}") {
					continue
				}
				for _, tag := range strings.Split(m[1], ",") {
					if tag = strings.TrimSpace(tag); tag != "" {
						out[tag] = true
					}
				}
			}
		}
	}
	return out
}

// lintTagsRe matches the tag list handed to golangci-lint. It cannot reuse
// literalTagsRe: the flag is spelled differently (two dashes, and "=" rather
// than a space), and matching the looser pattern would let a `go build -tags`
// line inside a lint step satisfy the lint half of the rule.
var lintTagsRe = regexp.MustCompile(`--build-tags[=\s]+"?([A-Za-z0-9_,$}{]+)"?`)

// stepStartRe marks the beginning of a workflow step.
var stepStartRe = regexp.MustCompile(`^\s*-\s+(name|uses|run|id):`)

// ciLintedTags returns every tag some workflow passes to golangci-lint via
// --build-tags, and how many golangci-lint steps were found at all.
//
// THE COUNT IS RETURNED SEPARATELY ON PURPOSE. "No lint step" and "a lint step
// with no tags" are different failures with different repairs — one means the
// fourth gate is gone or renamed past this scanner, the other means it is
// running against the default build only — and a single empty map would report
// them identically. Both are fatal; the caller says which.
//
// ATTRIBUTED PER STEP, NOT PER FILE. Scanning a whole workflow for the flag
// would let a --build-tags belonging to some other step satisfy this one, which
// is the same conflation the guard's header is about.
func ciLintedTags(t *testing.T, repoRoot string) (map[string]bool, int) {
	t.Helper()
	out := map[string]bool{}
	steps := 0

	for _, path := range workflowFiles(t, repoRoot) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, step := range workflowSteps(string(body)) {
			// COMMENTS ARE SKIPPED BEFORE ANYTHING IS DECIDED. The lint step's own
			// comment block names both "golangci-lint" and "--build-tags" — a
			// scanner that read prose would keep passing after the flag was
			// deleted, which is the failure mode a guard cannot afford and the one
			// the mutation for this arm checks.
			var code []string
			for _, line := range step {
				if trimmed := strings.TrimSpace(line); !strings.HasPrefix(trimmed, "#") {
					code = append(code, trimmed)
				}
			}
			isLint := false
			for _, line := range code {
				if strings.Contains(line, "golangci-lint") {
					isLint = true
					break
				}
			}
			if !isLint {
				continue
			}
			steps++
			for _, line := range code {
				for _, m := range lintTagsRe.FindAllStringSubmatch(line, -1) {
					if strings.ContainsAny(m[1], "${}") {
						continue // parameterised — out of scope, as above
					}
					for _, tag := range strings.Split(m[1], ",") {
						if tag = strings.TrimSpace(tag); tag != "" {
							out[tag] = true
						}
					}
				}
			}
		}
	}
	return out, steps
}

// workflowSteps splits a workflow into step blocks. Structural enough to
// attribute one argument to the step that carries it without pulling a YAML
// parser into test/arch for a single field; a step runs until the next list
// item that opens one.
func workflowSteps(body string) [][]string {
	var steps [][]string
	var cur []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if stepStartRe.MatchString(line) && len(cur) > 0 {
			steps = append(steps, cur)
			cur = nil
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		steps = append(steps, cur)
	}
	return steps
}
