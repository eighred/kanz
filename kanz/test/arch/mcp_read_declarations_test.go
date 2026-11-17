package arch

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestMCPToolDeclarationsRecognizeReadNames(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		source string
		owner  bool
		tools  int
	}{
		{
			name: "owner explicit and implicit",
			source: `package server
type Tool struct { Name string }
var explicit = Tool{Name: "get_order_state"}
var implicit = []Tool{{Name: "get_positions"}, {Name: "get_risk_measures"}}
`,
			owner: true, tools: 3,
		},
		{
			name: "canonical import alias",
			source: `package other
import reads "github.com/eighred/kanz/services/mcp/internal/server"
var explicit = reads.Tool{Name: "get_order_state"}
var implicit = []reads.Tool{{Name: "get_positions"}}
`,
			tools: 2,
		},
		{
			name: "actual tool lookup miss",
			source: `package server
type Tool struct { Name string }
type Server struct { tools []Tool }
func (s *Server) tool(name string) (Tool, bool) {
    for _, t := range s.tools {
        if t.Name == name { return t, true }
    }
    return Tool{}, false
}
`,
			owner: true,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			_, tools, offenders := scanMCPDeclarationFixture(t, fixture.source, fixture.owner)
			if tools != fixture.tools || len(offenders) != 0 {
				t.Fatalf("tools = %d, offenders = %v; want %d safe declarations", tools, offenders, fixture.tools)
			}
		})
	}
}

func TestMCPToolDeclarationsRejectCapitalNames(t *testing.T) {
	for _, name := range []string{"submit_order", "place-order", "cancelOrder", "amend_order", "execute-order", "send_order", "route-order"} {
		for _, shape := range []string{`Tool{Name: "NAME"}`, `[]Tool{{Name: "NAME"}}`} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				source := "package server\ntype Tool struct { Name string }\nvar declared = " + strings.ReplaceAll(shape, "NAME", name)
				_, tools, offenders := scanMCPDeclarationFixture(t, source, true)
				if tools != 1 || len(offenders) != 1 || offenders[0] != "tool "+name {
					t.Fatalf("tools = %d, offenders = %v; want rejection of actual declared name %q", tools, offenders, name)
				}
			})
		}
	}
	// Keep the original function-name boundary, including unexported functions.
	functions, tools, offenders := scanMCPDeclarationFixture(t, "package server\nfunc submitOrder() {}\nfunc CancelOrder() {}", true)
	if functions != 2 || tools != 0 || len(offenders) != 2 {
		t.Fatalf("function boundary lost: functions = %d, tools = %d, offenders = %v", functions, tools, offenders)
	}
}

func TestMCPToolDeclarationsRejectUnverifiableNames(t *testing.T) {
	for _, literal := range []string{
		`Tool{Name: dynamicName}`,
		`[]Tool{{Name: dynamicName}}`,
		`Tool{}`,
		`[]Tool{{}}`,
		`Tool{Name: ""}`,
	} {
		t.Run(literal, func(t *testing.T) {
			source := "package server\ntype Tool struct { Name string }\nvar declared = " + literal
			_, tools, offenders := scanMCPDeclarationFixture(t, source, true)
			if tools != 1 || len(offenders) != 1 || !strings.Contains(offenders[0], "explicit non-empty string literal") {
				t.Fatalf("tools = %d, offenders = %v; want unverifiable declaration refused", tools, offenders)
			}
		})
	}
	for _, fixture := range []struct {
		name string
		body string
	}{
		{"empty append registration", `func register(s *Server) { s.tools = append(s.tools, Tool{}) }`},
		{"empty successful lookup", `func (s *Server) tool(name string) (Tool, bool) { return Tool{}, true }`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source := "package server\ntype Tool struct { Name string }\ntype Server struct { tools []Tool }\n" + fixture.body
			_, tools, offenders := scanMCPDeclarationFixture(t, source, true)
			if tools != 1 || len(offenders) != 1 || !strings.Contains(offenders[0], "explicit non-empty string literal") {
				t.Fatalf("tools = %d, offenders = %v; want empty offered tool refused", tools, offenders)
			}
		})
	}
}

func TestMCPToolDeclarationsIgnoreCommentsAndOtherDTOs(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		source string
		owner  bool
	}{
		{
			name: "comments and unrelated Name fields",
			source: `package server
// Tool{Name: "submit_order"}
// func CancelOrder() {}
type DTO struct { Name string }
var data = DTO{Name: "place_order"}
var rows = []DTO{{Name: "amend_order"}}
func read() {
    type Tool struct { Name string }
    _ = Tool{Name: "execute_order"}
}
`,
			owner: true,
		},
		{
			name: "different Tool owner",
			source: `package other
type Tool struct { Name string }
var data = Tool{Name: "submit_order"}
`,
		},
		{
			name: "unrelated imported Tool",
			source: `package other
import server "example.com/another/server"
var data = server.Tool{Name: "submit_order"}
`,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			_, tools, offenders := scanMCPDeclarationFixture(t, fixture.source, fixture.owner)
			if tools != 0 || len(offenders) != 0 {
				t.Fatalf("non-tool source counted: tools = %d, offenders = %v", tools, offenders)
			}
		})
	}
}

func scanMCPDeclarationFixture(t *testing.T, source string, owner bool) (functions, tools int, offenders []string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return mcpReadDeclarations(f, owner)
}
