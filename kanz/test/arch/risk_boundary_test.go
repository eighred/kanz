// Package arch holds build-time architecture tests. Each test runs
// `go list -json ./...` from the module root, walks every package's
// direct + test imports, and fails the build when a forbidden edge
// appears. The tests are deliberately simple and dependency-free:
// the goal is fast feedback in CI without pulling in an arch-lint DSL.
//
// RISK-02 is the first tenant — enforces the risk-engine module
// boundary the RISK-01 api/v1 interface declared. Future modules
// (PRED, DATA) add their own test files to this directory under the
// same pattern.
//
// # The risk-module boundary
//
// The Go language's `internal/` rule (kanz/internal/...) only
// restricts to "rooted at kanz/" — anything under the kanz module
// can already import kanz/internal/risk/anything. The architecture
// test adds the finer-grained rule the api/v1 contract depends on:
//
//   - Code OUTSIDE `kanz/internal/risk/` may import only
//     `kanz/internal/risk/api/v*`. The impl packages (compute,
//     state, ingest, scenario, publish, degraded, domain) are
//     private to the module.
//
//   - `kanz/internal/risk/api/v*` may NOT import any impl package.
//     Doing so would leak impl types into the public contract and
//     defeat the api-as-contract design.
//
// A regression — a caller reaching past api/ for "just one helper",
// or the api package picking up a domain type — fails this test
// immediately, before it can ossify into a wider dependency.
package arch

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	modulePath = "github.com/eighred/kanz"
	// riskRoot is the risk module's root package itself (package risk —
	// RISK-11 degraded.go, the cross-cutting home). It has no trailing
	// slash, so the riskModulePrefix check below misses it; it is
	// risk-internal and must be classified as such, not as an outsider.
	riskRoot         = modulePath + "/internal/risk"
	riskModulePrefix = riskRoot + "/"
	riskAPIPrefix    = riskRoot + "/api/"
	// riskComposerPrefix is the risk-engine SERVICE — the risk module's
	// designated composition root (ORCH-01/PERS-01). Wiring the concrete
	// EngineImpl, the ingest pipeline, durable state, and bootstrap replay
	// inherently requires the impl packages (engine/state/compute/publish/
	// ingest/persist) — the api/v* surface is interfaces only and cannot be
	// constructed from itself. The boundary protects every OTHER consumer;
	// the one service that owns the module is exempt, the standard
	// "main/wiring layer may import internals" carve-out.
	riskComposerPrefix = modulePath + "/services/risk-engine/"
)

// isRiskInternal reports whether an import path is the risk module's root
// package or any package beneath it.
func isRiskInternal(importPath string) bool {
	return importPath == riskRoot || strings.HasPrefix(importPath, riskModulePrefix)
}

// isRiskComposer reports whether a package is part of the risk-engine service,
// the module's composition root (see riskComposerPrefix).
func isRiskComposer(importPath string) bool {
	return strings.HasPrefix(importPath, riskComposerPrefix)
}

// pkgInfo is the subset of `go list -json` output the arch tests need.
// Imports is direct runtime imports; TestImports / XTestImports are
// the in-package and external test-only imports. We check all three
// because a test file reaching past the boundary is still a boundary
// violation — the impl package would gain a hidden test-only consumer
// that breaks the moment we move the impl symbol.
type pkgInfo struct {
	ImportPath   string
	Standard     bool
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// loadPackages shells out to `go list -json ./...` from the module
// root so we see every package, not just those rooted at the test's
// cwd. The output is a concatenated stream of JSON objects (one per
// package); a streaming decoder handles it without buffer assembly.
func loadPackages(t *testing.T) []pkgInfo {
	t.Helper()
	root := moduleRoot(t)
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list: %v\nstderr: %s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var pkgs []pkgInfo
	for dec.More() {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list returned no packages — check module root resolution")
	}
	return pkgs
}

// moduleRoot returns the directory containing the nearest go.mod so
// `go list ./...` enumerates the whole module rather than just the
// test's package directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	p := strings.TrimSpace(string(out))
	if p == "" || p == "/dev/null" || p == "NUL" {
		t.Fatal("not in a Go module — `go env GOMOD` returned no go.mod path")
	}
	return filepath.Dir(p)
}

// allImports returns every import path a package depends on across
// runtime, internal-test, and external-test compilation units. We
// check all three because the architecture rule applies to *any* Go
// source file in the package — production or test.
func allImports(p pkgInfo) []string {
	out := make([]string, 0, len(p.Imports)+len(p.TestImports)+len(p.XTestImports))
	out = append(out, p.Imports...)
	out = append(out, p.TestImports...)
	out = append(out, p.XTestImports...)
	return out
}

// TestRiskBoundary_OutsidersUseAPIOnly enforces the first half of
// the RISK-01 contract: callers outside the risk module may only
// import its api/v* sub-packages.
func TestRiskBoundary_OutsidersUseAPIOnly(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		if pkg.Standard {
			continue
		}
		// Risk-internal packages (the root package + sub-packages) are
		// allowed to import each other.
		if isRiskInternal(pkg.ImportPath) {
			continue
		}
		// The risk-engine service is the module's composition root — it
		// wires the impl packages into a running engine (ORCH-01/PERS-01).
		if isRiskComposer(pkg.ImportPath) {
			continue
		}
		for _, imp := range allImports(pkg) {
			if !strings.HasPrefix(imp, riskModulePrefix) {
				continue
			}
			if strings.HasPrefix(imp, riskAPIPrefix) {
				continue
			}
			t.Errorf("boundary violation: %s imports %s — outsiders must use %s* only",
				pkg.ImportPath, imp, riskAPIPrefix)
		}
	}
}

// TestRiskBoundary_APIDoesNotImportImpl enforces the second half:
// the api/v* contract surface must not leak impl types by importing
// any non-api risk sub-package.
func TestRiskBoundary_APIDoesNotImportImpl(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		if !strings.HasPrefix(pkg.ImportPath, riskAPIPrefix) {
			continue
		}
		for _, imp := range allImports(pkg) {
			if !strings.HasPrefix(imp, riskModulePrefix) {
				continue
			}
			if strings.HasPrefix(imp, riskAPIPrefix) {
				continue
			}
			t.Errorf("contract leak: %s imports %s — api/v* must not depend on impl packages",
				pkg.ImportPath, imp)
		}
	}
}
