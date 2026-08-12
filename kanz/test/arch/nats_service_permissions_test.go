package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// SEC-M3e: tenancy.yaml's `permissions` blocks are hand-derived from the code —
// exactly the shape SEC-M3d already warned rots the moment someone adds a
// publish. This guard is the thing that makes that claim survive a second
// change: it derives each service's PUBLISH set from code, the same way
// subject_topology_test.go's declaredSubjects derives the estate-wide set (AST,
// not a fresh regex scanner), and fails the build if a service's code names a
// subject its own tenancy.yaml `permissions.publish` does not allow.
//
// A service denied at publish time by its OWN broker account authenticates
// FINE and then fails on every publish with a permissions violation — which
// reads as a broken feature, not a missing grant, exactly the failure mode
// this file's header (SEC-M3b) already describes for identity. This is the
// same failure one layer down, at the subject level instead of the account
// level.
//
// SCOPE, HONESTLY (same stance as subject_topology_test.go's own header):
// this resolves a `bus.Event{Subject: ...}` literal directly, a package-level
// const/var it names (one or more import hops), simple same-function local
// variable assignments, and two bounded one-function-call hops, same package
// only: (a) the argument passed at a small private emit/publish helper's call
// sites, when the helper's own Subject: field names one of its parameters; and
// (b) a callee's OWN return tuple — `subject, ... := f(...)` — folded through
// f's return statements, including every branch of a switch/if inside f that
// returns a different literal (accounting's cashmove.Kind.wire() is exactly
// this shape: three literals, one per Kind, and ALL three are real, reachable
// results, not one ambiguous one). Both hops are matched by NAME within the
// same directory only — no receiver-type tracking, the same accepted
// collision risk resolveParamAcrossCallSites already took before hop (b)
// existed.
//
// It does NOT chase a SECOND hop (a callee calling another callee in turn),
// trace through interfaces, maps, or anything assembled from concatenation
// (market-data's BusSink.eventType is exactly this excluded shape — an
// fmt.Sprintf over a config-injected field — and this guard correctly
// resolves it to zero evidence). Where it cannot resolve a Subject: value to
// one or more literals, it SKIPS that call site rather than guessing — an
// unresolved subject is a false negative (accepted, and logged at -v), never a
// false positive that fails the build on correct code. market-data's wildcard
// grant is verified separately, by
// TestMarketDataPublishGrantCoversItsConfigInjectedWildcard below, against the
// parts of that same excluded Sprintf that ARE fixed (the domain constant and
// the closed set of variant suffixes) — a code-checked bound rather than a
// rubber-stamped allow-list, since the middle segment genuinely cannot be
// folded to a literal.
func TestServicePublishesOnlySubjectsItsTenancyPermissionsAllow(t *testing.T) {
	root := moduleRoot(t)
	perms := servicePublishPermissions(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if len(perms) == 0 {
		t.Fatal("no __system__ kanz-services permissions parsed from tenancy.yaml — has the format changed?")
	}

	rc := newResolveCache(root)

	var problems []string
	checked := 0
	for _, svc := range servicesWithEntrypoints(t, root) {
		if _, undeployed := notDeployed[svc]; undeployed {
			continue
		}
		if !dialsNATS(t, root, svc) {
			continue
		}
		svid := systemAccountSVID(svc)
		perm, ok := perms[svid]
		if !ok {
			problems = append(problems, svc+": tenancy.yaml has no `permissions` block for "+svid+
				" at all — every subject it publishes is unverifiable and, per NATS semantics, "+
				"UNRESTRICTED within __system__ (the exact SEC-M3e exposure this guard exists to end)")
			continue
		}
		checked++

		subjects := servicePublishedSubjects(t, rc, root, svc)
		var files sortedSubjects
		for subj, sites := range subjects {
			files = append(files, subjectSites{subject: subj, sites: sites})
		}
		sort.Sort(files)
		for _, ss := range files {
			if permDenies(ss.subject, perm) || !covered(ss.subject, perm.allow) {
				problems = append(problems, svc+": publishes "+strconv.Quote(ss.subject)+" ("+strings.Join(ss.sites, ", ")+
					") but tenancy.yaml's permissions.publish for "+svid+" does not allow it — the service will "+
					"authenticate fine and then be DENIED at publish time (a NATS permissions violation), which "+
					"looks like a broken feature, not a missing grant")
			}
		}
	}

	// Non-vacuity: this estate definitely has NATS-dialing services with resolvable
	// literal publishes (venue-binance's "market.crypto.trade" alone guarantees it).
	// A scan that checked zero services would pass no matter how wrong the tenancy
	// file was, which is the exact failure this guard exists to prevent.
	if checked == 0 {
		t.Fatal("checked zero services against tenancy.yaml permissions — the scanner or the service list is broken")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d service(s) publish a subject tenancy.yaml does not permit:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

type subjectSites struct {
	subject string
	sites   []string
}
type sortedSubjects []subjectSites

func (s sortedSubjects) Len() int           { return len(s) }
func (s sortedSubjects) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s sortedSubjects) Less(i, j int) bool { return s[i].subject < s[j].subject }

// permDenies reports whether perm's deny list blocks subject. archiver's
// `publish: { deny: [">"] }` is the shape this exists for: an empty `allow`
// list was PROVEN (service-user-permissions-report.md) not to restrict
// anything on this nats-server version, so a truly publish-nothing service
// must use `deny`, and this guard must honor it as authoritative over `allow`.
func permDenies(subject string, perm publishPerm) bool {
	return covered(subject, perm.deny)
}

// ---- tenancy.yaml permissions parsing (plain line/brace scanner, matching the
// style nats_identity_test.go's systemAccountUsers / operatorServiceAccounts
// already use for this same hand-written file) --------------------------------

type publishPerm struct {
	allow []string
	deny  []string
}

// servicePublishPermissions returns, for every __system__ user with a
// `permissions` block, its publish allow/deny lists, keyed by SVID. A user
// with NO permissions block at all is intentionally OMITTED — callers must
// treat "absent" as "unverified / unrestricted", never as "denies everything".
func servicePublishPermissions(t *testing.T, path string) map[string]publishPerm {
	t.Helper()
	out := map[string]publishPerm{}
	for svid, entry := range systemUserEntries(t, path) {
		out[svid] = parsePublishPerm(entry)
	}
	return out
}

// serviceSubscribeAllow returns, for every __system__ user with a
// `permissions` block, its subscribe allow/deny lists, keyed by SVID — the
// subscribe-side twin of servicePublishPermissions, parsed from the exact same
// entry text so the two can never see a different shape of the same user.
// SEC-M3f (nats_jetstream_machinery_test.go) is the first caller: `_INBOX.>`
// lives here, not in publish.
func serviceSubscribeAllow(t *testing.T, path string) map[string]publishPerm {
	t.Helper()
	out := map[string]publishPerm{}
	for svid, entry := range systemUserEntries(t, path) {
		out[svid] = parseSubscribePerm(entry)
	}
	return out
}

// systemUserEntries isolates tenancy.yaml's __system__ account (brace-depth —
// same technique systemAccountUsers in nats_identity_test.go uses) and
// returns, per SVID, the raw text of that user's own entry — from its own
// opening `{` to its own matching closing `}`. servicePublishPermissions and
// serviceSubscribeAllow both parse from here, so the file is walked and
// brace-counted exactly once per call site, not reimplemented per permission
// direction.
//
// Whole-line comments (every comment in this file is its own `#`-prefixed
// line — verified, no trailing inline comments exist here) are dropped before
// counting braces: several of them quote NATS config shapes for illustration
// ("`permissions: { publish: { allow: [] } }`" split across two comment
// lines), and counting those braces as real structure would corrupt the depth
// tracking below.
//
// Entry boundaries are found by absolute brace depth, not by assuming `{` and
// `user:` share a line: the four operator entries (kanz-halt, kanz-mandate,
// kanz-altevent, kanz-household) open with a lone `{` on its own line, one
// line above `user: "..."` — the shape every service entry does NOT use (it
// opens `{ user: "..."` on one line). A scan keyed off the `user:` line's own
// brace count misses the operator shape entirely and returns a one-line,
// permissions-less "entry" for all four — invisible until something actually
// reads their subscribe block, which is exactly what this file's new
// $JS.ACK.>/_INBOX.> guard is the first to do.
func systemUserEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tenancy.yaml: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	text := strings.Join(kept, "\n")

	idx := strings.Index(text, "__system__")
	if idx < 0 {
		t.Fatal("tenancy.yaml has no __system__ account — has the format changed?")
	}
	open := strings.IndexByte(text[idx:], '{')
	if open < 0 {
		t.Fatal("__system__ has no opening brace — has the format changed?")
	}

	out := map[string]string{}
	depth := 1 // positioned just past __system__'s own opening brace
	entryStart := -1
	for i := idx + open + 1; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
			if depth == 2 {
				entryStart = i
			}
		case '}':
			depth--
			if depth == 1 && entryStart >= 0 {
				entry := text[entryStart : i+1]
				if m := userLine.FindStringSubmatch(entry); m != nil {
					out[m[1]] = entry
				}
				entryStart = -1
			}
			if depth == 0 {
				return out // __system__'s own closing brace
			}
		}
	}
	return out
}

var quotedRe = regexp.MustCompile(`"([^"]+)"`)

// parsePublishPerm extracts the publish allow/deny arrays from one user entry's
// text. entryText with no `publish:` key returns a zero-value publishPerm
// (empty allow, empty deny) — the caller distinguishes "no permissions block at
// all" separately, by testing map membership, not by this return value.
func parsePublishPerm(entryText string) publishPerm {
	pubIdx := strings.Index(entryText, "publish:")
	if pubIdx < 0 {
		return publishPerm{}
	}
	rest := entryText[pubIdx+len("publish:"):]
	// publish's own block ends at its matching close-brace; `subscribe:` (this
	// file's only sibling key) always follows it, so bounding the scan there is
	// exactly as reliable as brace-counting here and far simpler.
	if subIdx := strings.Index(rest, "subscribe:"); subIdx >= 0 {
		rest = rest[:subIdx]
	}
	return publishPerm{
		allow: extractQuotedAfter(rest, "allow:"),
		deny:  extractQuotedAfter(rest, "deny:"),
	}
}

// parseSubscribePerm extracts the subscribe allow/deny arrays from one user
// entry's text — the subscribe-side twin of parsePublishPerm. `subscribe:` is
// always the last key in an entry (publish precedes it, nothing follows it),
// so unlike parsePublishPerm there is no sibling key to bound the scan at;
// extractQuotedAfter's own "up to the first ']'" bound is sufficient.
func parseSubscribePerm(entryText string) publishPerm {
	subIdx := strings.Index(entryText, "subscribe:")
	if subIdx < 0 {
		return publishPerm{}
	}
	rest := entryText[subIdx+len("subscribe:"):]
	return publishPerm{
		allow: extractQuotedAfter(rest, "allow:"),
		deny:  extractQuotedAfter(rest, "deny:"),
	}
}

func extractQuotedAfter(s, key string) []string {
	idx := strings.Index(s, key)
	if idx < 0 {
		return nil
	}
	rest := s[idx+len(key):]
	end := strings.Index(rest, "]")
	if end < 0 {
		return nil
	}
	var out []string
	for _, m := range quotedRe.FindAllStringSubmatch(rest[:end], -1) {
		out = append(out, m[1])
	}
	return out
}

// ---- code-side: what does a service's tree actually publish? ----------------

// resolveCache memoizes parsed packages (by directory) across the whole run —
// the same shared package (e.g. internal/execution) is reached from more than
// one service via an import hop, and re-parsing it per service would be
// wasted work for no extra precision.
type resolveCache struct {
	root  string
	fset  *token.FileSet
	files map[string][]*ast.File // dir -> parsed non-test .go files
}

func newResolveCache(root string) *resolveCache {
	return &resolveCache{root: root, fset: token.NewFileSet(), files: map[string][]*ast.File{}}
}

func (rc *resolveCache) packageFiles(t *testing.T, dir string) []*ast.File {
	if fs, ok := rc.files[dir]; ok {
		return fs
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		rc.files[dir] = nil
		return nil
	}
	var fs []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(rc.fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), perr)
		}
		fs = append(fs, f)
	}
	rc.files[dir] = fs
	return fs
}

// importDir resolves an import alias used in file to the on-disk directory it
// names, if (and only if) it is a package inside this module — an external
// module's constants are out of scope (this platform's subjects are never
// declared in a third-party dependency).
func (rc *resolveCache) importDir(file *ast.File, alias string) (string, bool) {
	const modulePrefix = "github.com/eighred/kanz/"
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		} else {
			segs := strings.Split(path, "/")
			name = segs[len(segs)-1]
		}
		if name != alias {
			continue
		}
		if !strings.HasPrefix(path, modulePrefix) {
			return "", false
		}
		return filepath.Join(rc.root, filepath.FromSlash(strings.TrimPrefix(path, modulePrefix))), true
	}
	return "", false
}

// resolveIdentInPackage looks up name as a package-level const/var declared in
// any file already loaded for dir, and folds its value (recursing through
// further idents/selectors, depth-bounded).
func (rc *resolveCache) resolveIdentInPackage(t *testing.T, dir, name string, depth int) []string {
	for _, f := range rc.packageFiles(t, dir) {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range vs.Names {
					if n.Name != name || i >= len(vs.Values) {
						continue
					}
					return rc.resolveExpr(t, f, dir, vs.Values[i], nil, depth-1)
				}
			}
		}
	}
	return nil
}

// resolveExpr folds expr to every literal string it can statically prove —
// see this file's header for exactly what it does and does not chase.
func (rc *resolveCache) resolveExpr(t *testing.T, file *ast.File, dir string, expr ast.Expr, fn *ast.FuncDecl, depth int) []string {
	if depth <= 0 {
		return nil
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return nil
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return nil
		}
		return []string{s}

	case *ast.Ident:
		// 1) a same-function local: every assignment to this name within fn.
		if fn != nil {
			if vals := rc.resolveLocalAssignments(t, file, dir, fn, e.Name, depth); vals != nil {
				return vals
			}
			// 2) a parameter of fn: trace exactly one function-call hop within the
			// same directory (package) to every call site, same package only.
			if idx, isParam := paramIndex(fn, e.Name); isParam {
				return rc.resolveParamAcrossCallSites(t, dir, fn, idx, depth)
			}
		}
		// 3) a package-level const/var in the same directory.
		return rc.resolveIdentInPackage(t, dir, e.Name, depth)

	case *ast.SelectorExpr:
		pkgIdent, ok := e.X.(*ast.Ident)
		if !ok {
			return nil
		}
		targetDir, ok := rc.importDir(file, pkgIdent.Name)
		if !ok {
			return nil
		}
		return rc.resolveIdentInPackage(t, targetDir, e.Sel.Name, depth)

	case *ast.CallExpr:
		// bus.TenantRoutedSubject(tenant, X) — the MT-02 tenant routing wrapper
		// (#358/#360). Without this case the scanner resolves NOTHING for a
		// publisher that routes by tenant, and this guard goes SILENTLY BLIND to
		// it: measured, by deleting webhook-ingest's order.order.submit grant and
		// watching the guard stay green.
		//
		// That is worse than the gap it was written to close. A publisher whose
		// subject the scanner cannot see is indistinguishable from one that
		// publishes nothing, so widening a wrapper's use quietly removes services
		// from this check one at a time.
		//
		// Both forms are returned, because both must be granted: the WIRE subject
		// carries the prefix, and the unprefixed logical name is what the same
		// service still publishes on any path that has not moved.
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "TenantRoutedSubject" && len(e.Args) == 2 {
			var out []string
			for _, base := range rc.resolveExpr(t, file, dir, e.Args[1], fn, depth-1) {
				out = append(out, base, "tenant.*."+base)
			}
			return out
		}
		return nil
	}
	return nil
}

// resolveLocalAssignments collects every literal (or further-resolvable) value
// assigned to name via `:=` or `=` anywhere in fn's body — an if/else assigning
// two different subjects (venue userdata handlers do exactly this) means BOTH
// are real, reachable publish subjects, not a single ambiguous one. Returns nil
// (not found), as opposed to an empty non-nil slice, when name is never
// assigned in fn — the caller uses nil to mean "try the next resolution path".
func (rc *resolveCache) resolveLocalAssignments(t *testing.T, file *ast.File, dir string, fn *ast.FuncDecl, name string, depth int) []string {
	var found bool
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// `a, b, c := f(...)`: one call populates every name in Lhs, so a
		// matching name's position in Lhs is its position in f's RETURN
		// tuple, not an index into Rhs (Rhs has exactly one element here:
		// the call itself). cashmove.encode's `subject, entryType, ok :=
		// m.Kind.wire()` is exactly this shape.
		tuple := len(assign.Rhs) == 1 && len(assign.Lhs) > 1
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name {
				continue
			}
			if tuple {
				found = true
				out = append(out, rc.resolveCallTuple(t, file, dir, assign.Rhs[0], i, depth-1)...)
				continue
			}
			if i >= len(assign.Rhs) {
				continue
			}
			found = true
			out = append(out, rc.resolveExpr(t, file, dir, assign.Rhs[i], fn, depth-1)...)
		}
		return true
	})
	if !found {
		return nil
	}
	return out
}

// resolveCallTuple resolves the value that would be assigned to the idx'th
// name in a tuple assignment (`a, b, c := f(...)`) whose single right-hand
// side is a function or method call — the return-side counterpart to
// resolveParamAcrossCallSites' argument-side hop. It finds the function or
// method the call names — same directory (package) only, matched by NAME,
// receiver type not tracked (small private helpers like Kind.wire have no
// name collision in practice; see this file's header) — and folds every
// return statement's idx'th result expression via foldReturnValues, so a
// switch/if inside the callee returning several different literals yields
// ALL of them, not one ambiguous choice.
func (rc *resolveCache) resolveCallTuple(t *testing.T, file *ast.File, dir string, rhs ast.Expr, idx, depth int) []string {
	if depth <= 0 {
		return nil
	}
	call, ok := rhs.(*ast.CallExpr)
	if !ok {
		return nil
	}
	name := ""
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		name = fun.Sel.Name
	default:
		return nil
	}
	calleeFile, calleeFn := rc.findFuncByName(t, dir, name)
	if calleeFn == nil {
		return nil
	}
	return rc.foldReturnValues(t, calleeFile, dir, calleeFn, idx, depth)
}

// foldReturnValues folds every literal (or further-resolvable) value fn
// returns at result position idx. It is the shared core behind
// resolveCallTuple (reached mid-scan, from an unresolved call site) and
// TestMarketDataPublishGrantCoversItsConfigInjectedWildcard (reached
// directly, from a function already known by name — bussink.go's variant()).
func (rc *resolveCache) foldReturnValues(t *testing.T, file *ast.File, dir string, fn *ast.FuncDecl, idx, depth int) []string {
	if depth <= 0 || fn.Body == nil {
		return nil
	}
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || idx >= len(ret.Results) {
			return true
		}
		out = append(out, rc.resolveExpr(t, file, dir, ret.Results[idx], fn, depth-1)...)
		return true
	})
	return out
}

// findFuncByName finds a top-level function or method declared in dir's
// package (already-loaded files only — callers reach here via a directory
// already scanned for this same service) whose name is name — plain function
// or method, receiver type not checked (see resolveCallTuple's doc). Returns
// the first match; this package has no same-name collisions among the small
// private helpers this reaches (cashmove.Kind.wire, bussink.variant).
func (rc *resolveCache) findFuncByName(t *testing.T, dir, name string) (*ast.File, *ast.FuncDecl) {
	for _, f := range rc.packageFiles(t, dir) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != name || fn.Body == nil {
				continue
			}
			return f, fn
		}
	}
	return nil, nil
}

// paramIndex reports the index of fn's parameter named name, across a
// flattened parameter list (multi-name fields like `a, b string` counted
// individually, matching Go's own positional argument order).
func paramIndex(fn *ast.FuncDecl, name string) (int, bool) {
	if fn.Type.Params == nil {
		return 0, false
	}
	i := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if n.Name == name {
				return i, true
			}
			i++
		}
	}
	return 0, false
}

// resolveParamAcrossCallSites finds every call, anywhere in dir's package,
// naming fn (by identifier, for a plain function, or by selector, for a
// method — matched by NAME only, same heuristic style as busConsumerCalls'
// isSelector; this package has no receiver-type collisions on these small
// private helpers) and resolves the argument at paramIdx. This is the ONE
// function-call hop this resolver performs; it does not chase an argument that
// is itself another function's parameter (depth still bounds recursion, but
// resolveExpr's *ast.Ident case only re-enters resolveParamAcrossCallSites from
// within the ORIGINAL fn, not from a callee found here, since callSiteFn below
// is always fn itself's callers, evaluated with fn set to nil).
func (rc *resolveCache) resolveParamAcrossCallSites(t *testing.T, dir string, fn *ast.FuncDecl, paramIdx, depth int) []string {
	var out []string
	for _, f := range rc.packageFiles(t, dir) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || paramIdx >= len(call.Args) {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}
			if name != fn.Name.Name {
				return true
			}
			// Evaluated with fn=nil: caller-side arguments are resolved as
			// literals / package consts only, not chased through a further
			// parameter — this is the hop bound.
			out = append(out, rc.resolveExpr(t, f, dir, call.Args[paramIdx], nil, depth)...)
			return true
		})
	}
	return out
}

// crossPackagePublishSurfaces names, for a service whose actual bus.Event
// construction happens in a SHARED package it calls into rather than literally
// inside services/<name>, which shared package(s) to ALSO scan for that
// service.
//
// This is deliberately a small, hand-maintained, EXPLICIT list — not "every
// internal/ package this service imports". A service routinely imports a
// shared package for a type or interface it reads and never calls the
// PUBLISHING side of: e.g. oms and compliance both import internal/compliance
// for the mandate types they consume, but internal/compliance/publisher.go's
// Publish is only ever called by cmd/kanz-mandate. Blanket-including every
// import would misattribute kanz-mandate's own subject to both oms and
// compliance, which do not and must not have a compliance.mandate.changed
// PUBLISH grant (they only subscribe to it). Each entry below was verified by
// reading the composition root's actual call site — see
// .superpowers/sdd/service-user-permissions-report.md.
var crossPackagePublishSurfaces = map[string][]string{
	// risk-engine's RISK-10 output (EventTypeExposureRecomputed /
	// EventTypeMeasuresComputed) is emitted by internal/risk/publish.Publisher,
	// constructed and called from services/risk-engine's composition root.
	"risk-engine": {"internal/risk/publish"},
	// market-ingest's book-snapshot publish is internal/marketedge/ingest's
	// Engine, run by pkg/alpha.Runner — itself wired from
	// services/market-ingest/cmd/market-ingest/main.go.
	//
	// internal/marketedge/bars is the SECOND publisher on that runner (#425): the
	// 1-minute candle collector, teed off the same trade feeds. It is listed
	// because this derivation is per-package and would otherwise PASS BY NOT
	// LOOKING — the subject would be published by a service whose grant nothing
	// checked, and the failure mode is a NATS permissions denial in production
	// with a green suite behind it.
	"market-ingest": {"internal/marketedge/ingest", "internal/marketedge/bars"},
	// webhook-ingest's signal + order-command publishes are
	// internal/signal/translate's Translator, constructed and driven from
	// services/webhook-ingest/internal/ingest (pipeline.go's tr.Emit).
	"webhook-ingest": {"internal/signal/translate"},
	// AUTH-01d (#352): both of these construct pkg/authbus.NewBusRecorder at their
	// composition root, and the bus.Event for platform.authz.decision is built
	// inside authbus — neither service names the subject anywhere in its own tree.
	//
	// WITHOUT THESE TWO LINES THE GRANT IS UNVERIFIED, and that was measured, not
	// assumed: with the entries absent, replacing "platform.authz.decision" in
	// either service's tenancy.yaml permissions with a subject that does not exist
	// left this guard GREEN. The block's presence was checked; its contents were
	// not. A wrong or deleted grant would then be found only at runtime, by the
	// broker denying every authorization decision.
	"api-gateway": {"pkg/authbus"},
	"copilot":     {"pkg/authbus"},
}

// servicePublishedSubjects walks services/<svc> (recursively — a service is a
// tree of packages: cmd/, internal/foo, internal/bar, ...) PLUS any directory
// named for it in crossPackagePublishSurfaces, and finds every literal
// `bus.Event{Subject: ...}` field, resolving its value via resolveExpr. Only
// bus.Event is matched (not bus.Message, which archiver's Kafka-bound DLQ path
// also populates with a Subject field) — every real NATS publish in this
// estate constructs a bus.Event; bus.Message is the Kafka/wire-receive shape.
// Restricting to it is what keeps this guard from flagging archiver's Kafka
// topic name as an unauthorized NATS subject.
func servicePublishedSubjects(t *testing.T, rc *resolveCache, root, svc string) map[string][]string {
	t.Helper()
	dirs := []string{filepath.Join(root, "services", svc)}
	for _, extra := range crossPackagePublishSurfaces[svc] {
		dirs = append(dirs, filepath.Join(root, filepath.FromSlash(extra)))
	}
	return publishedSubjectsIn(t, rc, dirs)
}

// publishedSubjectsIn is servicePublishedSubjects's directory-walking core,
// factored out so nats_jetstream_machinery_test.go's operator-CLI scan (cmd/
// trees, not services/) can reuse the exact same bus.Event{Subject: ...}
// resolution instead of a second walker.
func publishedSubjectsIn(t *testing.T, rc *resolveCache, dirs []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, scanDir := range dirs {
		scanDirForPublishedSubjects(t, rc, scanDir, out)
	}
	return out
}

func scanDirForPublishedSubjects(t *testing.T, rc *resolveCache, serviceDir string, out map[string][]string) {
	t.Helper()
	err := filepath.WalkDir(serviceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		var f *ast.File
		for _, cached := range rc.packageFiles(t, dir) {
			if rc.fset.Position(cached.Pos()).Filename == path {
				f = cached
				break
			}
		}
		if f == nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := cl.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok || pkgIdent.Name != "bus" || sel.Sel.Name != "Event" {
					return true
				}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || key.Name != "Subject" {
						continue
					}
					pos := rc.fset.Position(kv.Pos())
					rel, rerr := filepath.Rel(serviceDir, pos.Filename)
					if rerr != nil {
						rel = pos.Filename
					}
					site := filepath.ToSlash(rel) + ":" + strconv.Itoa(pos.Line)
					for _, subj := range dedupStrings(rc.resolveExpr(t, f, dir, kv.Value, fn, 6)) {
						if !subjectShape.MatchString(subj) || protoTypeRef.MatchString(subj) {
							continue
						}
						out[subj] = append(out[subj], site)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", serviceDir, err)
	}
}

func dedupStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// TestMarketDataPublishGrantCoversItsConfigInjectedWildcard is this file's
// other half for market-data. TestServicePublishesOnlySubjectsItsTenancy
// PermissionsAllow cannot fold BusSink.eventType (services/market-data/
// internal/feed/bussink.go) to a literal: it's an fmt.Sprintf over
// s.assetClass, a field NewBusSink sets from a deploy-time config value —
// genuinely runtime-variable, never a closed set of literals an AST resolver
// could enumerate. So market-data's publish grant is legitimately a wildcard
// (market.>), not a list — and this test is the code-checked assertion that
// the wildcard is honest, not rubber-stamped: it re-derives, from the SAME
// source the resolver could not fold, the two parts that ARE fixed — the
// domain constant and the closed set of variant suffixes — and proves
// tenancy.yaml's market-data grant covers every subject those two fixed parts
// can combine with ANY middle segment to produce. If bussink.go ever adds a
// fourth variant, renames the domain, or the grant is ever narrowed below
// domainMarket+".>", this test — not just the eyeballed comment above the
// grant — is what catches it.
func TestMarketDataPublishGrantCoversItsConfigInjectedWildcard(t *testing.T) {
	root := moduleRoot(t)
	perms := servicePublishPermissions(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	svid := systemAccountSVID("market-data")
	perm, ok := perms[svid]
	if !ok {
		t.Fatalf("tenancy.yaml has no permissions block for %s — has the account been renamed?", svid)
	}

	rc := newResolveCache(root)
	dir := filepath.Join(root, "services", "market-data", "internal", "feed")

	domains := dedupStrings(rc.resolveIdentInPackage(t, dir, "domainMarket", 3))
	if len(domains) != 1 {
		t.Fatalf("resolved %d value(s) for bussink.go's domainMarket constant, want exactly 1 — "+
			"has BusSink.eventType's subject shape changed?", len(domains))
	}
	domain := domains[0]

	variantFile, variantFn := rc.findFuncByName(t, dir, "variant")
	if variantFn == nil {
		t.Fatal("bussink.go's variant() function was not found — has BusSink.eventType been restructured?")
	}
	variants := dedupStrings(rc.foldReturnValues(t, variantFile, dir, variantFn, 0, 3))
	if len(variants) == 0 {
		t.Fatal("resolved zero literal variants from bussink.go's variant() — this test can no longer prove " +
			"the closed set it bounds the wildcard to, and must not silently pass")
	}

	// The middle segment (assetClass) is genuinely unbounded — NewBusSink takes
	// it as a plain config string, not an enum — so any placeholder proves the
	// point; two different ones guard against a grant that happens to allow one
	// specific asset class literally instead of covering the wildcard.
	for _, placeholder := range []string{"equity", "some-configured-asset-class"} {
		for _, v := range variants {
			subject := domain + "." + placeholder + "." + v
			if permDenies(subject, perm) || !covered(subject, perm.allow) {
				t.Fatalf("market-data's real publish shape is %s.<config-injected assetClass>.%s "+
					"(bussink.go's BusSink.eventType) but tenancy.yaml's permissions.publish for %s "+
					"does not cover %q — the wildcard grant no longer bounds what the code can actually produce",
					domain, v, svid, subject)
			}
		}
	}
}
