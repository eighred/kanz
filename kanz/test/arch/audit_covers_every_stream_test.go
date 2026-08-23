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

// THE AUDIT LOG COVERS EVERY PROVISIONED STREAM, OR IT IS NOT THE RECORD IT
// CLAIMS TO BE (#698).
//
// # What went wrong without it
//
// audit's DefaultSubjects was `">"`, with the comment "materializes everything
// — audit completeness over economy". A subscription binds to exactly ONE
// stream: pkg/bus.Subscribe resolved the subject through the JetStream client,
// which ends `return resp.Streams[0], nil` — the first of however many match, in
// the SERVER's order, with no error and no warning. `">"` matches all sixteen
// streams bootstrap-job.yaml provisions, so audit consumed one and materialized
// nothing from the other fifteen.
//
// Every symptom this estate designs against, at once: the subscription
// SUCCEEDED, the pod reported Ready, kanz_bus_consume_total climbed from the one
// stream that did deliver, and "this estate produced no compliance.breach FACTs"
// was indistinguishable from "audit is not subscribed to the stream carrying
// them". audit_log is hash-chained and #665 verifies that chain on a schedule —
// A CHAIN OVER AN INCOMPLETE SET VERIFIES GREEN. Integrity and completeness are
// different properties and only one was checked.
//
// # What this checks, in both directions
//
// pkg/bus now refuses a multi-stream subject, so `">"` cannot come back. What it
// cannot refuse is an enumeration that is INCOMPLETE — a list naming fifteen of
// sixteen streams resolves fine, one per subscription, and loses the sixteenth
// as silently as `">"` lost fifteen. So:
//
//	every provisioned stream is covered by at least one audit subject
//	every audit subject matches at least one provisioned stream
//
// The second arm is what stops the list rotting the other way: an entry for a
// stream that was renamed or removed would sit there looking like coverage.
//
// The streams are read from infra/nats/bootstrap-job.yaml — the thing that
// actually provisions them — so the two cannot drift without this failing.
func TestAuditMaterializesEveryProvisionedStream(t *testing.T) {
	streams := provisionedStreams(t)

	// NON-VACUITY on the parse. A broken scan would report full coverage of
	// nothing, which is the same shape as the defect this guard exists for.
	if len(streams) < 10 {
		t.Fatalf("parsed only %d stream(s) from infra/nats/bootstrap-job.yaml — the scan is broken, "+
			"not the estate, and a coverage check over an empty set passes by finding nothing",
			len(streams))
	}
	subjects := auditDefaultSubjects(t)
	if len(subjects) == 0 {
		t.Fatal("audit's DefaultSubjects is empty — it would subscribe nothing at all")
	}

	// ARM 1: no stream may go unaudited.
	var uncovered []string
	for name, patterns := range streams {
		covered := false
		for _, p := range patterns {
			for _, s := range subjects {
				if subjectsOverlap(s, p) {
					covered = true
				}
			}
		}
		if !covered {
			uncovered = append(uncovered, name+" ("+strings.Join(patterns, ",")+")")
		}
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Errorf("%d provisioned stream(s) that audit subscribes NOTHING from:\n  %s\n\n"+
			"Every FACT in those streams is absent from the compliance record, and nothing reports "+
			"it: the subscriptions audit DOES hold succeed, the pod reports Ready, and its consume "+
			"counter climbs from the streams it does read. audit_log's hash chain verifies green "+
			"over the incomplete set (#665 checks integrity, not completeness).\n\n"+
			"Add the stream's own subject space to auditcfg.DefaultSubjects — not a domain prefix, "+
			"which would match two streams and be refused by pkg/bus.Subscribe.",
			len(uncovered), strings.Join(uncovered, "\n  "))
	}

	// ARM 2: no audit subject may match nothing. A stale entry reads as coverage.
	var orphaned []string
	for _, s := range subjects {
		hit := false
		for _, patterns := range streams {
			for _, p := range patterns {
				if subjectsOverlap(s, p) {
					hit = true
				}
			}
		}
		if !hit {
			orphaned = append(orphaned, s)
		}
	}
	sort.Strings(orphaned)
	if len(orphaned) > 0 {
		t.Errorf("%d audit subject(s) matching NO provisioned stream:\n  %s\n\n"+
			"Either the stream was renamed or removed and this entry is stale — in which case it "+
			"sits in the list looking like coverage — or the subject is misspelled, in which case "+
			"pkg/bus.Subscribe will refuse to start with ErrStreamNotFound. Neither is a state to "+
			"leave the compliance record in.", len(orphaned), strings.Join(orphaned, "\n  "))
	}
}

// ensureStream matches a provisioning line in bootstrap-job.yaml:
//
//	ensure_stream ACCOUNTING    "accounting.>"    168h
//
// The subjects are a comma-separated list inside one quoted argument.
var ensureStream = regexp.MustCompile(`(?m)^\s*ensure_stream\s+([A-Z_]+)\s+"([^"]+)"`)

// provisionedStreams returns stream name -> its subject patterns, read from the
// Job that actually creates them. Read from the manifest rather than restated
// here so the two cannot drift.
func provisionedStreams(t *testing.T) map[string][]string {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "nats", "bootstrap-job.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string][]string{}
	for _, m := range ensureStream.FindAllStringSubmatch(string(body), -1) {
		var subs []string
		for _, s := range strings.Split(m[2], ",") {
			if s = strings.TrimSpace(s); s != "" {
				subs = append(subs, s)
			}
		}
		out[m[1]] = subs
	}
	return out
}

// subjectsOverlap reports whether two NATS subject patterns share any concrete
// subject — the question "would a subscription on a also receive b's traffic".
//
// NATS token rules: `*` matches exactly one token, `>` matches one or more
// trailing tokens. Implemented directly rather than pulled from the client
// because the client exposes no such predicate, and because a guard that got
// this wrong would silently report coverage it does not have.
func subjectsOverlap(a, b string) bool {
	at, bt := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; ; i++ {
		aEnd, bEnd := i >= len(at), i >= len(bt)
		switch {
		case aEnd && bEnd:
			return true
		case aEnd || bEnd:
			// One pattern ran out. They still overlap only if the other's
			// remaining tokens are reachable — which, both having been checked
			// token-by-token above, they are not: `a.b` and `a.b.c` share no
			// concrete subject.
			return false
		}
		x, y := at[i], bt[i]
		if x == ">" || y == ">" {
			return true
		}
		if x != y && x != "*" && y != "*" {
			return false
		}
	}
}

// auditDefaultSubjects reads services/audit's DefaultSubjects from source.
//
// READ FROM SOURCE, NOT IMPORTED, because Go's internal/ rule puts
// services/audit/internal/config out of reach of this package — the same
// constraint that put the principal-header constants in pkg/auth. Parsed on the
// AST rather than grepped: a regex over the file would match the list in the
// doc comment above the declaration, and a guard that reads its own
// documentation is one this repository has already been bitten by.
func auditDefaultSubjects(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "services", "audit", "internal", "config", "config.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if name.Name != "DefaultSubjects" || i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			found = true
			for _, el := range lit.Elts {
				bl, ok := el.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(bl.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", bl.Value, err)
				}
				out = append(out, s)
			}
		}
		return true
	})
	if !found {
		t.Fatalf("no DefaultSubjects declaration found in %s — it was renamed or moved, and this "+
			"guard is reading nothing", path)
	}
	return out
}
