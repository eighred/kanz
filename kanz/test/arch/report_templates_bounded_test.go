package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// NO AUDIT REPORT TEMPLATE MAY SELECT AN UNBOUNDED SLICE OF THE LOG (#304).
//
// The audit store is WORM by contract (services/audit/internal/audit/store.go
// — append-only, no update, no delete, because the AUDIT-01b tamper-evidence
// rests on it). A table that only ever grows has no steady state, so a template
// whose filter carries no Limit and no time window does not read "a lot of
// rows" — it reads EVERY record the tenant has ever produced, into the pod, and
// RenderJSON's MarshalIndent then holds a second inflated copy. That was
// `full-log`: `Filter: audit.Filter{}`, shipped and reachable at
// GET /v1/audit/reports/full-log.
//
// WHY A GUARD AND NOT A TEST. The obvious alternative is a unit test asserting
// BuiltIns()["full-log"].Filter.Limit != 0, and it is strictly weaker: it fixes
// the one template that was found. The failure mode here is a template ADDED
// later — templates are explicitly designed as data ("adding one is config, not
// code", the AUDIT-01d requirement), so the next one arrives from someone
// copying a neighbour, and the neighbour they copy might be the empty one. This
// is default-deny over the whole table, so the commit that adds an unbounded
// template is the commit that goes red.
//
// It also cannot be satisfied by a Kind alone, deliberately. A Kind narrows the
// selection but does not BOUND it: `authz-decisions` is every authorization
// decision the tenant has ever made, which on a busy estate is the largest of
// the four, not a small one. Only Limit or a Since/Until window is accepted.
//
// WHAT THIS GUARD DOES NOT COVER, said plainly so it is not read as more than it
// is: it reads the shipped BuiltIns table in source. A deployment that loads
// templates from JSON at runtime is outside its reach — that path is guarded
// instead by report.Generate returning ErrUnboundedTemplate, which refuses a
// Limit-less template on the request rather than serving it. The two are
// complementary: this one fails the build, that one fails the call.
const templatesFile = "services/audit/internal/report/templates.go"

func TestNoAuditReportTemplateIsUnbounded(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(templatesFile))

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v — the guard cannot check what it cannot read", templatesFile, err)
	}

	filters := templateFilters(t, f)

	// NON-VACUITY. If the map is renamed, moved or restructured, this guard would
	// otherwise find zero templates and pass — reporting "no unbounded templates"
	// about a file it no longer understands. An empty result is a guard failure,
	// not a clean bill of health.
	if len(filters) == 0 {
		t.Fatalf("found no report templates in %s. The guard locates them as `Filter: audit.Filter{...}` "+
			"inside BuiltIns; if the table moved or changed shape, MOVE THIS GUARD WITH IT rather than "+
			"leaving it passing over a file it cannot parse", templatesFile)
	}

	var unbounded []string
	for name, fields := range filters {
		_, hasLimit := fields["Limit"]
		_, hasSince := fields["Since"]
		_, hasUntil := fields["Until"]
		if !hasLimit && !hasSince && !hasUntil {
			unbounded = append(unbounded, name)
		}
	}
	sort.Strings(unbounded)

	if len(unbounded) > 0 {
		t.Fatalf("report template(s) %v carry a filter with no Limit and no Since/Until.\n\n"+
			"The audit log is append-only and only grows, so this reads the tenant's ENTIRE history into "+
			"memory on one GET and marshals a second indented copy — an export with no upper bound on "+
			"time or memory. Give the template `Limit: DefaultPageSize` (the route pages with "+
			"?after=<next_cursor>), or a Since/Until window.\n\n"+
			"A Kind is NOT a bound: authz-decisions is every decision the tenant has ever made.\n\n"+
			"Found in %s. See #304.", unbounded, templatesFile)
	}
}

// templateFilters returns, per template name, the set of field names set in that
// template's audit.Filter literal.
//
// It keys on the template's own Name/map key rather than on position, so a
// reordered table reports the same thing.
func templateFilters(t *testing.T, f *ast.File) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}

	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		// The map entry: "full-log": {Name: ..., Filter: audit.Filter{...}}.
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			return true
		}
		tmpl, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return true
		}
		name := strings.Trim(key.Value, `"`)

		for _, elt := range tmpl.Elts {
			field, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			ident, ok := field.Key.(*ast.Ident)
			if !ok || ident.Name != "Filter" {
				continue
			}
			lit, ok := field.Value.(*ast.CompositeLit)
			if !ok {
				continue
			}
			set := map[string]bool{}
			for _, fe := range lit.Elts {
				fkv, ok := fe.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				fid, ok := fkv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				// A field written as an explicit zero — Limit: 0 — is NOT a
				// bound. postgres.go emits no LIMIT clause at all for it
				// (`if f.Limit > 0`), so treating it as "set" would let the
				// exact defect back in wearing the field name that fixes it.
				if isZeroLiteral(fkv.Value) {
					continue
				}
				set[fid.Name] = true
			}
			out[name] = set
		}
		return true
	})
	return out
}

// isZeroLiteral reports whether e is the literal 0, so `Limit: 0` is not counted
// as a Limit.
func isZeroLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && strings.TrimLeft(lit.Value, "0") == ""
}
