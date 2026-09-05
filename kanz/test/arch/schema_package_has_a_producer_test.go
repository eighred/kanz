package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A SCHEMA PACKAGE NOTHING PRODUCES IS A CONTRACT THIS PLATFORM DOES NOT HONOUR
// (#1039).
//
// # The case that produced this guard
//
// kanz-schemas/proto/factor/v1 declares FactorModel, FactorExposure,
// FactorReturn and FactorCovariance — the multi-factor risk model, every message
// keyed model_id + as_of, with a package doc explaining the point-in-time
// discipline a historical recompute reads them under. It is generated into the
// Go SDK, the Python SDK and the TypeScript SDK. Three published SDKs.
//
// `grep -rn "kanz-schemas-go/factor/v1" kanz/` returned NOTHING. Not one
// producer, not one consumer, in any language. Meanwhile the risk engine fitted
// a factor model per evaluation, computed FactorVaR99 off it, published the
// number, and discarded the loadings and the covariance when the call returned.
// The schema built for exactly that artifact sat unused beside the code throwing
// the artifact away, for as long as both existed, and nothing anywhere reported
// it.
//
// # Why "a producer", specifically, and not "an importer"
//
// This is the schema-package granularity of the same rule
// stored_series_has_a_producer_test.go applies to enumerated constants and
// store_has_a_writer_test.go applies to tables: a declared artifact must have a
// WRITER. An import proves somebody can read the type; it does not prove any
// value of it was ever created, and a consumer-only package is a decoder for
// messages that never arrive — the empty-partition failure whose symptom is
// "we hold none of those" rather than "nobody ever wrote any".
//
// So the evidence required is that non-test Go under kanz/ MATERIALIZES A VALUE
// of one of the package's message types — `&factorpb.FactorModel{...}`,
// `var m altpb.Commitment`, `new(wealthpb.HouseholdValued)`. An import that only
// names a type in a signature, or reads an enum constant, is not enough.
//
// THE THREE SPELLINGS ARE ALL THREE REQUIRED, and the second is why. cmd/kanz-
// altevent and cmd/kanz-household are real producers — they publish
// alternatives.v1 and wealth.v1 FACTs onto the bus — and NEITHER writes a
// composite literal: both declare `var m pb.T` and fill it by unmarshalling an
// operator's JSON. A composite-literal-only rule reported both as dark on the
// first run of this guard. That is the "guard that checks a weaker property than
// its name claims" failure, caught here by calibration rather than in review.
//
// # What it does NOT prove, stated because a guard trusted past its reach is
// worse than none
//
//   - MATERIALIZING IS NOT PUBLISHING. `var m pb.T` is also how a CONSUMER
//     decodes, so a package used only to decode messages somebody else produces
//     satisfies this guard. Separating the two needs the value tracked to a
//     marshal or a publish, which is a taint analysis rather than a pattern
//     match. The residue is bounded and the motivating defect is not in it:
//     factor.v1 was named by NOTHING — no producer, no consumer, no signature —
//     and total absence is what this catches, with margin.
//   - It sees GO only. A package produced solely from kanz-py or kanz-web reads
//     as dark here. That was checked by hand when this guard was written — every
//     package the exemptions below name was dark in all three SDKs — and it is
//     why each reason says which languages were searched.
//   - It does not prove the producer is REACHED. A construction inside a
//     function nothing calls satisfies it. no_dark_measure_seam_test.go is the
//     guard for that class, and the two are complementary rather than
//     overlapping.
func TestEverySchemaPackageHasAProducer(t *testing.T) {
	root := moduleRoot(t)
	repo := filepath.Dir(root) // kanz/ and kanz-schemas/ are siblings

	packages := schemaPackages(t, filepath.Join(repo, "kanz-schemas", "proto"))
	// NON-VACUITY, FIRST ARM. A scan that finds no proto packages — a moved
	// directory, a renamed layout — passes every assertion below while the estate
	// grows its next factor.v1.
	if len(packages) < 20 {
		t.Fatalf("found only %d proto packages under kanz-schemas/proto — the scan is broken, "+
			"not the estate; this repository declares far more", len(packages))
	}

	produced, constructions := schemaPackageProducers(t, root, packages)
	// NON-VACUITY, SECOND ARM. If the AST walk stops resolving constructions —
	// an import-alias assumption that stops holding, a walk that never enters
	// services/ — every package looks dark at once and the exemption map is the
	// only thing standing between that and a red build. Both floors are far
	// below the current numbers and far above zero.
	if constructions < 200 {
		t.Fatalf("resolved only %d schema-message constructions across kanz/ — the AST scan is "+
			"broken, not the estate", constructions)
	}
	if len(produced) < 15 {
		t.Fatalf("only %d of %d proto packages resolved to ANY producer — the scan is broken, "+
			"not the estate", len(produced), len(packages))
	}

	var dark []string
	for _, p := range packages {
		if produced[p] {
			continue
		}
		if _, exempt := schemaPackageWithoutAProducerExempt[p]; exempt {
			continue
		}
		dark = append(dark, p)
	}
	sort.Strings(dark)
	if len(dark) > 0 {
		t.Errorf("%d proto package(s) are generated into the SDKs and nothing in kanz/ ever "+
			"constructs a message from them: %v\n\n"+
			"A schema with no producer is a contract the platform publishes and does not honour. "+
			"factor.v1 sat in three SDKs for months while the risk engine fitted the model it "+
			"describes and threw it away every call (#1039). Either write the producer, or add "+
			"an entry to schemaPackageWithoutAProducerExempt naming the issue that will.",
			len(dark), dark)
	}

	// THE DEAD-ENTRY ARM. An exemption that outlives its repair is how the next
	// dark package gets waved through: the map grows, nobody re-reads it, and the
	// guard degrades into a list of things it is allowed to ignore.
	for pkg, reason := range schemaPackageWithoutAProducerExempt {
		if !schemaPackageListed(packages, pkg) {
			t.Errorf("exemption for %q names no proto package — the directory is gone or was "+
				"renamed. Delete the entry (%s)", pkg, reason)
			continue
		}
		if produced[pkg] {
			t.Errorf("exemption for %q is stale — the package now has a producer. Delete the "+
				"entry (%s)", pkg, reason)
		}
	}
}

// schemaPackageWithoutAProducerExempt names each proto package generated into
// the SDKs that nothing in kanz/ produces, with why.
//
// EVERY ENTRY HERE IS A REAL FINDING, not a design decision. They are the same
// class of defect factor.v1 was, found by the same scan on the same day, and
// each was checked against kanz-py/ and kanz-web/ as well as kanz/ — none of the
// four is produced in any language. They are listed rather than fixed because
// each is its own piece of work with its own data seam, and a guard that lands
// with four unrelated features attached is a guard that does not land.
var schemaPackageWithoutAProducerExempt = map[string]string{
	"performance/v1": "PERF-01 return/attribution wire shapes. The performance module computes " +
		"attribution in-process and serves it over the query surface; nothing ever builds a " +
		"performance.v1 message. #1039 found it; it needs its own issue and its own producer.",
	"regulatory/v1": "Regulatory filing wire shapes. The filing path assembles its own in-process " +
		"shapes and this package is named by neither Go, Python nor TypeScript. #1039 found it; " +
		"it needs its own issue and its own producer.",
	"sustainability/v1": "Climate/ESG wire shapes. Same finding as regulatory/v1 and found by the " +
		"same scan on 2026-09-05 — declared, generated into three SDKs, produced by nothing in " +
		"any of them. #1039 found it; it needs its own issue and its own producer.",
	"xva/v1": "XVA (CVA/DVA/FVA) wire shapes. internal/risk/xva computes the adjustments and the " +
		"credit curve behind them entirely in Go structs; the proto package is named nowhere. " +
		"#1039 found it; it needs its own issue and its own producer.",
}

// schemaPackages returns every "<domain>/<version>" directory under
// kanz-schemas/proto that holds at least one .proto file — DERIVED from the
// schema tree rather than listed here, because "somebody enumerated the set by
// hand and missed a member" is the exact defect this guard exists to catch, and
// a list written here would be one more copy of the thing that broke.
func schemaPackages(t *testing.T, protoRoot string) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(protoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Sibling agent worktrees and build scratch never hold schemas.
			if n := d.Name(); n == ".claude" || n == ".git" || n == ".gotmp" || n == "gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".proto") {
			return nil
		}
		rel, relErr := filepath.Rel(protoRoot, path)
		if relErr != nil {
			return relErr
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 {
			return nil // not <domain>/<version>/<file>.proto
		}
		seen[parts[0]+"/"+parts[1]] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v — the guard cannot check what it cannot read", protoRoot, err)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// schemaPackageProducers walks non-test Go under kanz/ and reports which schema
// packages have at least one message constructed from them, plus the total
// number of constructions resolved (the non-vacuity denominator).
func schemaPackageProducers(t *testing.T, root string, packages []string) (map[string]bool, int) {
	t.Helper()
	const importPrefix = "github.com/eighred/kanz/kanz-schemas-go/"

	known := map[string]bool{}
	for _, p := range packages {
		known[p] = true
	}

	produced := map[string]bool{}
	total := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// .claude holds sibling agent worktrees — a guard that walked into one
			// would read another agent's half-written tree as this repository's.
			case ".claude", ".git", ".gotmp", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file this walk cannot parse is not a pass. Generated or not, an
			// unparseable file could be the only producer of a package.
			t.Fatalf("parse %s: %v", path, perr)
		}

		// alias -> schema package ("domain/v1"). Unaliased imports resolve to the
		// generated package name, which protoc-gen-go derives as <domain><version>.
		aliases := map[string]string{}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || !strings.HasPrefix(p, importPrefix) {
				continue
			}
			pkg := strings.TrimPrefix(p, importPrefix)
			if !known[pkg] {
				continue
			}
			name := strings.ReplaceAll(pkg, "/", "")
			if imp.Name != nil {
				name = imp.Name.Name
			}
			aliases[name] = pkg
		}
		if len(aliases) == 0 {
			return nil
		}

		record := func(e ast.Expr) {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return
			}
			if pkg, ok := aliases[id.Name]; ok {
				produced[pkg] = true
				total++
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit: // &pb.T{...}
				record(v.Type)
			case *ast.ValueSpec: // var m pb.T
				record(v.Type)
			case *ast.CallExpr: // new(pb.T)
				if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "new" && len(v.Args) == 1 {
					record(v.Args[0])
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v — the guard cannot check what it cannot read", root, err)
	}
	return produced, total
}

func schemaPackageListed(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
