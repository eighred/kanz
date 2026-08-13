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

// EVERY REFUSAL THE WEBHOOK PERIMETER CAN RETURN MUST HAVE A STATUS DECIDED FOR
// IT (#416).
//
// webhook-ingest's HTTP surface maps the pipeline's typed errors to status codes
// in writePipelineError, which ends in a default arm answering 502 Bad Gateway —
// "a real backend fault". Any sentinel nobody wrote a case for lands there.
//
// TWO DID, AND BOTH WERE DECISIONS ABOUT THE CALLER'S OWN INPUT:
//
//	ErrStaleSignal    the alert carried no `ts`, or one too old to act on
//	ErrNoAllocation   the alert named a fund with no venue allocation
//
// Each cost the same three things. It BLAMED US — a sender's clock or a sender's
// fund_id read as this platform being broken, and 502 is what an operator pages
// on. It INVITED A RETRY THAT COULD NEVER SUCCEED — senders and proxies retry
// 5xx, and both conditions are permanent (Alloc is static config read at startup;
// an alert only gets older). And because neither was ErrBadRequest, `decided`
// RELEASED THE NONCE instead of burning it, so each redelivery re-entered the
// whole pipeline rather than collapsing to ErrReplayed. The alerts that retried
// hardest were the ones guaranteed to fail.
//
// WHY A GUARD AND NOT TWO MORE UNIT TESTS. Both sentinels already had a test —
// TestUnmappedFundDenied and the freshness tests — and both passed throughout.
// They asserted the SENTINEL that came back, never the STATUS the caller would
// receive. That is the asymmetry: a per-sentinel test only ever covers the
// sentinels somebody thought about, and these two are precisely the ones nobody
// did. A new sentinel added tomorrow gets the 502 by default, silently, and its
// author gets no signal at all.
//
// WHAT IT CHECKS: every exported Err* declared in the ingest package is NAMED in
// one of the two places a status decision is made — writePipelineError (which
// picks the code) or mapTranslateErr (which re-maps a translator sentinel onto a
// pipeline one that writePipelineError already handles).
//
// WHAT IT CANNOT CHECK: that the status chosen is the RIGHT one. Naming
// ErrStaleSignal in a case that answers 418 satisfies this guard. The behavioural
// tests beside the code carry that half — TestPerimeter_AStaleAlertIs400AndBurnsItsNonce
// and TestPerimeter_AnUnmappedFundIs400AndBurnsItsNonce. What this forces is that
// a status was CHOSEN, in a diff a reviewer can see, rather than inherited from a
// default nobody looked at.

// ingestStatusSentinelPkg is the package whose exported sentinels must be mapped.
const ingestStatusSentinelPkg = "services/webhook-ingest/internal/ingest"

// ingestStatusDeciders are the functions where a status decision counts as made.
// Module-relative file, then the function within it.
var ingestStatusDeciders = map[string]string{
	"services/webhook-ingest/internal/server/server.go":   "writePipelineError",
	"services/webhook-ingest/internal/ingest/pipeline.go": "mapTranslateErr",
}

// ingestStatusExempt maps a sentinel to the reason it needs no status, and the
// issue that retires the entry.
//
// KEEP THIS SHORT AND ARGUED. Every entry is a refusal a caller can provoke and
// receive no considered answer to. "It never happens" is not a reason — that is
// what was believed about both sentinels this guard was written for.
var ingestStatusExempt = map[string]string{
	// Returned by NewPipeline, never by Process: a fund whose venue weights are not
	// a split is a COMPOSITION-ROOT failure that stops the process from starting, so
	// it never reaches an HTTP response. Verified: the only construction site is
	// NewPipeline's ValidateAllocation call.
	"ErrBadAllocation": "startup-only — returned by NewPipeline, never by Process, so no request can receive it",
}

func TestEveryIngestSentinelHasAStatusDecided(t *testing.T) {
	root := moduleRoot(t)

	sentinels := exportedErrVars(t, filepath.Join(root, filepath.FromSlash(ingestStatusSentinelPkg)))
	// NON-VACUITY: the package declares eight today (six of its own plus the two
	// aliases of translator sentinels). Coming back near-empty means the declaration
	// shape moved and this guard is asserting nothing.
	if len(sentinels) < 6 {
		t.Fatalf("found %d exported Err* in %s (%v) — expected at least 6. The declaration "+
			"shape moved and this guard is asserting nothing", len(sentinels), ingestStatusSentinelPkg, sentinels)
	}

	decided := map[string]string{} // sentinel name -> where it was decided
	for rel, fn := range ingestStatusDeciders {
		named := identsNamedIn(t, filepath.Join(root, filepath.FromSlash(rel)), fn)
		if len(named) == 0 {
			t.Fatalf("%s: found no identifiers inside %s — the function was renamed or moved, "+
				"and this guard can no longer see where status is decided", rel, fn)
		}
		for name := range named {
			if _, already := decided[name]; !already {
				decided[name] = rel + ":" + fn
			}
		}
	}

	for _, name := range sentinels {
		if reason, ok := ingestStatusExempt[name]; ok {
			t.Logf("%s: exempt — %s", name, reason)
			continue
		}
		if where, ok := decided[name]; ok {
			t.Logf("%s: decided in %s", name, where)
			continue
		}
		t.Errorf("%s is an exported refusal of package ingest and NO status is decided for it.\n"+
			"It falls to writePipelineError's default arm, which answers 502 Bad Gateway — the "+
			"status reserved for a fault of ours. That blames this platform for the caller's "+
			"input, and 5xx is RETRYABLE, so a permanent refusal becomes a retry storm. Add a "+
			"case to writePipelineError, or re-map it in mapTranslateErr onto one that already "+
			"has a case. If it truly cannot reach a response, add it to ingestStatusExempt with "+
			"the argument.", name)
	}

	// DEAD-ENTRY ARM: an exemption for a sentinel that no longer exists has
	// outlived its repair, and would wave through a future sentinel that reused the
	// name.
	have := map[string]bool{}
	for _, n := range sentinels {
		have[n] = true
	}
	for name, reason := range ingestStatusExempt {
		if !have[name] {
			t.Errorf("exemption for %q (%s) matches no exported Err* in %s — delete it",
				name, reason, ingestStatusSentinelPkg)
		}
	}
}

// exportedErrVars returns the exported Err* var names declared in the non-test
// Go files of dir, sorted.
func exportedErrVars(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, gf := range goFilesUnder(t, dir) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, gf.rel, gf.body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", gf.rel, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range vs.Names {
					if name.IsExported() && strings.HasPrefix(name.Name, "Err") {
						out = append(out, name.Name)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// identsNamedIn returns the set of identifier names appearing anywhere inside the
// function fn declared in path. Names, not resolved types: a sentinel is written
// either bare or package-qualified depending on which side of the boundary the
// decider sits on, and both spellings are the same decision.
func identsNamedIn(t *testing.T, path, fn string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				out[id.Name] = true
			}
			return true
		})
	}
	return out
}
