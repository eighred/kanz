package arch

import (
	"strings"
	"testing"
)

// operatorPrefix roots every package belonging to the operator service.
const operatorPrefix = modulePath + "/services/operator/"

// venueServicePrefix is the boundary exchangeauth (internal/venueadapter/exchangeauth)
// exists to route around: services/venue-okx/internal/... and
// services/venue-binance/internal/... are unimportable from services/operator today
// only because Go's `internal/` rule happens to say so. Nothing fails the build if
// that boundary is crossed via a venue package that ISN'T under internal/ (a future
// services/venue-*/ helper that sits above internal/, for instance) — the compiler
// enforces internal/ specifically, not "operator must not depend on a venue
// service" as a design rule. This guard makes the design rule itself permanent.
const venueServicePrefix = modulePath + "/services/venue-"

// TestOperatorImportsNoVenueService walks the operator's full transitive import
// graph — direct and test imports, recursively, not just one hop — and fails if any
// package under services/venue- is reachable from it. The operator's only sanctioned
// path to exchange-auth logic is the shared internal/venueadapter/exchangeauth
// package (see services/operator/internal/venueproof), precisely so this is never
// true; a violation here means a venue service's package was imported directly,
// bypassing that shared boundary.
func TestOperatorImportsNoVenueService(t *testing.T) {
	pkgs := loadPackages(t)
	byPath := make(map[string]pkgInfo, len(pkgs))
	for _, p := range pkgs {
		byPath[p.ImportPath] = p
	}

	var operatorPkgs []string
	for _, p := range pkgs {
		if strings.HasPrefix(p.ImportPath, operatorPrefix) {
			operatorPkgs = append(operatorPkgs, p.ImportPath)
		}
	}
	// NON-VACUOUS (missing root). If the operator service is renamed or moved,
	// operatorPkgs is empty and the walk below would trivially find nothing to
	// walk — success that proves nothing.
	if len(operatorPkgs) == 0 {
		t.Fatalf("no packages found under %s — the operator service is missing or moved, this guard proves nothing", operatorPrefix)
	}

	// NON-VACUOUS (missing edges). If go list's JSON stopped reporting Imports,
	// every walk below would terminate immediately with no violation found —
	// also success that proves nothing.
	totalEdges := 0
	for _, root := range operatorPkgs {
		totalEdges += len(allImports(byPath[root]))
	}
	if totalEdges == 0 {
		t.Fatalf("operator packages report zero imports across %d packages — import data is not being read, this guard proves nothing", len(operatorPkgs))
	}

	for _, root := range operatorPkgs {
		visited := map[string]bool{}
		if offender := findVenueServiceImport(root, byPath, visited); offender != "" {
			t.Errorf("boundary violation: %s transitively imports %s — the operator must use internal/venueadapter/exchangeauth, never a services/venue-* package directly", root, offender)
		}
	}
}

// findVenueServiceImport does a depth-first search of path's transitive import
// graph (direct + test imports) for a package under venueServicePrefix, returning
// its import path, or "" if none is reachable. byPath is the full set of module
// packages from `go list -json ./...`; an import not present there is stdlib or an
// external module and cannot be a services/venue-* package, so the walk stops.
func findVenueServiceImport(path string, byPath map[string]pkgInfo, visited map[string]bool) string {
	if visited[path] {
		return ""
	}
	visited[path] = true

	if strings.HasPrefix(path, venueServicePrefix) {
		return path
	}
	p, ok := byPath[path]
	if !ok {
		return ""
	}
	for _, imp := range allImports(p) {
		if offender := findVenueServiceImport(imp, byPath, visited); offender != "" {
			return offender
		}
	}
	return ""
}
