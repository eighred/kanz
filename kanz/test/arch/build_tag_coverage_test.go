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
func TestEveryShippedBuildTagIsCompiledByCI(t *testing.T) {
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

	var problems []string
	for _, s := range shipped {
		for _, tag := range s.tags {
			if !compiled[tag] {
				problems = append(problems, fmt.Sprintf(
					"%s builds with -tags %q, which no workflow compiles\n\n"+
						"      A tag that ships without being compiled by any check on this repository is\n"+
						"      dead code that deploys — it has rotted that way twice already (the\n"+
						"      anthropic adapter, and -tags redis before EXEC-M22). Add %q to the tagged\n"+
						"      step in kanz-ci.yml, or stop shipping it.",
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
	t.Logf("%d Dockerfile(s) with literal build tags, all compiled by CI: %s",
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
