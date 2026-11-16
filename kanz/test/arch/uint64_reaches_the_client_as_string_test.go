package arch

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// #1307: bind integer representations to actual response types, rather than
// every property sharing a protobuf field name. These are serializer/type edges,
// not a second field schema: integer fields are derived from each named origin.
// Direct builtin int64/uint64 struct fields and direct scalar 64-bit fields on
// the bound top-level proto messages are covered. New endpoints, named Go
// integer aliases and recursively referenced types need an explicit binding.
// Bound proto maps/oneofs and 64-bit collections or field options refuse rather
// than receiving inferred client types. Nested definitions have their own owner.
//
// This bounded source guard is not a Go/TS type checker or arbitrary data-flow
// analysis. Named structs, explicit projections and protojson are the current
// forms. Missing/moved required declarations fail closed. HTTP/BFF/browser tests
// prove the real routing separately. uint64_json_domain_test.go retains the
// independent server map/getter guard protecting the mandate surface.
type integerStructWire struct{ origin, domain, wire, projection, client, declaration string }

var integerStructWires = []integerStructWire{
	{"kanz/internal/identity/access.go", "Access", "kanz/services/identity/internal/server/administration_json.go", "accessJSON", "users.ts", "UserAccess"},
	{"kanz/internal/identity/invite_lifecycle.go", "InvitationSummary", "kanz/services/identity/internal/server/administration_json.go", "invitationJSON", "invitations.ts", "InvitationSummary"},
	{"kanz/services/accounting/internal/server/custody.go", "breakView", "", "", "custody.ts", "CustodyBreak"},
	{"kanz/services/accounting/internal/custody/actions.go", "ActionState", "", "", "custody.ts", "ActionState"},
	{"kanz/services/audit/internal/audit/record.go", "Record", "kanz/services/audit/internal/server/server.go", "record", "evidence.ts", "EvidenceRecord"},
}

type integerProtoWire struct{ source, message, client, declaration string }

var integerProtoWires = []integerProtoWire{
	{"schemas/proto/compliance/v1/compliance.proto", "Mandate", "mandates.ts", "MandateDocument"},
	{"schemas/proto/common/v1/decimal.proto", "Decimal", "decimal.ts", "Decimal"},
	{"schemas/proto/order/v1/order_query.proto", "ListOrdersResponse", "risk.ts", "OrdersResponse"},
}

const integerComplianceSource = "kanz/services/compliance/internal/api/api.go"

func integerWireSources(t *testing.T) map[string]string {
	t.Helper()
	paths := map[string]bool{integerComplianceSource: true, "kanz/services/accounting/internal/custody/custody.go": true}
	for _, c := range integerStructWires {
		paths[c.origin] = true
		if c.wire != "" {
			paths[c.wire] = true
		}
		paths["web/src/api/"+c.client] = true
	}
	for _, c := range integerProtoWires {
		paths[c.source] = true
		paths["web/src/api/"+c.client] = true
	}
	out := map[string]string{}
	for path := range paths {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		out[path] = string(b)
	}
	return out
}

func TestAUint64FieldReachesTheClientAsAString(t *testing.T) {
	if problems := integerWireProblems(integerWireSources(t)); len(problems) != 0 {
		t.Fatalf("unsafe or unresolved browser integer contracts:\n%s", strings.Join(problems, "\n"))
	}
}

type integerJSONField struct {
	goType string
	quoted bool
}

func integerGoFile(source string) (*ast.File, error) {
	return parser.ParseFile(token.NewFileSet(), "contract.go", source, parser.SkipObjectResolution)
}

func integerJSONFields(source, name string) (map[string]integerJSONField, error) {
	f, err := integerGoFile(source)
	if err != nil {
		return nil, err
	}
	var found *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if d, ok := n.(*ast.TypeSpec); ok && d.Name.Name == name {
			found, _ = d.Type.(*ast.StructType)
		}
		return true
	})
	if found == nil {
		return nil, fmt.Errorf("missing struct %s", name)
	}
	out := map[string]integerJSONField{}
	for _, field := range found.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		tag := ""
		if field.Tag != nil {
			var e error
			tag, e = strconv.Unquote(field.Tag.Value)
			if e != nil {
				return nil, e
			}
		}
		parts := strings.Split(reflect.StructTag(tag).Get("json"), ",")
		if parts[0] == "-" {
			continue
		}
		for _, name := range field.Names {
			if !name.IsExported() {
				continue
			}
			key := parts[0]
			if key == "" {
				key = name.Name
			}
			if _, duplicate := out[key]; duplicate {
				return nil, fmt.Errorf("duplicate JSON key %s.%s", name, key)
			}
			out[key] = integerJSONField{integerExpr(field.Type), strings.Contains(","+strings.Join(parts[1:], ",")+",", ",string,")}
		}
	}
	return out, nil
}

func integerExpr(n ast.Node) string {
	var out bytes.Buffer
	if format.Node(&out, token.NewFileSet(), n) != nil {
		return ""
	}
	return out.String()
}

func integerFunction(source, name string) *ast.FuncDecl {
	f, err := integerGoFile(source)
	if err != nil {
		return nil
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

func integerWireProblems(sources map[string]string) []string {
	var problems []string
	check := func(path, declaration, field string, bounded bool) {
		fields, err := integerTSFields(sources[path], declaration)
		if err != nil {
			problems = append(problems, path+": "+err.Error())
			return
		}
		declared, ok := fields[field]
		if !ok || bounded && declared != "number" || !bounded && !tsTypeIsString(declared) {
			problems = append(problems, fmt.Sprintf("%s:%s.%s unsafe/missing type %q", path, declaration, field, declared))
		}
	}
	for _, c := range integerStructWires {
		origin, err := integerJSONFields(sources[c.origin], c.domain)
		if err != nil {
			problems = append(problems, c.origin+": "+err.Error())
			continue
		}
		wire := origin
		if c.wire != "" {
			wire, err = integerJSONFields(sources[c.wire], c.projection)
			if err != nil {
				problems = append(problems, c.wire+": "+err.Error())
				continue
			}
		}
		checked := 0
		for key, field := range origin {
			if field.goType != "int64" && field.goType != "uint64" {
				continue
			}
			checked++
			bounded := c.domain == "breakView" && key == "age_seconds" && integerBoundedAge(sources)
			projected, ok := wire[key]
			if !ok || !bounded && projected.goType != "string" && !(projected.goType == field.goType && projected.quoted) {
				problems = append(problems, c.domain+"."+key+": JSON must encode 64-bit integer as text")
			}
			check("web/src/api/"+c.client, c.declaration, key, bounded)
		}
		if checked == 0 {
			problems = append(problems, c.domain+": no 64-bit fields; binding is vacuous")
		}
	}
	for _, c := range integerProtoWires {
		fields, err := integerProtoFields(sources[c.source], c.message)
		if err != nil {
			problems = append(problems, c.source+": "+err.Error())
			continue
		}
		for _, field := range fields {
			check("web/src/api/"+c.client, c.declaration, field, false)
		}
	}
	// Hand-built mandate views use the same qualified Mandate.version contract.
	for _, name := range []string{"handlePropose", "handleApprove", "proposalJSON"} {
		fn := integerFunction(sources[integerComplianceSource], name)
		found, safe := 0, true
		if fn != nil {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				kv, ok := n.(*ast.KeyValueExpr)
				if !ok || integerExpr(kv.Key) != `"version"` {
					return true
				}
				found++
				call, ok := kv.Value.(*ast.CallExpr)
				if !ok || integerExpr(call.Fun) != "jsonUint64" || len(call.Args) != 1 || !strings.HasSuffix(integerExpr(call.Args[0]), ".GetVersion()") {
					safe = false
				}
				return true
			})
		}
		if found != 1 || !safe {
			problems = append(problems, name+": unresolved/unsafe Mandate.version map serialization")
		}
	}
	fn := integerFunction(sources[integerComplianceSource], "jsonUint64")
	if !integerUint64Formatter(fn) {
		problems = append(problems, "jsonUint64: unsigned string conversion changed")
	}
	for _, declaration := range []string{"PendingMandateChange", "ProposedMandateChange"} {
		check("web/src/api/mandates.ts", declaration, "version", false)
	}
	sort.Strings(problems)
	return problems
}

func integerUint64Formatter(fn *ast.FuncDecl) bool {
	if fn == nil || fn.Body == nil || len(fn.Body.List) != 1 {
		return false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	return ok && integerExpr(call.Fun) == "strconv.FormatUint" && len(call.Args) == 2 && integerExpr(call.Args[0]) == "v" && integerExpr(call.Args[1]) == "10"
}

func integerBoundedAge(sources map[string]string) bool {
	// time.Duration saturates within int64 nanoseconds. Whole seconds are at
	// most 9,223,372,036, below 2^53. Tie this premise to its actual producer,
	// rather than allowing every age_seconds or every int64 to be a number.
	ageSource := sources["kanz/services/accounting/internal/custody/custody.go"]
	viewSource := sources["kanz/services/accounting/internal/server/custody.go"]
	ageFile, err := integerGoFile(ageSource)
	if err != nil || !integerDefaultImport(ageFile, "time", "time") {
		return false
	}
	viewFile, err := integerGoFile(viewSource)
	if err != nil || !integerDefaultImport(viewFile, "time", "time") || !integerDefaultImport(viewFile, "custody", "github.com/eighred/kanz/services/accounting/internal/custody") {
		return false
	}
	var age *ast.FuncDecl
	breakStructs := 0
	for _, declaration := range ageFile.Decls {
		if group, ok := declaration.(*ast.GenDecl); ok {
			for _, spec := range group.Specs {
				if named, ok := spec.(*ast.TypeSpec); ok && named.Name.Name == "Break" {
					if _, ok := named.Type.(*ast.StructType); !ok || named.Assign.IsValid() {
						return false
					}
					breakStructs++
				}
			}
		}
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "Age" && fn.Recv != nil && len(fn.Recv.List) == 1 && integerExpr(fn.Recv.List[0].Type) == "Break" {
			if age != nil {
				return false
			}
			age = fn
		}
	}
	view := integerFunction(viewSource, "toBreakView")
	if breakStructs != 1 || age == nil || age.Type.Params == nil || len(age.Type.Params.List) != 1 || integerExpr(age.Type.Params.List[0].Type) != "time.Time" || age.Type.Results == nil || len(age.Type.Results.List) != 1 || integerExpr(age.Type.Results.List[0].Type) != "time.Duration" || view == nil || view.Recv != nil || view.Type.Params == nil || len(view.Type.Params.List) != 2 {
		return false
	}
	for i, expected := range []struct{ name, kind string }{{"b", "custody.Break"}, {"now", "time.Time"}} {
		parameter := view.Type.Params.List[i]
		if len(parameter.Names) != 1 || parameter.Names[0].Name != expected.name || integerExpr(parameter.Type) != expected.kind {
			return false
		}
	}
	found, safe := 0, true
	ast.Inspect(view.Body, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok && integerExpr(kv.Key) == "AgeSeconds" {
			found++
			if integerExpr(kv.Value) != "int64(b.Age(now).Seconds())" {
				safe = false
			}
		}
		return true
	})
	return found == 1 && safe
}

// Only the current unaliased named imports establish this narrow premise.
// This does not infer package names or types through arbitrary Go aliases.
func integerDefaultImport(file *ast.File, name, path string) bool {
	found := 0
	for _, imported := range file.Imports {
		value, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return false
		}
		binding := filepath.Base(value)
		if imported.Name != nil {
			binding = imported.Name.Name
		}
		if binding == name {
			if value != path || imported.Name != nil {
				return false
			}
			found++
		}
	}
	return found == 1
}

var integerProtoName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func integerProtoFields(source, message string) ([]string, error) {
	tokens, err := integerTSTokens(source)
	if err != nil {
		return nil, err
	}
	// Protobuf statements are semicolon-delimited, independent of line layout.
	var flat []string
	for _, token := range tokens {
		if token != "\n" {
			flat = append(flat, token)
		}
	}
	var body []string
	found := false
	for i := 0; i < len(flat); i++ {
		if flat[i] != "message" {
			if flat[i] == "{" {
				i, err = integerClosing(flat, i)
				if err != nil {
					return nil, err
				}
			}
			continue
		}
		if i+2 >= len(flat) || !integerProtoName.MatchString(flat[i+1]) || flat[i+2] != "{" {
			return nil, fmt.Errorf("unsupported protobuf message declaration")
		}
		end, err := integerClosing(flat, i+2)
		if err != nil {
			return nil, err
		}
		if flat[i+1] == message {
			if found {
				return nil, fmt.Errorf("duplicate top-level protobuf message %s", message)
			}
			body, found = flat[i+3:end], true
		}
		// Nested declarations cannot impersonate the qualified top-level type.
		i = end
	}
	if !found {
		return nil, fmt.Errorf("missing top-level protobuf message %s", message)
	}
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(body); {
		if body[i] == ";" {
			i++
			continue
		}
		if body[i] == "message" || body[i] == "enum" {
			if i+2 >= len(body) || !integerProtoName.MatchString(body[i+1]) || body[i+2] != "{" {
				return nil, fmt.Errorf("unsupported nested declaration in %s", message)
			}
			end, err := integerClosing(body, i+2)
			if err != nil {
				return nil, err
			}
			i = end + 1
			continue
		}
		// These need a separate reviewed wire shape, not scalar string inference.
		if body[i] == "map" || body[i] == "oneof" || body[i] == "group" {
			return nil, fmt.Errorf("unsupported %s form in bound message %s", body[i], message)
		}
		end := i
		for end < len(body) && body[end] != ";" {
			if body[end] == "{" || body[end] == "}" {
				return nil, fmt.Errorf("unsupported field block in bound message %s", message)
			}
			end++
		}
		if end == len(body) {
			return nil, fmt.Errorf("unterminated field in bound message %s", message)
		}
		field := body[i:end]
		i = end + 1
		label := ""
		if len(field) > 0 && (field[0] == "optional" || field[0] == "required" || field[0] == "repeated") {
			label, field = field[0], field[1:]
		}
		if len(field) == 0 {
			return nil, fmt.Errorf("empty field in bound message %s", message)
		}
		switch field[0] {
		case "int64", "sint64", "sfixed64", "uint64", "fixed64":
			if label == "repeated" || len(field) != 4 || !integerProtoName.MatchString(field[1]) || field[2] != "=" {
				return nil, fmt.Errorf("unsupported 64-bit field form in bound message %s", message)
			}
			if number, err := strconv.ParseUint(field[3], 10, 29); err != nil || number == 0 {
				return nil, fmt.Errorf("invalid field number in bound message %s", message)
			}
			if seen[field[1]] {
				return nil, fmt.Errorf("duplicate protobuf field %s.%s", message, field[1])
			}
			seen[field[1]] = true
			out = append(out, field[1])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no direct scalar 64-bit fields", message)
	}
	return out, nil
}

// Parse individual named-interface properties, including same-line semicolons,
// comments, quoted literals and nested types. No mapped/generic type inference
// and no Node toolchain in the Go architecture suite.
func integerTSFields(source, declaration string) (map[string]string, error) {
	tokens, err := integerTSTokens(source)
	if err != nil {
		return nil, err
	}
	for i := 0; i+2 < len(tokens); i++ {
		if tokens[i] != "interface" || tokens[i+1] != declaration {
			continue
		}
		start := i + 2
		for start < len(tokens) && tokens[start] != "{" {
			start++
		}
		if start == len(tokens) {
			break
		}
		end, err := integerClosing(tokens, start)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		part := start + 1
		for j := part; j <= end; j++ {
			if j < end && (tokens[j] == "{" || tokens[j] == "[" || tokens[j] == "(") {
				j, err = integerClosing(tokens, j)
				if err != nil {
					return nil, err
				}
				continue
			}
			if j != end && tokens[j] != ";" && tokens[j] != "\n" {
				continue
			}
			p := tokens[part:j]
			if len(p) >= 3 {
				colon := 1
				if p[colon] == "?" {
					colon++
				}
				if p[colon] == ":" {
					if _, duplicate := out[p[0]]; duplicate {
						return nil, fmt.Errorf("duplicate %s.%s", declaration, p[0])
					}
					out[p[0]] = strings.Join(p[colon+1:], " ")
				}
			}
			part = j + 1
		}
		return out, nil
	}
	return nil, fmt.Errorf("missing interface %s", declaration)
}

func integerClosing(tokens []string, start int) (int, error) {
	close := map[string]string{"{": "}", "[": "]", "(": ")"}[tokens[start]]
	for i := start + 1; i < len(tokens); i++ {
		if tokens[i] == close {
			return i, nil
		}
		if tokens[i] == "{" || tokens[i] == "[" || tokens[i] == "(" {
			var err error
			i, err = integerClosing(tokens, i)
			if err != nil {
				return 0, err
			}
		}
	}
	return 0, fmt.Errorf("unterminated %s declaration", tokens[start])
}

func integerTSTokens(source string) ([]string, error) {
	var out []string
	for i := 0; i < len(source); {
		c := source[i]
		if c == ' ' || c == '\t' || c == '\r' {
			i++
			continue
		}
		if c == '/' && i+1 < len(source) && source[i+1] == '/' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(source) && source[i+1] == '*' {
			end := strings.Index(source[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment")
			}
			i += end + 4
			continue
		}
		start := i
		if c == '\'' || c == '"' || c == '`' {
			i++
			for i < len(source) && source[i] != c {
				if source[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(source) {
				return nil, fmt.Errorf("unterminated string")
			}
			i++
		} else if integerIdentifier(c) {
			i++
			for i < len(source) && integerIdentifier(source[i]) {
				i++
			}
		} else {
			i++
		}
		out = append(out, source[start:i])
	}
	return out, nil
}

func integerIdentifier(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$'
}

func tsTypeIsString(declared string) bool {
	sawString := false
	for _, part := range strings.Split(declared, "|") {
		switch strings.TrimSpace(part) {
		case "string":
			sawString = true
		case "null", "undefined":
		default:
			return false
		}
	}
	return sawString
}
