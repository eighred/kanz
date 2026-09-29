package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE ENVIRONMENT IS READ THROUGH ONE HELPER PACKAGE (#641).
//
// # What went wrong without it
//
// AGENTS.md names the shape: "A copied helper is how a fix stops spreading: 17
// services each had their own secret() and 15 were wrong while 2 were right."
// That one was repaired and is guarded. Its siblings in the SAME FILES were
// never touched, and had grown LARGER than the original — 40 copies of envOr in
// five variants, 27 of parseLevel in three, 13 of splitList.
//
// They disagreed, and one disagreement was the 26-vs-1 shape exactly:
//
//   - ONE service of twenty-seven accepted `warning`. The rest matched `warn`
//     only, so LOG_LEVEL=warning fell through to a default of INFO — MORE
//     verbose than asked for, chosen silently. Set estate-wide off one ConfigMap
//     key, it gave the requested level in identity and INFO everywhere else.
//   - FIFTEEN of twenty-seven trimmed whitespace, so `LOG_LEVEL=" debug"` — a
//     YAML block scalar, a mounted file with a trailing newline — was DEBUG in
//     fifteen services and INFO in the other twelve.
//   - envOr split THREE ways on a whitespace-only value: fifteen treated it as
//     unset and used the default, twenty-four passed the raw value to whatever
//     parsed it next.
//
// The issue counted 39 and 26. They were 40 and 27 when the repair began, which
// is the argument in miniature: the copies were still multiplying while the
// issue describing them sat open.
//
// # Why it checks a SHAPE and not the three names
//
// The forty-first copy will not be called envOr — the eighteen Decimal bridges
// #628 retired were called four different things. What identifies this defect is
// a package-level function that takes a KEY AND A DEFAULT and reads the
// environment: that is a wrapper, and a wrapper is what gets copied.
//
// A DIRECT os.Getenv IS NOT THE DEFECT and is not reported. Forty files read the
// environment directly for keys with no default, custom parsing or a required
// value, and a guard that flagged those would need forty exemptions and would be
// switched off — the failure mode a noisy checker always has. What is forbidden
// is re-implementing the wrapper.
func TestTheEnvironmentIsReadThroughOneHelper(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	scanned := 0
	seen := map[string]bool{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "gen" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/env/") {
			return nil // the one home
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		scanned++
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name == nil || fn.Body == nil || fn.Recv != nil {
				continue
			}
			if !readsEnvironment(fn.Body) || !looksLikeAnEnvWrapper(fn) {
				continue
			}
			key := rel + ":" + fn.Name.Name
			seen[key] = true
			if _, exempt := envHelperExempt[key]; exempt {
				continue
			}
			offenders = append(offenders, key+" is an environment-reading wrapper outside internal/env. "+
				"There were eighty of these across three helpers, disagreeing about whitespace, about "+
				"whether `warning` is a log level, and about what a blank value means — and one service "+
				"in twenty-seven behaved differently off the same ConfigMap key. Use env.Or, "+
				"env.ParseLevelOr, env.SplitList, env.Duration, env.Bool, env.Int, or env.Lookup "+
				"where a blank value must be refused.")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// NON-VACUITY. This module has hundreds of non-test Go files, and forty of
	// them read the environment directly. A walk that scanned none would pass
	// however many wrappers had grown back.
	if scanned < 200 {
		t.Fatalf("scanned only %d non-test Go file(s) — the walk is broken and this guard proves "+
			"nothing", scanned)
	}

	for key := range envHelperExempt {
		if !seen[key] {
			offenders = append(offenders, "the exemption for "+key+" is DEAD: no such wrapper exists")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("%d environment wrapper(s) outside internal/env:\n\n  %s",
			len(offenders), strings.Join(offenders, "\n\n  "))
	}
}

// envHelperExempt names environment-reading wrappers allowed outside
// internal/env, with the issue that retires each.
//
// Empty. All eighty were retired and none needed to stay — including the one in
// tools/scaffold's template, which is why a newly scaffolded service now calls
// env.Or rather than being born with copy forty-one.
var envHelperExempt = map[string]string{
	// A SHAPE COLLISION, NOT A COPY (#692). secretFrom(path, envKey string) has
	// two string parameters and returns (string, error), which is the same
	// SIGNATURE as a key-and-default wrapper — but its second parameter is
	// another KEY, not a default, and its first is a path this CLI takes from a
	// flag. It applies no default and owns no convention this package could hold.
	//
	// It IS a near-copy of a different concept: pkg/secret.Read, the one
	// implementation of file-or-env secret reading. The two are not
	// interchangeable as written — Read derives the path from KEY_FILE while this
	// reads a flag — so unifying them is a change to this tool's command line
	// rather than a helper swap, and it is not smuggled into #692's repair.
	"cmd/universe/main.go:secretFrom": "#692 — a (path, key) secret reader, not a key-and-default wrapper; unifying it with pkg/secret.Read is a CLI change",
}

// readsEnvironment reports whether the body calls os.Getenv or os.LookupEnv.
func readsEnvironment(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" &&
			(sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") {
			found = true
			return false
		}
		return true
	})
	return found
}

// looksLikeAnEnvWrapper reports whether fn has the shape that gets copied: it
// takes only strings — a key, usually with a default — and returns a value
// derived from them.
//
// A function that reads a specific key and builds something larger (a whole
// Config, a *tls.Config) is not this: it takes no key and returns a struct, so
// there is nothing to copy into the next service but the key name itself.
func looksLikeAnEnvWrapper(fn *ast.FuncDecl) bool {
	return stringOnlyWrapper(fn) || typedDefaultWrapper(fn)
}

// stringOnlyWrapper is the ORIGINAL shape, unchanged: every parameter a string,
// every result a value kind. Kept exactly as it was so this change can only ADD
// coverage — a rewrite that happened to narrow it would retire guarding nobody
// asked to retire.
func stringOnlyWrapper(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil || fn.Type.Params.NumFields() == 0 {
		return false
	}
	for _, p := range fn.Type.Params.List {
		if !isTypeNamed(p.Type, "string") {
			return false
		}
	}
	if fn.Type.Results == nil || fn.Type.Results.NumFields() == 0 {
		return false
	}
	for _, r := range fn.Type.Results.List {
		if !isEnvValueType(r.Type) {
			return false
		}
	}
	return true
}

// typedDefaultWrapper is the shape that was invisible (#692): a KEY and a TYPED
// DEFAULT.
//
// This function's own doc already said what identifies the defect — "a
// package-level function that takes a KEY AND A DEFAULT and reads the
// environment" — and the result list already allowed Duration and int, so a
// typed RETURN was anticipated. A typed DEFAULT was not, and that is the natural
// spelling: durationOr(key string, def time.Duration). Four such copies were live
// while this guard was green — datamaster's durationOr and boolOr, market-data's
// rollupDur, and two in test/load — and datamaster's swallowed the parse error on
// five schedules, two of which bound a dual-control approval window.
//
// A DEFAULT IS REQUIRED, which is what keeps this from swallowing its neighbours.
// pkg/secret.Read takes a key and returns (string, error) with NO default: it is
// the one implementation of a different concept — KEY plus KEY_FILE — and it has
// its own guard. Requiring a second parameter is the line between "applies a
// default this package should own" and "reads a required value".
func typedDefaultWrapper(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil || fn.Type.Params.NumFields() < 2 {
		return false
	}
	for i, p := range fn.Type.Params.List {
		if i == 0 && !isTypeNamed(p.Type, "string") {
			return false
		}
		if !isEnvValueType(p.Type) {
			return false
		}
	}
	if fn.Type.Results == nil || fn.Type.Results.NumFields() == 0 {
		return false
	}
	values := 0
	for _, r := range fn.Type.Results.List {
		if isTypeNamed(r.Type, "error") {
			continue
		}
		if !isEnvValueType(r.Type) {
			return false
		}
		values++
	}
	return values > 0
}

// isEnvValueType reports whether a type is one an environment wrapper reads a
// value into: the scalar kinds a ConfigMap key can carry.
func isEnvValueType(e ast.Expr) bool {
	for _, name := range []string{"string", "Level", "bool", "Duration", "int"} {
		if isTypeNamed(e, name) {
			return true
		}
	}
	return false
}

// isTypeNamed reports whether e's type expression ends in name, looking through
// a pointer, a slice and a selector so slog.Level and *string both match.
//
// Named distinctly rather than shared with the identical helper in
// one_decimal_float_bridge_test.go: these two guards were written on separate
// branches, and a shared name would have collided at merge for no benefit. If a
// third needs it, that is the moment to promote one.
func isTypeNamed(e ast.Expr, name string) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == name
	case *ast.StarExpr:
		return isTypeNamed(t.X, name)
	case *ast.ArrayType:
		return isTypeNamed(t.Elt, name)
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == name
	}
	return false
}
