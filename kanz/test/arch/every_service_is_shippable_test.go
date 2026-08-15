package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A SERVICE THAT CANNOT BE BUILT INTO AN IMAGE CANNOT BE DEPLOYED, AND NOTHING
// SAID SO.
//
// # How this was found
//
// Five services were written, tested and merged with no Dockerfile and no entry
// in the build matrix: autopilot, identity, lineage, performance and web-bff. Two
// of them are the backends of finished, closed work —
//
//	identity  the platform's own credential authority (#364). Slices 1-4 merged;
//	          it mints the ES256 tokens the gateway trusts.
//	web-bff   the session-holding backend the web application talks to (#371),
//	          whose five slices are complete with 110 passing frontend tests.
//
// — so the entire client stack was finished, green, and unshippable at the same
// time. Nobody was wrong at any step. Every test passed, every guard passed, and
// no signal anywhere distinguishes "this service ships" from "this service is
// source code somebody can run on a laptop".
//
// identity and web-bff are fixed here because finished work depends on them. The
// other three are DECLARED below rather than built: an image nothing pulls is a
// signed artifact with no consumer, and the point of this guard is to make the
// gap visible, not to close it by publishing things nobody asked for.
//
// # Why this is the shape of failure this repository keeps paying for
//
// It is the same shape as #283 (a registered metric with no writer), as the
// dark-capability audit (a package nothing imports), and as the DR classification
// (a store with no backup and no exclusion): a thing that LOOKS complete because
// completeness was never something anything checked. CLAUDE.md's answer is the
// standing one — an invariant worth keeping is a guard, not a paragraph.
//
// # What this checks
//
// Every services/<name>/cmd/<name> holding a main package must have a Dockerfile
// AND an entry in the build matrix, or an argued exemption naming the issue that
// retires it. Deny-by-default, with a dead-entry arm so an exemption cannot
// outlive its repair.
//
// It deliberately does NOT require a deployment manifest. An image is the
// irreducible floor — without one there is nothing to deploy under any
// configuration — while WHERE a service runs is a placement decision with real
// prerequisites (identity's own CNPG cluster, for one) and forcing a manifest
// here would be answered by writing a plausible one, which is the invented
// exclusion this estate keeps refusing.

// unshippableByDesign exempts a service from needing an image.
//
// An entry is a DECLARATION that the service is not meant to ship yet, not a
// place to park work. Each names the issue that removes it again, and the
// dead-entry arm below deletes the entry for you when the Dockerfile appears.
var unshippableByDesign = map[string]string{
	"autopilot": "#416 — the in-house alpha decision layer. It exists as a decision core with no " +
		"data foundation under it and nothing that would run it in an estate; #416's own sequencing " +
		"puts the foundation before the loop. Shipping an image now would put a scheduler in a " +
		"cluster with nothing to decide from.",
	"performance": "#416 — performance attribution feeds the alpha work's evaluation loop and has " +
		"no consumer until it exists. Same reasoning as autopilot: an image with nothing to serve.",
	"lineage": "#107 — SOV-03 sovereign telemetry (cross-tenant NAV, growth, fee and slippage). It " +
		"has a real main and four wiring tests, and it is still the unfinished half of an open " +
		"issue rather than a service waiting on an image. Unlike identity and web-bff, no shipped " +
		"work depends on it, so building it now would sign and publish an artifact nothing pulls.",
}

func TestEveryServiceCanBeBuiltIntoAnImage(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	services := shippableServiceNames(t, root)
	// NON-VACUITY, half one: a scan that finds no services passes on an estate
	// where nothing ships at all.
	if len(services) < 20 {
		t.Fatalf("found only %d service main packages under services/ — the scanner is broken, not "+
			"the estate (there were 27 when this guard was written)", len(services))
	}

	built := matrixServices(t, repoRoot)
	// NON-VACUITY, half two: if the workflow parse silently returned nothing,
	// every service would be reported and somebody would exempt the lot.
	if len(built) < 20 {
		t.Fatalf("parsed only %d services out of the build matrix — the workflow scan is broken", len(built))
	}

	var problems []string
	seenExempt := map[string]bool{}
	for _, svc := range services {
		dockerfile := filepath.Join(root, "services", svc, "Dockerfile")
		_, err := os.Stat(dockerfile)
		hasDockerfile := err == nil
		inMatrix := built[svc]

		if hasDockerfile && inMatrix {
			continue
		}
		if _, exempt := unshippableByDesign[svc]; exempt {
			seenExempt[svc] = true
			continue
		}
		var missing []string
		if !hasDockerfile {
			missing = append(missing, "no kanz/services/"+svc+"/Dockerfile")
		}
		if !inMatrix {
			missing = append(missing, "no entry in .github/workflows/build.yml's matrix")
		}
		problems = append(problems, fmt.Sprintf("%s: %s", svc, strings.Join(missing, "; ")))
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d service(s) exist in code but cannot be built into an image:\n  %s\n\n"+
			"A service with no image cannot be deployed under ANY configuration, so its tests "+
			"passing says nothing about whether it can run. This is how a finished feature stays "+
			"invisible: identity (#364) and web-bff (#371) were both complete and green while the "+
			"client stack they make up could not be shipped at all. Add the Dockerfile and the "+
			"matrix entry, or add an argued entry to unshippableByDesign naming the issue that "+
			"retires it.",
			len(problems), strings.Join(problems, "\n  "))
	}

	// THE DEAD-ENTRY ARM. An exemption that outlives its repair is worse than no
	// exemption: it is a standing claim that a service is not meant to ship,
	// asserted about one that now does.
	scanned := map[string]bool{}
	for _, svc := range services {
		scanned[svc] = true
	}
	for svc, why := range unshippableByDesign {
		if seenExempt[svc] {
			continue
		}
		_, err := os.Stat(filepath.Join(root, "services", svc, "Dockerfile"))
		switch {
		case !scanned[svc] && serviceExists(root, svc):
			// It exists but builds no binary, so it was never a candidate and the
			// exemption excuses nothing. A silent unused entry is how a map like
			// this fills with claims nobody can evaluate — and it is the exact
			// mistake made while writing this guard: lineage was exempted for
			// "having no main", which was wrong, because it has one.
			t.Errorf("unshippableByDesign[%q] exempts a service with no buildable main package, so "+
				"it excuses nothing. Delete the entry.\n  (%s)", svc, why)
		case err == nil && built[svc]:
			t.Errorf("unshippableByDesign[%q] is stale — the service now has a Dockerfile AND a "+
				"matrix entry. Delete the entry.\n  (%s)", svc, why)
		case !serviceExists(root, svc):
			t.Errorf("unshippableByDesign[%q] names a service that no longer exists under "+
				"services/. Delete the entry.\n  (%s)", svc, why)
		}
	}
}

// shippableServiceNames returns every services/<name> whose cmd/<name> holds a
// buildable main package.
//
// A BUILDABLE MAIN, NOT A DIRECTORY, AND NOT A TEST FILE. Two distinctions, each
// of which produced a wrong answer while this was being written:
//
//   - A services/<name>/cmd/<name> that exists but holds no main builds no
//     binary. Reporting it would send someone to write a Dockerfile for a
//     program that does not exist.
//   - _test.go is excluded. services/lineage/cmd/lineage contains exactly one
//     file, bus_metrics_scope_test.go, and it declares `package main` — because
//     that is what an external test of a main package must declare. Counting it
//     would have reported lineage as an unshippable SERVICE when what it
//     actually has is a test and no program.
//
// Distinct from serviceMains in pod_disruption_and_drain_test.go, which globs
// main.go PATHS for the drain-budget scan and would therefore miss a main
// declared in a differently named file.
func shippableServiceNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "services"))
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()
		cmdDir := filepath.Join(root, "services", svc, "cmd", svc)
		files, err := os.ReadDir(cmdDir)
		if err != nil {
			continue // no cmd/<name> at all: a library-shaped directory
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(cmdDir, f.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", f.Name(), err)
			}
			if strings.Contains(string(b), "\npackage main\n") || strings.HasPrefix(string(b), "package main\n") {
				out = append(out, svc)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func serviceExists(root, svc string) bool {
	info, err := os.Stat(filepath.Join(root, "services", svc))
	return err == nil && info.IsDir()
}

// matrixServices reads the build workflow's image matrix.
//
// build.yml is the source of truth; release.yml is held identical to it by
// TestReleaseMatrixCoversEveryBuiltService, so checking one checks both.
func matrixServices(t *testing.T, repoRoot string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatalf("read build.yml: %v", err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		// Matrix entries are "- service: <name>". Matched on the list marker so a
		// prose mention of "service: x" in a comment cannot register one.
		const marker = "- service:"
		if !strings.HasPrefix(line, marker) {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, marker))
		if name != "" {
			out[name] = true
		}
	}
	return out
}
