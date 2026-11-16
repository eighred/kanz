package agent_boundary_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
)

// This suite intentionally runs only inside the dedicated bounded Linux
// namespace. Ordinary package runs disclose the skip; its CI evidence checker
// requires every named child to pass and rejects any skip.
func TestRealAgentBoundary(t *testing.T) {
	if os.Getenv("TEST_AGENT_BOUNDARY") != "1" {
		t.Skip("requires isolated Linux agent-boundary qualification")
	}
	if runtime.GOOS != "linux" {
		t.Skip("agent-boundary runtime proof requires its bounded Linux namespace")
	}
	root := os.Getenv("TEST_AGENT_SOURCE_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("TEST_AGENT_SOURCE_ROOT must identify the read-only source checkout")
	}
	paths := map[string]string{}
	for _, name := range []string{"GATEWAY", "MCP", "OPTIMIZATION", "MUTANT_GATEWAY"} {
		paths[name] = os.Getenv("TEST_AGENT_" + name + "_BINARY")
		if !filepath.IsAbs(paths[name]) {
			t.Fatalf("TEST_AGENT_%s_BINARY must identify the compiled fixture", name)
		}
	}
	routes := capabilityOracle(t, filepath.Join(root, "kanz", "services", "api_gateway", "internal", "authz", "arch_test.go"))
	emitRouteInventory(t, root, routes)
	issuer := newFixtureIssuer(t)
	risk, riskAddr := newRiskFixture(t)
	gatewaySocket, gatewayRisk, gatewayOperator := gatewayMeshFixture(t, risk)
	mcpAddr, optAddr := loopbackAddress(t), loopbackAddress(t)
	policy := filepath.Join(t.TempDir(), "read_policy.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"agent.reader":["risk.read","risk.scenario"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureProcess(t, paths["MCP"], map[string]string{
		"MCP_LISTEN": mcpAddr, "MCP_METRICS_LISTEN": loopbackAddress(t), "MCP_RISK_QUERY_ADDR": riskAddr, "MCP_POLICY_FILE": policy,
	}, "http://"+mcpAddr+"/readyz")
	fixtureProcess(t, paths["OPTIMIZATION"], map[string]string{
		"OPTIMIZATION_LISTEN": optAddr, "OPTIMIZATION_METRICS_LISTEN": loopbackAddress(t),
		"OPTIMIZATION_TENANT": "tenant_a", "OPTIMIZATION_PROPOSAL_MAX_AGE": "1m",
	}, "http://"+optAddr+"/readyz")
	var forwarded atomic.Int64
	inert := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"fixture": "inert-authorized-upstream", "principal": r.Header.Get(auth.HeaderPrincipalSubject),
			"tenant": r.Header.Get(auth.HeaderPrincipalTenant),
		})
	}))
	t.Cleanup(inert.Close)
	startGateway := func(t *testing.T, binary string) string {
		t.Helper()
		addr := loopbackAddress(t)
		fixtureProcess(t, binary, map[string]string{
			"API_GATEWAY_LISTEN": addr, "API_GATEWAY_RISK_ENGINE_ADDR": gatewayRisk, "API_GATEWAY_OMS_READ_ADDR": gatewayRisk,
			"API_GATEWAY_SPIFFE_SOCKET": gatewaySocket,
			"API_GATEWAY_OIDC_ISSUER":   issuer.server.URL, "API_GATEWAY_OIDC_AUDIENCE": "agent-boundary",
			"API_GATEWAY_OIDC_JWKS_URI": issuer.server.URL + "/jwks", "API_GATEWAY_REVOCATIONS_URI": issuer.server.URL + "/revocations",
			"API_GATEWAY_REQUIRED_ROLE": readerRole, "API_GATEWAY_TRADE_ROLE": "human.trade",
			"API_GATEWAY_OPERATOR_ROLE": "human.operate", "API_GATEWAY_OPERATOR_ADDR": gatewayOperator,
			"API_GATEWAY_FUND_ROLE": "human.fund", "API_GATEWAY_ACCOUNTING_ADDR": inert.URL,
			"API_GATEWAY_APPROVE_ROLE": "human.approve", "API_GATEWAY_MANDATE_ROLE": "human.mandate",
			"API_GATEWAY_COMPLIANCE_ADDR": inert.URL, "API_GATEWAY_DATAMASTER_ADDR": inert.URL,
			"API_GATEWAY_AUDIT_ROLE": "human.audit", "API_GATEWAY_AUDIT_ADDR": inert.URL,
			"API_GATEWAY_MCP_ADDR": "http://" + mcpAddr, "API_GATEWAY_OPTIMIZATION_ADDR": "http://" + optAddr,
		}, "http://"+addr+"/readyz")
		return "http://" + addr
	}
	gateway := startGateway(t, paths["GATEWAY"])
	portfolios := []string{"PF_A", "PF_B", "MISSING", "OWNER_DOWN", "OWNER_UNKNOWN", "PF_A_UNKNOWN", "PF_A_STALE"}
	reader := issuer.token(t, "tenant_a", []string{readerRole}, portfolios)
	forge := map[string]string{
		auth.HeaderPrincipalSubject: "forged:administrator", auth.HeaderPrincipalTenant: "tenant_b",
		auth.HeaderPrincipalRoles:      "human.trade,human.mandate,human.fund,human.operate",
		auth.HeaderPrincipalPortfolios: `["*"]`,
	}

	t.Run("GatewayMutationDenials", func(t *testing.T) {
		beforeHTTP, beforeRPC := forwarded.Load(), risk.calls.Load()
		for _, route := range routes {
			if route.capability == "Read" {
				continue
			}
			t.Run(route.name(), func(t *testing.T) {
				status, _ := request(t, route.method(), gateway+route.path(), reader, `{}`, nil)
				if err := denied(status); err != nil {
					t.Fatal(err)
				}
			})
		}
		if forwarded.Load() != beforeHTTP || risk.calls.Load() != beforeRPC {
			t.Fatal("a denied agent request reached an upstream")
		}
	})
	t.Run("GatewayAnonymousDenials", func(t *testing.T) {
		for _, route := range routes {
			if route.capability == "Read" {
				continue
			}
			t.Run(route.name(), func(t *testing.T) {
				status, _ := request(t, route.method(), gateway+route.path(), "", `{}`, forge)
				if status != http.StatusUnauthorized {
					t.Fatalf("anonymous request with forged headers returned %d, require401", status)
				}
			})
		}
	})
	t.Run("GatewayPrivilegedControls", func(t *testing.T) {
		token := issuer.tokenFor(t, "human:control-proof", "tenant_a", []string{readerRole, "human.mandate"}, []string{"PF_A"})
		before := forwarded.Load()
		status, body := request(t, http.MethodPost, gateway+"/v1/portfolios/PF_A/mandate", token, `{}`, forge)
		if status != 200 || forwarded.Load() != before+1 || !strings.Contains(string(body), `"principal":"human:control-proof"`) || !strings.Contains(string(body), `"tenant":"tenant_a"`) {
			t.Fatalf("privileged inert control failed or trusted forged identity: status%d", status)
		}
	})
	t.Run("GatewayWritesDisabled", func(t *testing.T) {
		control := issuer.tokenFor(t, "human:disabled-writes-proof", "tenant_a",
			[]string{readerRole, "human.trade", "human.approve"}, []string{"PF_A"})
		beforeHTTP, beforeRPC := forwarded.Load(), risk.calls.Load()
		for _, command := range []struct{ path, body string }{
			{"/v1/orders", `{"orderId":"DISABLED_SUBMIT"}`},
			{"/v1/orders/DISABLED_CANCEL/cancel", `{}`},
			{"/v1/orders/DISABLED_APPROVE/approve", `{"digest":"fixture-command-digest"}`},
		} {
			status, body := request(t, http.MethodPost, gateway+command.path, control, command.body, nil)
			var refusal struct {
				Error string `json:"error"`
			}
			if status != http.StatusServiceUnavailable || json.Unmarshal(body, &refusal) != nil || refusal.Error != "order writes are disabled" {
				t.Fatalf("no-broker gateway %s did not report its disabled write surface: status%d", command.path, status)
			}
		}
		if forwarded.Load() != beforeHTTP || risk.calls.Load() != beforeRPC {
			t.Fatal("disabled order commands reached an upstream")
		}
	})
	t.Run("GatewayForgedHeaders", func(t *testing.T) {
		before := forwarded.Load()
		for _, path := range []string{"/v1/orders", "/v1/model-portfolios/orders", "/v1/portfolios/PF_A/mandate"} {
			status, _ := request(t, http.MethodPost, gateway+path, reader, `{}`, forge)
			if err := denied(status); err != nil {
				t.Fatal(err)
			}
		}
		if forwarded.Load() != before {
			t.Fatal("forged principal reached a mutation upstream")
		}
	})
	t.Run("GatewayUnknownAmend", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPatch} {
			status, _ := request(t, method, gateway+"/v1/orders/ORDER_1/amend", reader, `{}`, nil)
			if status != http.StatusNotFound {
				t.Fatalf("unimplemented amendment acquired a route: %d", status)
			}
		}
	})
	t.Run("MCPToolInventory", func(t *testing.T) {
		status, body := request(t, http.MethodPost, gateway+"/v1/mcp", reader, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
		var response struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if status != 200 || json.Unmarshal(body, &response) != nil || len(response.Result.Tools) != 1 || response.Result.Tools[0].Name != "get_risk_measures" {
			t.Fatal("actual MCP tool inventory changed without a reviewed read projection")
		}
	})
	t.Run("MCPReadProjection", func(t *testing.T) {
		status, _ := request(t, http.MethodGet, gateway+"/v1/portfolios/PF_A/exposure", reader, "", nil)
		if status != http.StatusOK {
			t.Fatalf("gateway's actual workload-authenticated risk read failed: %d", status)
		}
		projection := mcpProjection(t, gateway, reader, "PF_A", forge)
		if projection.PortfolioID != "PF_A" || len(projection.Measures) != 1 || projection.Measures[0].Status != "MEASURED" || projection.Measures[0].Value == nil || *projection.Measures[0].Value != 42 || projection.AsOf == nil {
			t.Fatal("actual MCP/gateway/gRPC read did not preserve the governed fixture")
		}
	})
	t.Run("MCPTenantIsolation", func(t *testing.T) {
		foreign := mcpError(t, gateway, reader, "PF_B")
		missing := mcpError(t, gateway, reader, "MISSING")
		if strings.ReplaceAll(foreign, "PF_B", "RESOURCE") != strings.ReplaceAll(missing, "MISSING", "RESOURCE") {
			t.Fatal("cross-tenant and nonexistent portfolio are distinguishable")
		}
		outOfScope := issuer.token(t, "tenant_a", []string{readerRole}, []string{"OTHER"})
		_ = mcpError(t, gateway, outOfScope, "PF_A")
		wrongTenant := issuer.token(t, "tenant_b", []string{readerRole}, []string{"PF_A"})
		_ = mcpError(t, gateway, wrongTenant, "PF_A")
	})
	t.Run("MCPUnknownOwner", func(t *testing.T) {
		for _, id := range []string{"OWNER_DOWN", "OWNER_UNKNOWN"} {
			_ = mcpError(t, gateway, reader, id)
		}
	})
	t.Run("MCPUnknownCoverage", func(t *testing.T) {
		projection := mcpProjection(t, gateway, reader, "PF_A_UNKNOWN", nil)
		if len(projection.Measures) != 1 || projection.Measures[0].Status != "UNAVAILABLE" || projection.Measures[0].Value != nil || !strings.Contains(projection.Measures[0].Reason, "no_terms") {
			t.Fatal("unknown/partial risk became a plausible number")
		}
	})
	t.Run("MCPStaleness", func(t *testing.T) {
		projection := mcpProjection(t, gateway, reader, "PF_A_STALE", nil)
		if projection.AsOf == nil || len(projection.QualityFlags) != 1 || projection.QualityFlags[0] != "STALE" {
			t.Fatal("stale read lost its explicit freshness evidence")
		}
	})
	t.Run("MCPMutationRefusals", func(t *testing.T) {
		before := risk.calls.Load()
		for _, name := range []string{"submit_order", "cancel_order", "amend_order", "set_risk_policy", "change_mandate"} {
			body := mcpCall(name, "PF_A")
			status, data := request(t, http.MethodPost, gateway+"/v1/mcp", reader, body, nil)
			var reply rpcReply
			if status != 200 || json.Unmarshal(data, &reply) != nil || reply.Error == nil || reply.Error.Code != -32601 {
				t.Fatalf("MCP accepted mutation tool %s", name)
			}
		}
		if risk.calls.Load() != before {
			t.Fatal("unknown mutation tool reached a domain API")
		}
	})
	t.Run("RequestBodyBound", func(t *testing.T) {
		before := risk.calls.Load()
		status, _ := request(t, http.MethodPost, gateway+"/v1/mcp", reader, strings.Repeat(" ", (1<<20)+1), nil)
		if status != http.StatusRequestEntityTooLarge || risk.calls.Load() != before {
			t.Fatalf("oversized request reached the read plane: %d", status)
		}
	})
	const proposal = `{"portfolio_id":"PF_A","instruments":["A"],"expected_returns":[0.1],"objective":{"Type":0},"current_weights":{},"nav":"9007199254740993","prices":{"A":"0.000000000001"},"threshold":"0","currency":"USD"}`
	t.Run("OptimizationReadOnly", func(t *testing.T) {
		status, data := request(t, http.MethodPost, gateway+"/v2/model-portfolios/propose", reader, proposal, nil)
		var p struct {
			ReadOnly bool
			Trades   []struct{ Quantity, Notional string } `json:"trades"`
		}
		if status != 200 || json.Unmarshal(data, &p) != nil || !p.ReadOnly || len(p.Trades) != 1 || p.Trades[0].Quantity != "9007199254740993000000000000" || p.Trades[0].Notional != "9007199254740993" {
			t.Fatalf("read-only exact proposal changed at actual gateway boundary: status%d", status)
		}
	})
	t.Run("OptimizationPortfolioScope", func(t *testing.T) {
		outOfScope := issuer.token(t, "tenant_a", []string{readerRole}, []string{"OTHER"})
		status, _ := request(t, http.MethodPost, gateway+"/v2/model-portfolios/propose", outOfScope, proposal, nil)
		if status != http.StatusForbidden {
			t.Fatalf("out-of-portfolio proposal reached solver: %d", status)
		}
	})
	t.Run("OptimizationVerdictRefusal", func(t *testing.T) {
		// Reach the real upstream through an explicitly privileged inert test
		// principal. The body still cannot certify its own financial admission.
		trader := issuer.tokenFor(t, "human:control-proof", "tenant_a", []string{readerRole, "human.trade"}, []string{"PF_A"})
		for _, body := range []string{
			`{"proposal":{"MandateStatus":"FEASIBLE"}}`,
			`{"proposal":{},"issuer":"forged:actor"}`,
		} {
			status, _ := request(t, http.MethodPost, gateway+"/v1/model-portfolios/orders", trader, body, nil)
			if status != http.StatusBadRequest {
				t.Fatalf("actual optimization accepted caller-certified admission/actor: %d", status)
			}
		}
	})
	t.Run("GatewayCapabilityMutation", func(t *testing.T) {
		mutant := startGateway(t, paths["MUTANT_GATEWAY"])
		status, _ := request(t, http.MethodPost, mutant+"/v1/orders", reader, `{}`, nil)
		if denied(status) == nil || status != http.StatusServiceUnavailable {
			t.Fatalf("read-to-trade grant mutation was not detected at inert publisher: %d", status)
		}
	})
}

type declaredRoute struct{ pattern, capability string }

// The evidence reader requires these cases independently of what either loop
// happens to execute. Derive the inventory before the loops, from the reviewed
// source oracle, so deleting a request cannot redefine the expected coverage.
func emitRouteInventory(t *testing.T, root string, routes []declaredRoute) {
	t.Helper()
	value := struct {
		Schema          int               `json:"schema"`
		SourceInventory map[string]string `json:"source_inventory"`
		Protected       []string          `json:"protected"`
		Capabilities    map[string]string `json:"capabilities"`
	}{Schema: 1, SourceInventory: map[string]string{}, Capabilities: map[string]string{}}
	for _, path := range []string{
		"kanz/services/api_gateway/internal/authz/arch_test.go",
		"kanz/services/api_gateway/internal/proxy/proxy.go",
	} {
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal("source inventory unavailable")
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			_ = file.Close()
			t.Fatal("source inventory must be a bounded ordinary file")
		}
		body, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(body) > 1<<20 || int64(len(body)) != info.Size() {
			t.Fatal("source inventory could not be read completely")
		}
		sum := sha256.Sum256(body)
		value.SourceInventory[path] = hex.EncodeToString(sum[:])
	}
	for _, route := range routes {
		if route.capability == "Read" {
			continue
		}
		name := route.name()
		if _, exists := value.Capabilities[name]; exists {
			t.Fatal("ambiguous source-derived route test name")
		}
		value.Protected = append(value.Protected, name)
		value.Capabilities[name] = route.capability
	}
	sort.Strings(value.Protected)
	if len(value.Protected) == 0 {
		t.Fatal("source-derived protected route inventory is empty")
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("AGENT_BOUNDARY_ROUTE_INVENTORY:%s\n", data)
}

func (r declaredRoute) method() string { return strings.SplitN(r.pattern, " ", 2)[0] }
func (r declaredRoute) path() string {
	path := strings.SplitN(r.pattern, " ", 2)[1]
	return regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "PF_A")
}
func (r declaredRoute) name() string {
	return strings.NewReplacer(" ", "_", "/", "_", "{", "", "}", "").Replace(r.pattern)
}

func capabilityOracle(t *testing.T, path string) []declaredRoute {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var routes []declaredRoute
	maps := 0
	for _, declaration := range f.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TestTheWholeRouteTableIsDeclared" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
				return true
			}
			name, ok := assignment.Lhs[0].(*ast.Ident)
			if !ok || name.Name != "want" {
				return true
			}
			literal, ok := assignment.Rhs[0].(*ast.CompositeLit)
			if !ok {
				t.Fatal("route oracle is no longer an explicit reviewed map")
			}
			maps++
			for _, item := range literal.Elts {
				pair, ok := item.(*ast.KeyValueExpr)
				if !ok {
					t.Fatal("invalid oracle route entry")
				}
				key, ok := pair.Key.(*ast.BasicLit)
				if !ok || key.Kind != token.STRING {
					t.Fatal("nonliteral oracle route")
				}
				pattern, err := strconv.Unquote(key.Value)
				if err != nil {
					t.Fatal(err)
				}
				capability, ok := pair.Value.(*ast.SelectorExpr)
				if !ok {
					t.Fatal("nonliteral oracle capability")
				}
				qualifier, ok := capability.X.(*ast.Ident)
				if !ok || qualifier.Name != "authz" {
					t.Fatal("unexpected oracle capability owner")
				}
				routes = append(routes, declaredRoute{pattern, capability.Sel.Name})
			}
			return true
		})
	}
	// The existing golden table instantiates no audit role. Its
	// separate audit-role composition tests own that boundary. Include every
	// route in the live role-guarded block, asserting Audit independently of the
	// capability argument passed to Handle; changing it to Read must still fail.
	auditPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "proxy", "proxy.go")
	routes = append(routes, auditRouteOracle(t, auditPath)...)
	seen := map[string]bool{}
	capabilities := map[string]int{}
	for _, route := range routes {
		if seen[route.pattern] || !strings.Contains(route.pattern, " /v") {
			t.Fatal("ambiguous oracle route")
		}
		seen[route.pattern] = true
		capabilities[route.capability]++
	}
	for _, capability := range []string{"Read", "Trade", "Operate", "Fund", "Approve", "Mandate", "Audit"} {
		if capabilities[capability] == 0 {
			t.Fatalf("route oracle omitted %s", capability)
		}
	}
	if maps != 1 || len(routes) < 20 {
		t.Fatal("route oracle is absent or vacuous")
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].pattern < routes[j].pattern })
	return routes
}

func auditRouteOracle(t *testing.T, path string) []declaredRoute {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var routes []declaredRoute
	blocks := 0
	ast.Inspect(f, func(n ast.Node) bool {
		statement, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		condition, ok := statement.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.NEQ {
			return true
		}
		field, ok := condition.X.(*ast.SelectorExpr)
		if !ok || field.Sel.Name != "Audit" {
			return true
		}
		owner, ok := field.X.(*ast.SelectorExpr)
		if !ok || owner.Sel.Name != "roles" {
			return true
		}
		blocks++
		ast.Inspect(statement.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || method.Sel.Name != "Handle" {
				return true
			}
			literal, ok := call.Args[1].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatal("audit route oracle requires an explicit reviewed pattern")
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			routes = append(routes, declaredRoute{pattern, "Audit"})
			return true
		})
		return false
	})
	if blocks != 1 || len(routes) < 6 {
		t.Fatal("audit role route inventory is missing or ambiguous")
	}
	return routes
}

func TestGatewayCapabilityOracle(t *testing.T) {
	path := filepath.Join("..", "..", "services", "api_gateway", "internal", "authz", "arch_test.go")
	routes := capabilityOracle(t, path)
	protected := 0
	for _, route := range routes {
		if route.capability != "Read" {
			protected++
		}
	}
	t.Logf("reviewed source oracle resolves %d routes, %d protected", len(routes), protected)
}

type rpcReply struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
}
type projectedRead struct {
	PortfolioID  string   `json:"portfolio_id"`
	AsOf         *string  `json:"as_of"`
	QualityFlags []string `json:"quality_flags"`
	Measures     []struct {
		Status, Reason string
		Value          *float64 `json:"value"`
	} `json:"measures"`
}

func mcpCall(name, portfolio string) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": map[string]string{"portfolio_id": portfolio}}})
	return string(b)
}

func mcpProjection(t *testing.T, gateway, token, portfolio string, headers map[string]string) projectedRead {
	t.Helper()
	status, body := request(t, http.MethodPost, gateway+"/v1/mcp", token, mcpCall("get_risk_measures", portfolio), headers)
	var reply rpcReply
	if status != 200 || json.Unmarshal(body, &reply) != nil || reply.Error != nil || reply.Result.IsError || len(reply.Result.Content) != 1 {
		t.Fatalf("actual MCP read refused fixture %s: status%d", portfolio, status)
	}
	text := reply.Result.Content[0].Text
	for _, private := range []string{"PRIVATE-", "tenant_a", "tenant_b", "owner_tenant", "source_position", "source_event_ids"} {
		if strings.Contains(text, private) {
			t.Fatal("MCP projection exposed private domain fields")
		}
	}
	var projection projectedRead
	if json.Unmarshal([]byte(text), &projection) != nil {
		t.Fatal("actual MCP projection is not valid JSON")
	}
	return projection
}

func mcpError(t *testing.T, gateway, token, portfolio string) string {
	t.Helper()
	status, body := request(t, http.MethodPost, gateway+"/v1/mcp", token, mcpCall("get_risk_measures", portfolio), nil)
	var reply rpcReply
	if status != 200 || json.Unmarshal(body, &reply) != nil || reply.Error == nil {
		t.Fatalf("MCP disclosed refused portfolio %s: status%d", portfolio, status)
	}
	for _, private := range []string{"tenant_b", "PRIVATE-", "private upstream", "private missing"} {
		if strings.Contains(string(body), private) {
			t.Fatal("MCP refusal leaked private domain evidence")
		}
	}
	return reply.Error.Message
}
