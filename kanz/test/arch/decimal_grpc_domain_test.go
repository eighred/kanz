package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A gRPC METHOD THAT ACCEPTS A DECIMAL FROM ITS CALLER MUST BOUND ITS DOMAIN (#246).
//
// This is the sibling of TestBusPayloadDecodersBoundTheDecimalDomain, and it
// exists because that guard could not have caught what it was meant to catch.
//
// #95 bounded every BUS ingress and left a guard behind. That guard keys on
// proto.Unmarshal(payload, …) — "this file decodes a FACT off the bus". A gRPC
// method argument is decoded by the transport before the handler runs, so a
// server method is outside the rule BY CONSTRUCTION, not exempted from it; the
// bus guard's own comment at :44-57 names the residue it cannot see. The
// follow-up that enumerated that residue, #185, listed three sites by hand and
// grpcsrv was not among them.
//
// The result: services/risk-engine/internal/grpcsrv passed a caller-supplied
// ScenarioShock.Pct straight into the risk compute layer. Decimal.exponent is a
// plain int32 on the wire, and the compute layer expanded 10^abs(exponent) in a
// loop — {coefficient: 1, exponent: -2000000000} meant two billion iterations
// PER POSITION, PER SHOCK. The engine stops answering and still reports healthy.
// Two hand sweeps had already been over this platform looking for exactly that.
//
// So the question is asked STRUCTURALLY, and from the SCHEMA rather than from
// the Go: for every rpc whose request message transitively contains a
// common.v1.Decimal, the file implementing that method must perform a domain
// check. Adding a Decimal to a request message therefore pulls its handler into
// this guard automatically — nobody has to remember to come back here.
//
// WHAT THIS DOES NOT PROVE, stated plainly because a guard trusted past its
// reach is worse than none:
//
//   - It is an AST PAIRING check, like its sibling. It confirms a domain check
//     exists in a file that needs one, not that the check sits on the path the
//     Decimal takes. A file can satisfy it and still hand an unchecked value on
//     down another branch.
//   - It sees the request message, not the response. A Decimal a service RECEIVES
//     in a gRPC RESPONSE (a client call, not a server method) is a different
//     ingress and is not covered here.
//   - It matches a server by the generated Unimplemented<Service>Server embed. A
//     hand-written implementation of the interface that does not embed it is
//     invisible — that embed is required for forward compatibility, so it is
//     present everywhere today, but it is an assumption, not a proof.

// pendingGRPCDomainChecks are gRPC server files that accept a Decimal-carrying
// request without a domain check.
//
// EMPTY, and that is the point. The stale-exemption arm below FAILS the build
// when an entry no longer matches, so this cannot quietly accumulate excuses. A
// new entry needs a written reason naming the issue that retires it.
var pendingGRPCDomainChecks = map[string]string{}

func TestGRPCServersBoundTheDecimalDomain(t *testing.T) {
	root := moduleRoot(t)
	schema := loadProtoSchema(t, filepath.Join(filepath.Dir(root), "kanz-schemas", "proto"))

	// NON-VACUITY, part one: the schema scanner.
	if len(schema.fields) == 0 {
		t.Fatal("parsed no proto messages — the schema scanner is broken, and every gRPC server would pass unexamined")
	}
	if !schema.containsDecimal("common.v1.Money") {
		t.Fatal("common.v1.Money does not resolve to a common.v1.Decimal — the proto type resolver is broken, " +
			"so no request would ever look like it carries a Decimal")
	}

	// decimalRPCs is service.Method → the request message that carries the Decimal.
	decimalRPCs := map[string]string{}
	for _, r := range schema.rpcs {
		if schema.containsDecimal(r.request) {
			decimalRPCs[r.service+"."+r.method] = r.request
		}
	}
	// NON-VACUITY, part two: the anchor. EvaluateScenario is the method #246 was
	// filed against; if the scan stops seeing it, the guard has gone quiet.
	if _, ok := decimalRPCs["RiskQueryService.EvaluateScenario"]; !ok {
		t.Fatalf("RiskQueryService.EvaluateScenario is not recognised as carrying a Decimal — it does "+
			"(ScenarioShock.pct), so the rpc/message scan has stopped matching. Found %d Decimal-carrying rpcs.",
			len(decimalRPCs))
	}

	var offenders []string
	servers, guarded := 0, 0
	seenPending := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || n == "vendor" || n == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}

		// Which service does this file serve, and on which receiver types? The
		// generated embed is the only declaration that says so without linking
		// the whole module.
		serviceOf := map[string]string{} // receiver type name → proto service name
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				for _, fld := range st.Fields.List {
					if len(fld.Names) != 0 { // an embed has no field name
						continue
					}
					sel, ok := fld.Type.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if svc, ok := generatedServiceEmbed(sel.Sel.Name); ok {
						serviceOf[ts.Name.Name] = svc
					}
				}
			}
		}
		if len(serviceOf) == 0 {
			return nil
		}
		servers++

		var needs []string
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			svc, ok := serviceOf[receiverTypeName(fn.Recv.List[0].Type)]
			if !ok {
				continue
			}
			if req, carries := decimalRPCs[svc+"."+fn.Name.Name]; carries {
				needs = append(needs, fmt.Sprintf("%s(%s)", fn.Name.Name, req))
			}
		}
		if len(needs) == 0 {
			return nil
		}

		checksDomain := false
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "dec" && domainCheckers[sel.Sel.Name] {
				checksDomain = true
			}
			return true
		})

		rel := filepath.ToSlash(mustRel(root, path))
		if checksDomain {
			guarded++
			return nil
		}
		if _, pending := pendingGRPCDomainChecks[rel]; pending {
			seenPending[rel] = true
			return nil
		}
		sort.Strings(needs)
		offenders = append(offenders, fmt.Sprintf("%s: %s", rel, strings.Join(needs, ", ")))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// NON-VACUITY, part three: the Go side. Two ways for this conjunction to fail
	// open, and each has to be closed separately.
	if servers == 0 {
		t.Fatal("found no gRPC servers — the Unimplemented<Service>Server embed detection has stopped " +
			"matching, so every handler would pass this guard unexamined")
	}
	if guarded == 0 && len(offenders) == 0 && len(pendingGRPCDomainChecks) == 0 {
		t.Fatal("no gRPC server implements a Decimal-carrying method — either the schema no longer has one " +
			"(it does; the anchor above just passed), or the method matching is broken and this guard can " +
			"now only ever pass")
	}

	for file := range pendingGRPCDomainChecks {
		if !seenPending[file] {
			offenders = append(offenders, file+
				": declared in pendingGRPCDomainChecks but no longer serves an unchecked Decimal-carrying "+
				"request — stale exemption, remove it")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("gRPC server methods accept a caller-supplied Decimal with NO domain check:\n  %s\n\n"+
			"Decimal.exponent is an unvalidated wire field and the arithmetic behind these handlers "+
			"expands 10^abs(exponent). A request carrying {1, -2000000000} does not compute a wrong "+
			"number — it does not return, and the service keeps reporting healthy while it stops "+
			"answering.\n\n"+
			"Call dec.InDomainDeep on the whole request message at the top of the method and return "+
			"codes.InvalidArgument. InDomainDeep walks EVERY Decimal in the message, so a field added to "+
			"the schema later is covered without anyone returning here. Never substitute zero for a "+
			"refused value: a zero price or size reads as flat and is dropped from downstream checks.",
			strings.Join(offenders, "\n  "))
	}
}

// generatedServiceEmbed reports the proto service name behind a protoc-gen-go-grpc
// forward-compatibility embed, e.g. UnimplementedRiskQueryServiceServer →
// RiskQueryService.
func generatedServiceEmbed(name string) (string, bool) {
	const prefix, suffix = "Unimplemented", "Server"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return "", false
	}
	svc := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if svc == "" {
		return "", false
	}
	return svc, true
}

func receiverTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// --- the schema side ----------------------------------------------------

// protoSchema is enough of the .proto tree to answer one question: does this
// request message, anywhere inside it, carry a common.v1.Decimal?
//
// It is parsed from the SCHEMA rather than read off the generated Go because the
// schema is where a new Decimal field is added. A guard driven by the generated
// code would only notice after somebody regenerated and looked.
type protoSchema struct {
	fields map[string][]string // full message name → field type names, as written
	memo   map[string]bool
	rpcs   []protoServiceRPC
}

type protoServiceRPC struct{ service, method, request string }

const decimalFullName = "common.v1.Decimal"

var (
	protoPackageRe = regexp.MustCompile(`^package\s+([A-Za-z0-9_.]+)\s*;`)
	protoMessageRe = regexp.MustCompile(`^message\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)
	protoScopeRe   = regexp.MustCompile(`^(?:enum|oneof)\s+[A-Za-z_][A-Za-z0-9_]*\s*\{`)
	protoServiceRe = regexp.MustCompile(`^service\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)
	protoRPCRe     = regexp.MustCompile(`^rpc\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*(?:stream\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*\)`)
	protoFieldRe   = regexp.MustCompile(`^(?:repeated\s+|optional\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s+[a-z_][A-Za-z0-9_]*\s*=\s*\d+`)
	protoMapRe     = regexp.MustCompile(`^map\s*<\s*[A-Za-z0-9_.]+\s*,\s*([A-Za-z_][A-Za-z0-9_.]*)\s*>\s+[a-z_][A-Za-z0-9_]*\s*=\s*\d+`)
)

func loadProtoSchema(t *testing.T, root string) *protoSchema {
	t.Helper()
	s := &protoSchema{fields: map[string][]string{}, memo: map[string]bool{}}
	// raw holds each field type exactly as written plus the scope it was written
	// in, so it can be resolved against the message table once the whole tree is
	// known — a field can name a message declared in a file parsed later.
	type rawField struct{ owner, scope, typ string }
	var raws []rawField

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		body, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		var pkg, service string
		var stack []string // enclosing message names, innermost last
		var opaque []bool  // parallel: true for enum/oneof/service/option scopes
		for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
			ln := strings.TrimSpace(line)
			if ln == "" || strings.HasPrefix(ln, "//") || strings.HasPrefix(ln, "*") || strings.HasPrefix(ln, "/*") {
				continue
			}
			if m := protoPackageRe.FindStringSubmatch(ln); m != nil {
				pkg = m[1]
				continue
			}
			if m := protoMessageRe.FindStringSubmatch(ln); m != nil {
				full := m[1]
				if len(stack) > 0 {
					full = stack[len(stack)-1] + "." + m[1]
				} else if pkg != "" {
					full = pkg + "." + m[1]
				}
				s.fields[full] = nil
				// `message X {}` on one line opens and closes in the same breath.
				// Pushing it unconditionally left the scope stack permanently one
				// level deep, and every message declared afterwards was recorded
				// under a nested name nothing looked up — venue.v1.ExecuteRequest
				// vanished behind an empty DescribeRequest and the guard passed on
				// a server it should have failed. The unbalanced-stack check at the
				// end of this walk is what makes that class of slip loud.
				if !closesOnSameLine(ln) {
					stack = append(stack, full)
					opaque = append(opaque, false)
				}
				continue
			}
			if m := protoServiceRe.FindStringSubmatch(ln); m != nil {
				if closesOnSameLine(ln) {
					continue
				}
				service = m[1]
				stack = append(stack, "")
				opaque = append(opaque, true)
				continue
			}
			if protoScopeRe.MatchString(ln) && closesOnSameLine(ln) {
				continue
			}
			if protoScopeRe.MatchString(ln) {
				// A oneof's fields belong to the enclosing message, so the scope is
				// pushed as opaque-but-transparent-for-fields: repeat the current
				// owner rather than adding a level.
				owner := ""
				if len(stack) > 0 {
					owner = stack[len(stack)-1]
				}
				isOneof := strings.HasPrefix(ln, "oneof")
				stack = append(stack, owner)
				opaque = append(opaque, !isOneof)
				continue
			}
			if strings.HasPrefix(ln, "}") {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
					opaque = opaque[:len(opaque)-1]
				}
				if len(stack) == 0 {
					service = ""
				}
				continue
			}
			if service != "" {
				if m := protoRPCRe.FindStringSubmatch(ln); m != nil {
					s.rpcs = append(s.rpcs, protoServiceRPC{
						service: service, method: m[1], request: qualify(pkg, m[2]),
					})
				}
				continue
			}
			if len(stack) == 0 || opaque[len(opaque)-1] || stack[len(stack)-1] == "" {
				continue
			}
			owner := stack[len(stack)-1]
			if m := protoMapRe.FindStringSubmatch(ln); m != nil {
				raws = append(raws, rawField{owner: owner, scope: owner, typ: m[1]})
				continue
			}
			if m := protoFieldRe.FindStringSubmatch(ln); m != nil {
				raws = append(raws, rawField{owner: owner, scope: owner, typ: m[1]})
			}
		}
		// An unbalanced scope stack means the scanner lost track of where it was,
		// and everything declared after that point was filed under a name nothing
		// queries. That reads exactly like a clean repository, so it is fatal.
		if len(stack) != 0 {
			t.Fatalf("%s: scope stack still %d deep at end of file (%v) — the .proto scanner "+
				"mis-parsed a brace, and every message after that point is invisible to this guard",
				path, len(stack), stack)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("%s does not exist — the schema tree moved and this guard is scanning nothing", root)
		}
		t.Fatalf("walk %s: %v", root, err)
	}

	for _, r := range raws {
		if resolved, ok := s.resolve(r.scope, r.typ); ok {
			s.fields[r.owner] = append(s.fields[r.owner], resolved)
		}
	}
	return s
}

// closesOnSameLine reports whether a scope opened on this line is also closed on
// it — `message X {}`, `service S {}`.
func closesOnSameLine(ln string) bool {
	open := strings.Index(ln, "{")
	return open >= 0 && strings.Contains(ln[open:], "}")
}

func qualify(pkg, name string) string {
	if strings.Contains(name, ".") || pkg == "" {
		return name
	}
	return pkg + "." + name
}

// resolve maps a field's written type to a known message's full name. Scalars,
// enums and well-known types do not resolve, and that is the answer: they cannot
// contain a Decimal.
func (s *protoSchema) resolve(scope, typ string) (string, bool) {
	if _, ok := s.fields[typ]; ok {
		return typ, true
	}
	for cur := scope; cur != ""; {
		if _, ok := s.fields[cur+"."+typ]; ok {
			return cur + "." + typ, true
		}
		idx := strings.LastIndex(cur, ".")
		if idx < 0 {
			break
		}
		cur = cur[:idx]
	}
	// A type written fully qualified (common.v1.Decimal) or relative to its own
	// package resolves by suffix, which is unambiguous here because every message
	// in this schema has a distinct qualified tail.
	var hit string
	for full := range s.fields {
		if strings.HasSuffix(full, "."+typ) {
			if hit != "" && hit != full {
				return "", false // ambiguous: refuse to guess
			}
			hit = full
		}
	}
	return hit, hit != ""
}

func (s *protoSchema) containsDecimal(full string) bool {
	return s.containsDecimalSeen(full, map[string]bool{})
}

func (s *protoSchema) containsDecimalSeen(full string, seen map[string]bool) bool {
	if full == decimalFullName {
		return true
	}
	if v, ok := s.memo[full]; ok {
		return v
	}
	if seen[full] { // recursive message: this branch adds nothing
		return false
	}
	seen[full] = true
	for _, f := range s.fields[full] {
		if s.containsDecimalSeen(f, seen) {
			s.memo[full] = true
			return true
		}
	}
	// Only memoize a negative once the whole subtree was walked without a cycle
	// short-circuit; otherwise a cycle could freeze a false answer.
	if len(seen) == 1 {
		s.memo[full] = false
	}
	return false
}
