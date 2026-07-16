package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A PIN NOTHING ENFORCES IS NOT A PIN.
//
// release.yml grants packages: write and id-token: write — a compromised action
// running there can push an image AND mint a genuine cosign signature that
// verifies. Every action in every workflow was moved from a floating tag to an
// immutable commit SHA (SUPPLY-M1 task 1) for exactly that reason: the value of
// pinning is that MOVING an action becomes a reviewable commit, and that only
// holds if a tag-pinned action fails the build (KANZ_BRAIN.md: "latest is not a
// version; it is a promise that the gate can change without a commit. Pin every
// tool that can fail a build, and make moving it a reviewable change.").
//
// This test is the enforcement: every `uses:` reference across every workflow
// file must resolve to a 40-character commit SHA, or be named below with a
// reason. Without it, the pinning from task 1 is a snapshot that erodes the
// next time someone edits a workflow and reaches for a tag out of habit.
func TestWorkflowActionsArePinnedToSHA(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	files := workflowFiles(t, repoRoot)
	if len(files) == 0 {
		t.Fatal("no workflow files found under .github/workflows — has the layout changed? " +
			"(this test would otherwise pass vacuously)")
	}

	usesRe := regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*(\S+)(.*)$`)
	shaRe := regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	trailerRe := regexp.MustCompile(`#\s*\S+`)

	var problems []string
	seen := map[string]bool{} // action name -> referenced anywhere (for dead-exemption check)
	total := 0

	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Normalize CRLF: git on Windows (core.autocrlf=true) hands these files
		// over with CRLF, and a naive line-anchored regex would otherwise behave
		// differently on a Windows checkout than on CI (see deployability_test.go).
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		for _, m := range usesRe.FindAllStringSubmatch(body, -1) {
			actionRef := m[1]
			trailer := m[2]
			if strings.HasPrefix(actionRef, "./") {
				// A local composite action is in-repo, not a supply-chain edge —
				// there is nothing external to pin.
				continue
			}
			at := strings.LastIndex(actionRef, "@")
			if at <= 0 || at == len(actionRef)-1 {
				problems = append(problems, rel+": malformed uses reference "+actionRef+" (expected owner/repo@ref)")
				continue
			}
			total++
			name, ref := actionRef[:at], actionRef[at+1:]
			seen[name] = true

			_, exempt := pinExempt[name]
			isSHA := shaRe.MatchString(ref)

			if exempt {
				if isSHA {
					problems = append(problems, name+" ("+rel+"): now SHA-pinned ("+ref+") — remove it from "+
						"pinExempt, the exemption is stale")
				}
				continue
			}
			if !isSHA {
				problems = append(problems, rel+": "+name+"@"+ref+" is pinned to a tag/branch, not a commit SHA — "+
					"pin it to the commit SHA "+ref+" currently resolves to, keeping \""+ref+"\" as a trailing comment "+
					"(e.g. uses: "+name+"@<40-char-sha> # "+ref+")")
			} else if !trailerRe.MatchString(trailer) {
				problems = append(problems, rel+": "+name+"@"+ref+" is SHA-pinned but has no trailing \"# <ref>\" "+
					"comment — Dependabot reads that comment to know what tag/version the SHA corresponds to; a bare "+
					"SHA with no comment is invisible to it and will never receive a security bump (add e.g. \"# v4\" "+
					"after the SHA)")
			}
		}
	}

	if total == 0 {
		t.Fatal("zero `uses:` lines found across all workflow files — the regex or the workflow layout is broken " +
			"(this test would otherwise pass vacuously)")
	}

	// Anti-rot, direction two: an exemption for an action no longer referenced by
	// any workflow is dead weight — the next reader would believe it load-bearing.
	for name, reason := range pinExempt {
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, name+": exempted in pinExempt with no reason")
			continue
		}
		if !seen[name] {
			problems = append(problems, name+": in pinExempt but not referenced by any workflow — dead exemption, remove it")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("unenforced supply-chain pins:\n\n  %s\n\n"+
			"Every `uses:` action across every workflow must resolve to a 40-character commit SHA, or be named in "+
			"pinExempt with a written reason. A tag-pinned action can be silently repointed by its maintainer — "+
			"moving a SHA-pinned action is a reviewable commit instead.", strings.Join(problems, "\n  "))
	}
}

// pinExempt lists actions deliberately held at a tag instead of a commit SHA,
// each with the reason. An action is here because somebody DECIDED it, not
// because somebody forgot to pin it.
var pinExempt = map[string]string{}

// workflowFiles walks the repo root for every *.yml / *.yaml under a
// .github/workflows directory — both the root .github/workflows and
// kanz-schemas/.github/workflows, and any future one, without hardcoding the
// path list. A ninth workflow file, or a workflows directory in a new
// sub-repo, is picked up automatically; only .git is skipped (irrelevant and
// large).
func workflowFiles(t *testing.T, repoRoot string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() || filepath.Base(filepath.Dir(path)) != "workflows" {
			return nil
		}
		if filepath.Base(filepath.Dir(filepath.Dir(path))) != ".github" {
			return nil
		}
		if ext := filepath.Ext(path); ext == ".yml" || ext == ".yaml" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for workflow files: %v", repoRoot, err)
	}
	sort.Strings(files)
	return files
}
