package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// AN IN-MEMORY STORE MUST REFUSE WHAT ITS DURABLE SIBLING REFUSES (#818).
//
// # What this is protecting
//
// position.Postgres.Apply claims `position_fills` with an INSERT ... ON CONFLICT
// DO NOTHING and reads RowsAffected before it folds. position.Book.Apply folded
// unconditionally. Both satisfy position.Store, whose Apply contract says a fill
// already folded "by a redelivery, or by another pod — is not counted again".
//
// Book is only selected when OMS_DATABASE_URL is absent, which already mandates
// a single replica, so the divergence was not itself a production double-count.
// What it broke is the CERTIFICATION: every service-level test of the projector
// runs against Book, so a redelivery test written to prove exactly-once folding
// PASSED against a store that doubled the position. Two of the package's own
// tests stamped one fill_id on two trades and were green.
//
// This is the SECOND time the divergence has appeared in the OMS. order.Postgres
// claims `order_fills` and order.MemoryStore carries `appliedFills` because the
// in-memory seam had the same gap (#782), and #796 fixed the same shape again in
// the proposal stores. A third occurrence is not a coincidence; it is a class.
//
// # What a failure here means
//
// Not "a map is missing". It means the fake every service-level test in that
// package runs against will ACCEPT a duplicate the database REFUSES, so a green
// suite over that seam certifies exactly-once behaviour the platform does not
// have. On the position path the artifact of that is an ABSOLUTE PositionState,
// published on a compacted subject, that the risk engine, the compliance monitor
// and the OMS's own pre-trade gate all admit orders against.
//
// # Everything here is DERIVED, not listed
//
// The population, the pairing and the key are all read out of the source:
//
//   - the CLAIM comes from the SQL itself — a string literal (or a const the
//     method names) matching INSERT ... ON CONFLICT ... DO NOTHING, on a type
//     that also reads RowsAffected, because an insert whose verdict nobody reads
//     is an idempotent write and not an admission gate;
//   - the KEY comes from that statement's own conflict target, falling back to
//     its inserted column list, with tenant_id dropped — `fill_id` for both OMS
//     stores, `order_id` for the proposal stores;
//   - the SIBLING comes from method-set matching against the interfaces the
//     package declares, so a new backend is picked up the day it compiles.
//
// A hand-written list of the pairs is the artifact that keeps rotting in this
// tree, and it would have to be edited by the same change that introduces the
// next divergence.
//
// # Why it reads the AST with comments detached
//
// Three guards here have already passed while asserting nothing, because a regex
// over raw source matched their own explanatory prose. Every "ON CONFLICT DO
// NOTHING" in the paragraphs above is invisible to the code below: files are
// parsed with mode 0, and the claim is matched only in string LITERALS.

// claimRootDirs are the trees that hold domain stores. cmd/ is composition, and
// test/ is the guards themselves.
var claimRootDirs = []string{"services", "internal", "pkg"}

// claimSQL matches the admission-gate insert. The conflict target is optional:
// position_fills has no explicit one, and its inserted column list is the key.
var claimSQL = regexp.MustCompile(`(?is)insert\s+into\s+([a-z_][a-z0-9_]*)\s*\(([^)]*)\)(.*?)on\s+conflict\s*(\([^)]*\))?\s*do\s+nothing`)

// claimSQLNoCols is the same statement written without a column list.
var claimSQLNoCols = regexp.MustCompile(`(?is)insert\s+into\s+([a-z_][a-z0-9_]*)\s+(.*?)on\s+conflict\s*(\([^)]*\))?\s*do\s+nothing`)

// memorySiblingExempt names a non-durable implementation whose refusal this
// guard cannot see, keyed "pkgdir.Type" and valued with what proves it instead.
//
// DEFAULT-DENY. An entry is NOT "this backend is only used in tests" — that is
// precisely the argument that let #818 sit for as long as it did, and the test
// seam is the thing being protected rather than the thing being excused. An
// entry is only admissible when the refusal is proven somewhere a reader can
// run.
//
// THE ONE ENTRY IS A LIMIT OF THE GUARD, NOT OF THE CODE. The key match below
// works because both OMS fill stores index on something spelled like the SQL
// column; a store that delegates to a GENERIC core indexes on a type parameter's
// id, and no column name survives that. Following the field instead was tried
// and rejected: the first version of this guard passed MemoryProposals by
// matching the lease map inside the outbox it also holds — a green result for a
// property nobody meant to assert, which is the failure mode AGENTS.md names.
var memorySiblingExempt = map[string]string{
	"services/oms/internal/order.MemoryProposals": "holds no map of its own: the duplicate is refused by " +
		"internal/dualcontrol/proposalstore.Memory[T].Insert, which returns ErrExists for a held id under its " +
		"own lock. Parity with PostgresProposals is proven EXECUTABLY rather than here — both backends run the " +
		"shared contract suite in internal/dualcontrol/proposalstore/proposalstoretest, whose case \"a second " +
		"proposal for one id is refused and the first survives\" is this exact property — run for both backends by " +
		"TestMemoryProposalsHonourTheSharedContract and TestPostgresProposalsHonourTheSharedContract in " +
		"services/oms/internal/order/proposals_contract_test.go. The pair's other half is guarded by " +
		"TestAClaimAndItsExpiryExcludeEachOther (#796).",
}

// claimedKeyTerm is the normalized form a claim key must appear as inside the
// in-memory sibling's map index: "fill_id" ⇒ "fillid", which is a substring of
// both `fillID` and `fill.GetFillId()`.
func claimedKeyTerm(col string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(col), "_", ""))
}

type claimingType struct {
	pkgDir string
	typ    string
	table  string
	keys   []string // normalized, tenant dropped
}

func TestEveryInMemorySiblingOfAClaimingStoreClaimsTheSameKey(t *testing.T) {
	root := moduleRoot(t)

	// pkgDir -> parsed non-test files.
	pkgs := map[string][]*ast.File{}
	fset := token.NewFileSet()
	for _, sub := range claimRootDirs {
		base := filepath.Join(root, sub)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			// Mode 0: comments are not attached, so this guard cannot match its
			// own prose or anybody else's.
			f, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				return perr
			}
			dir := path.Dir(relPath(root, p))
			pkgs[dir] = append(pkgs[dir], f)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	if len(pkgs) == 0 {
		t.Fatal("parsed no packages — the guard would pass by finding nothing")
	}

	index := map[string]*pkgFacts{}
	for dir, files := range pkgs {
		index[dir] = indexPackage(files)
	}

	var claimers []claimingType
	// "pkgdir.Type" of every non-claiming sibling that fails, with the reason.
	var failures []string
	// Every sibling this run actually paired, which is what makes an exemption
	// falsifiable rather than merely spelled.
	reached := map[string]bool{}
	checked := 0

	for _, dir := range sortedKeys(pkgs) {
		p := index[dir]

		// Which types in this package make an admission claim, and on what key.
		claimingHere := map[string]claimingType{}
		for typ, decls := range p.methodsOf {
			table, keys, found := claimIn(decls, p.sqlConsts)
			if !found || !readsRowsAffected(decls) {
				continue
			}
			c := claimingType{pkgDir: dir, typ: typ, table: table, keys: keys}
			claimingHere[typ] = c
			claimers = append(claimers, c)
		}
		if len(claimingHere) == 0 {
			continue
		}

		for _, claimer := range sortedKeys(claimingHere) {
			c := claimingHere[claimer]
			if len(c.keys) == 0 {
				failures = append(failures, dir+"."+claimer+
					": its claim on "+c.table+" has no derivable key — the guard cannot say what a sibling would have to consult")
				continue
			}
			for _, sibling := range siblingsOf(claimer, p) {
				if _, alsoClaims := claimingHere[sibling]; alsoClaims {
					continue // two durable backends, not a fake
				}
				name := dir + "." + sibling
				checked++
				reached[name] = true
				if _, ok := memorySiblingExempt[name]; ok {
					continue
				}
				if consultsKey(p.methodsOf[sibling], c.keys) {
					continue
				}
				failures = append(failures, name+
					": implements the same interface as "+claimer+", which claims "+c.table+
					" on ("+strings.Join(c.keys, ", ")+") and reads the verdict — but no method of "+sibling+
					" both records and consults a map keyed on that. Every test running against "+sibling+
					" therefore certifies that a duplicate is accepted, which is behaviour the database does not have")
			}
		}
	}

	// A GUARD THAT FINDS NOTHING TO CHECK IS A GUARD THAT PASSES FOR FREE. Both
	// arms are load-bearing: no claimers means the SQL matcher stopped matching
	// (a rewritten statement, a moved package); no siblings means the pairing
	// stopped resolving.
	if len(claimers) == 0 {
		t.Fatal("found no store that claims an idempotency key — the SQL matcher no longer matches anything, so this guard is asserting nothing")
	}
	if checked == 0 {
		t.Fatalf("found %d claiming store(s) but paired none with a sibling implementation — the interface matching no longer resolves, so this guard is asserting nothing", len(claimers))
	}

	// A DEAD EXEMPTION IS WORSE THAN NO EXEMPTION: it reads as a decision
	// somebody made about code that is no longer there. The test is not "does
	// the package exist" but "did this run actually REACH that type as a sibling"
	// — an entry for a type the pairing no longer resolves is excusing nothing
	// and would go on excusing nothing silently.
	for name, why := range memorySiblingExempt {
		if !reached[name] {
			t.Errorf("memorySiblingExempt names %q, which this run never reached as a sibling of a claiming store. "+
				"The type is gone, or the pairing stopped resolving, or the entry was never real. Delete it.\n      reason on file: %s", name, why)
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		t.Fatalf("%d in-memory store(s) accept a duplicate their durable sibling refuses:\n\n  %s\n\n"+
			"Give the type the claimed-key set its sibling's table is (order.MemoryStore.appliedFills and "+
			"position.Book.appliedFills are the shape), held under the same lock as the rest of its state — "+
			"or add it to memorySiblingExempt with the reason a duplicate cannot reach it.",
			len(failures), strings.Join(failures, "\n  "))
	}
	t.Logf("checked %d sibling implementation(s) against %d claiming store(s)", checked, len(claimers))
}

// pkgFacts is one package, reduced to what this guard reasons over.
type pkgFacts struct {
	methodsOf map[string][]*ast.FuncDecl // receiver type -> its methods
	ifaces    map[string][]string        // interface name -> method names
	sqlConsts map[string]string          // package-level string const/var -> value
}

func indexPackage(files []*ast.File) *pkgFacts {
	p := &pkgFacts{
		methodsOf: map[string][]*ast.FuncDecl{},
		ifaces:    map[string][]string{},
		sqlConsts: map[string]string{},
	}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if recv := recvTypeName(d); recv != "" {
					p.methodsOf[recv] = append(p.methodsOf[recv], d)
				}
			case *ast.GenDecl:
				switch d.Tok {
				// A claim spelled as `const fooSQL = ...` is attributed to the
				// method that names it, which is how order.claimFill is written.
				case token.CONST, token.VAR:
					for _, spec := range d.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range vs.Names {
							if i < len(vs.Values) {
								if s, ok := stringLit(vs.Values[i]); ok {
									p.sqlConsts[name.Name] = s
								}
							}
						}
					}
				case token.TYPE:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						switch t := ts.Type.(type) {
						case *ast.InterfaceType:
							var names []string
							for _, m := range t.Methods.List {
								if _, isFunc := m.Type.(*ast.FuncType); !isFunc {
									continue
								}
								for _, n := range m.Names {
									names = append(names, n.Name)
								}
							}
							if len(names) > 0 {
								p.ifaces[ts.Name.Name] = names
							}
						}
					}
				}
			}
		}
	}
	return p
}

// recvTypeName is the package's receiverType with GENERIC receivers unwrapped —
// `func (m *Memory[T]) Insert` binds to "Memory". Without that the shared
// dual-control core has no methods at all as far as this guard can see, and a
// store delegating to it reads as a store delegating to nothing.
func recvTypeName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) != 1 {
		return ""
	}
	e := d.Recv.List[0].Type
	for {
		switch v := e.(type) {
		case *ast.StarExpr:
			e = v.X
		case *ast.IndexExpr:
			e = v.X
		case *ast.IndexListExpr:
			e = v.X
		case *ast.Ident:
			return v.Name
		default:
			return ""
		}
	}
}

// claimIn reports the table and key columns of the first admission-gate insert
// among these methods — in a string literal, or in a package-level SQL constant
// the method names.
func claimIn(decls []*ast.FuncDecl, sqlConsts map[string]string) (table string, keys []string, found bool) {
	for _, d := range decls {
		if d.Body == nil {
			continue
		}
		ast.Inspect(d.Body, func(n ast.Node) bool {
			if found {
				return false
			}
			switch v := n.(type) {
			case *ast.BasicLit:
				if s, ok := stringLit(v); ok {
					if tb, ks, ok := parseClaim(s); ok {
						table, keys, found = tb, ks, true
					}
				}
			case *ast.Ident:
				if s, ok := sqlConsts[v.Name]; ok {
					if tb, ks, ok := parseClaim(s); ok {
						table, keys, found = tb, ks, true
					}
				}
			}
			return true
		})
		if found {
			return table, keys, true
		}
	}
	return "", nil, false
}

// parseClaim reads the key out of the statement's OWN conflict target, falling
// back to its inserted column list, and drops tenant_id — which every table here
// carries and which identifies nothing on its own.
func parseClaim(sql string) (string, []string, bool) {
	m := claimSQL.FindStringSubmatch(sql)
	var table, cols, target string
	switch {
	case m != nil:
		table, cols, target = m[1], m[2], m[4]
	default:
		m = claimSQLNoCols.FindStringSubmatch(sql)
		if m == nil {
			return "", nil, false
		}
		table, target = m[1], m[3]
	}
	src := cols
	if t := strings.Trim(target, "()"); strings.TrimSpace(t) != "" {
		src = t
	}
	var keys []string
	for _, c := range strings.Split(src, ",") {
		k := claimedKeyTerm(c)
		if k == "" || k == "tenantid" {
			continue
		}
		keys = append(keys, k)
	}
	return table, keys, true
}

// readsRowsAffected separates an admission GATE from an idempotent write. An
// INSERT whose verdict nobody reads decides nothing, and its in-memory sibling
// has nothing to mirror.
func readsRowsAffected(decls []*ast.FuncDecl) bool {
	for _, d := range decls {
		if d.Body == nil {
			continue
		}
		hit := false
		ast.Inspect(d.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RowsAffected" {
				hit = true
			}
			return true
		})
		if hit {
			return true
		}
	}
	return false
}

// siblingsOf returns the other types in the package that satisfy — by method
// NAME set — an interface the claiming type also satisfies. Names rather than
// signatures: the compiler already checks the signatures, and a same-named
// method set on one interface in one package is the pairing.
func siblingsOf(claimer string, p *pkgFacts) []string {
	has := func(typ string, want []string) bool {
		got := map[string]bool{}
		for _, d := range p.methodsOf[typ] {
			got[d.Name.Name] = true
		}
		for _, w := range want {
			if !got[w] {
				return false
			}
		}
		return true
	}
	out := map[string]bool{}
	for _, want := range p.ifaces {
		if !has(claimer, want) {
			continue
		}
		for typ := range p.methodsOf {
			if typ != claimer && has(typ, want) {
				out[typ] = true
			}
		}
	}
	var list []string
	for typ := range out {
		list = append(list, typ)
	}
	sort.Strings(list)
	return list
}

// consultsKey reports whether some map field of this type is BOTH written and
// read at an index derived from the claimed key. Both halves matter: a set
// nobody consults refuses nothing, and a consultation of a set nobody fills
// refuses everything.
func consultsKey(decls []*ast.FuncDecl, keys []string) bool {
	written, read := map[string]bool{}, map[string]bool{}
	for _, d := range decls {
		if d.Body == nil {
			continue
		}
		// AN ASSIGNMENT'S TARGET IS NOT A CONSULTATION, and forgetting that is how
		// the first draft of this guard passed a store whose claim had been
		// deleted: ast.Inspect reaches `b.set[k]` on the LEFT of an assignment as
		// an IndexExpr like any other, so recording the fill and never reading it
		// satisfied both halves at once. AssignStmt is visited before its own
		// children, so marking the targets here excludes them below.
		targets := map[ast.Node]bool{}
		ast.Inspect(d.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok {
				for _, lhs := range as.Lhs {
					if field, ok := indexedField(lhs, keys); ok {
						written[field] = true
						targets[lhs] = true
					}
				}
			}
			if ix, ok := n.(*ast.IndexExpr); ok && !targets[n] {
				if field, ok := indexedField(ix, keys); ok {
					read[field] = true
				}
			}
			return true
		})
	}
	for f := range written {
		if read[f] {
			return true
		}
	}
	return false
}

// indexedField matches `x.field[<expr mentioning a claimed key>]` and returns
// the field name.
func indexedField(e ast.Expr, keys []string) (string, bool) {
	ix, ok := e.(*ast.IndexExpr)
	if !ok {
		return "", false
	}
	sel, ok := ix.X.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if _, ok := sel.X.(*ast.Ident); !ok {
		return "", false
	}
	idx := claimedKeyTerm(flatIdentText(ix.Index))
	for _, k := range keys {
		if strings.Contains(idx, k) {
			return sel.Sel.Name, true
		}
	}
	return "", false
}

// flatIdentText runs an expression's identifiers together, so `fillID` and
// `fill.GetFillId()` both normalize to something containing "fillid". The
// package's exprText renders through go/printer and needs a FileSet; this one
// deliberately drops the punctuation, which is the whole point.
func flatIdentText(e ast.Expr) string {
	var b strings.Builder
	ast.Inspect(e, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			b.WriteString(v.Name)
		case *ast.BasicLit:
			b.WriteString(v.Value)
		}
		return true
	})
	return b.String()
}
