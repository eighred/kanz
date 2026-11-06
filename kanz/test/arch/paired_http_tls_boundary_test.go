package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reviewed optional BFF-to-gateway hop only. These parsed source recipes are
// deliberately closed: a new transport/lifecycle requires a boundary review.
// This is syntax coverage, not general Go data flow or runtime TLS proof.
const reviewedBFFTLS = `package main

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
)

// The secure endpoint runs the same authentication/signing/authorization router.
// A BFF workload certificate authorizes the connection, never a trading action.
func newBFFTLSServer(addr string, handler http.Handler, bounds httpserver.Timeouts, source transport.Source) (*http.Server, error) {
	if source == nil || handler == nil {
		return nil, errors.New("BFF TLS listener requires workload identity and the gateway router")
	}
	peer, err := transport.ServiceID("kanz-services", "web-bff")
	if err != nil {
		return nil, err
	}
	server := httpserver.New(addr, handler, bounds)
	server.TLSConfig = transport.ServerTLSConfig(source, transport.AuthorizeServices(peer))
	return server, nil
}

type bffTLSListener struct {
	server      *http.Server
	listener    net.Listener
	closeSource func()
}

func prepareBFFTLS(ctx context.Context, cfg config.Config, handler http.Handler, bounds httpserver.Timeouts) (*bffTLSListener, error) {
	if cfg.BFFTLSListen == "" {
		return nil, nil
	}
	if cfg.SPIFFESocket == "" {
		return nil, errors.New("BFF TLS listener requires SPIFFE socket")
	}
	source, err := transport.NewSource(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, err
	}
	server, err := newBFFTLSServer(cfg.BFFTLSListen, handler, bounds, source)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.BFFTLSListen)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	return &bffTLSListener{server: server, listener: listener, closeSource: func() { _ = source.Close() }}, nil
}

func (p *bffTLSListener) close() {
	if p != nil {
		_ = p.listener.Close()
		p.closeSource()
	}
}
`

const reviewedEdgeConfig = `package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/eighred/kanz/services/web-bff/internal/edgeconfig"
)

// This init-container mode reads no session, signing, OIDC or tunnel credentials.
// The only values emitted are the closed proxy configuration and bounded ports.
func runEdgeConfig(args []string) int {
	o := edgeconfig.Default()
	flags := flag.NewFlagSet("envoy-config", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("output", "", "configuration output file in an existing directory")
	flags.IntVar(&o.ListenPort, "listen-port", o.ListenPort, "loopback application port")
	flags.IntVar(&o.BFFPort, "bff-port", o.BFFPort, "loopback BFF upstream port")
	flags.IntVar(&o.AdminPort, "admin-port", o.AdminPort, "readiness/metrics port")
	flags.IntVar(&o.RequestsPerSecond, "requests-per-second", o.RequestsPerSecond, "per-process request budget")
	flags.BoolVar(&o.AdminLoopback, "admin-loopback", false, "bind the control listener to loopback for local use")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *path == "" {
		fmt.Fprintln(os.Stderr, "invalid edge configuration arguments")
		return 2
	}
	if err := edgeconfig.Write(*path, o); err != nil {
		fmt.Fprintln(os.Stderr, "edge configuration generation failed")
		return 2
	}
	return 0
}
`

func parsedRecipe(src string) (*ast.File, error) {
	return parser.ParseFile(token.NewFileSet(), "recipe.go", src, 0)
}
func recipeEqual(got, want string) bool {
	a, e := parsedRecipe(got)
	if e != nil {
		return false
	}
	b, e := parsedRecipe(want)
	return e == nil && bondCloneNode(a) == bondCloneNode(b)
}
func reviewedEdgePrelude(fn *ast.FuncDecl) bool {
	if fn == nil || fn.Body == nil || len(fn.Body.List) != 2 {
		return false
	}
	want, _ := parsedRecipe(`package p; func main(){if len(os.Args)>1 && os.Args[1]=="envoy-config" {os.Exit(runEdgeConfig(os.Args[2:]))}; os.Exit(run())}`)
	return bondCloneNode(fn.Body) == bondCloneNode(want.Decls[0].(*ast.FuncDecl).Body)
}
func pairedTLSCall(call *ast.CallExpr) bool {
	want, e := parser.ParseExpr(`bffTLS.server.ServeTLS(bffTLS.listener, "", "")`)
	return e == nil && bondCloneNode(call) == bondCloneNode(want)
}

func checkPairedHTTPRecipes(files map[string]string) error {
	cfg, err := parsedRecipe(files["gateway-config.go"])
	if err != nil {
		return err
	}
	expected, _ := parsedRecipe(`package p;func f(){if cfg.BFFTLSListen != "" {
 _, port, err := net.SplitHostPort(cfg.BFFTLSListen)
 p, portErr := strconv.Atoi(port)
 if err != nil || portErr != nil || p < 1024 || p > 65535 || cfg.BFFTLSListen == cfg.Listen || cfg.SPIFFESocket == "" {
 return Config{}, errors.New("API_GATEWAY_BFF_TLS_LISTEN requires a distinct unprivileged TCP address and API_GATEWAY_SPIFFE_SOCKET")
 }
 }}`)
	wantValidation := bondCloneNode(expected.Decls[0].(*ast.FuncDecl).Body.List[0])
	validations, bindings := 0, 0
	ast.Inspect(cfg, func(n ast.Node) bool {
		if x, ok := n.(*ast.IfStmt); ok && bondCloneNode(x) == wantValidation {
			validations++
		}
		if x, ok := n.(*ast.KeyValueExpr); ok && bondCloneNode(x) == `BFFTLSListen:os.Getenv("API_GATEWAY_BFF_TLS_LISTEN")` {
			bindings++
		}
		return true
	})
	if validations != 1 || bindings != 1 {
		return fmt.Errorf("canonical optional TLS configuration/pairing refusal changed")
	}

	if !recipeEqual(files["bff_tls.go"], reviewedBFFTLS) || !recipeEqual(files["edge.go"], reviewedEdgeConfig) {
		return fmt.Errorf("paired TLS identity/lifecycle or credential-free config recipe changed")
	}
	web, e := parsedRecipe(files["web-main.go"])
	if e != nil {
		return e
	}
	valid := false
	for _, d := range web.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "main" {
			valid = reviewedEdgePrelude(f)
		}
	}
	if !valid {
		return fmt.Errorf("unreviewed pre-lifecycle command")
	}
	gateway, e := parsedRecipe(files["gateway-main.go"])
	if e != nil {
		return e
	}
	var run *ast.FuncDecl
	for _, d := range gateway.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "run" {
			run = f
		}
	}
	if run == nil {
		return fmt.Errorf("missing gateway lifecycle")
	}
	// Match complete statements, including control flow: comments or nested dead-code
	// fragments cannot satisfy the recipe. Ordinary unrelated router setup remains free.
	required := []string{
		`srv := httpserver.New(cfg.Listen, router, httpserver.Timeouts{ReadHeader:std.ReadHeader, Read:std.Read, Write:gatewayWriteTimeout, Idle:gatewayIdleTimeout})`,
		`bffTLS, err := prepareBFFTLS(ctx, cfg, router, httpserver.Timeouts{ReadHeader:std.ReadHeader, Read:std.Read, Write:gatewayWriteTimeout, Idle:gatewayIdleTimeout})`,
		`if err != nil {logger.Error("BFF workload listener initialization failed"); return 2}`,
		`defer bffTLS.close()`,
		`if bffTLS != nil { bffTLSDone = make(chan struct{}); go func(){defer close(bffTLSDone); if err := bffTLS.server.ServeTLS(bffTLS.listener,"",""); err != nil && !errors.Is(err,http.ErrServerClosed) {logger.Error("BFF workload listener failed"); fatal.Raise(err)}}() }`,
		`if bffTLS != nil {if err := bffTLS.server.Shutdown(sctx); err != nil {logger.Error("BFF workload listener shutdown failed"); _ = bffTLS.server.Close()}; <-bffTLSDone}`,
	}
	positions := []int{}
	for _, src := range required {
		f, e := parsedRecipe("package p;func f(){" + src + "}")
		if e != nil {
			return e
		}
		want := bondCloneNode(f.Decls[0].(*ast.FuncDecl).Body.List[0])
		count, pos := 0, -1
		for i, s := range run.Body.List {
			if bondCloneNode(s) == want {
				count++
				pos = i
			}
		}
		if count != 1 {
			return fmt.Errorf("paired gateway statement missing/duplicated: %s", src)
		}
		positions = append(positions, pos)
	}
	if positions[2] != positions[1]+1 || positions[3] != positions[2]+1 {
		return fmt.Errorf("TLS startup refusal/cleanup must immediately follow construction")
	}
	for i := 1; i < len(positions); i++ {
		if positions[i] <= positions[i-1] {
			return fmt.Errorf("TLS lifecycle ordering")
		}
	}
	serving, constructors := 0, 0
	ast.Inspect(gateway, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && (s.Sel.Name == "ServeTLS" || s.Sel.Name == "ListenAndServeTLS") {
				if pairedTLSCall(c) {
					serving++
				} else {
					serving += 100
				}
			}
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "prepareBFFTLS" {
				constructors++
			}
		}
		return true
	})
	if serving != 1 || constructors != 1 {
		return fmt.Errorf("unexpected TLS serving/construction site")
	}
	return nil
}

func pairedHTTPFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, path := range map[string]string{
		"bff_tls.go":        "services/api-gateway/cmd/api-gateway/bff_tls.go",
		"edge.go":           "services/web-bff/cmd/web-bff/edge.go",
		"web-main.go":       "services/web-bff/cmd/web-bff/main.go",
		"gateway-main.go":   gatewayServerFile,
		"gateway-config.go": "services/api-gateway/internal/config/config.go",
	} {
		b, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if e != nil {
			t.Fatal(e)
		}
		out[name] = string(b)
	}
	return out
}
func TestPairedHTTPBoundary(t *testing.T) {
	if e := checkPairedHTTPRecipes(pairedHTTPFiles(t, moduleRoot(t))); e != nil {
		t.Fatal(e)
	}
}
func TestPairedHTTPBoundaryMutations(t *testing.T) {
	original := pairedHTTPFiles(t, moduleRoot(t))
	if e := checkPairedHTTPRecipes(original); e != nil {
		t.Fatal(e)
	}
	cases := []struct{ name, file, old, new string }{
		{"config pair bypass", "gateway-config.go", `cfg.BFFTLSListen == cfg.Listen || cfg.SPIFFESocket == ""`, `false`},
		{"config default activation", "gateway-config.go", `os.Getenv("API_GATEWAY_BFF_TLS_LISTEN")`, `env.Or("API_GATEWAY_BFF_TLS_LISTEN", ":8443")`},
		{"identity bypass", "bff_tls.go", "transport.AuthorizeServices(peer)", "transport.AuthorizeAny()"},
		{"wrong peer", "bff_tls.go", `"web-bff"`, `"oms"`},
		{"missing socket refusal", "bff_tls.go", `cfg.SPIFFESocket == ""`, `false`},
		{"missing optional gate", "bff_tls.go", `cfg.BFFTLSListen == ""`, `false`},
		{"wrong router", "gateway-main.go", "prepareBFFTLS(ctx, cfg, router,", "prepareBFFTLS(ctx, cfg, http.NewServeMux(),"},
		{"wrong context", "gateway-main.go", "prepareBFFTLS(ctx, cfg, router,", "prepareBFFTLS(context.Background(), cfg, router,"},
		{"one timeout changed", "gateway-main.go", "Write: gatewayWriteTimeout, Idle:", "Write: gatewayWriteTimeout/2, Idle:"},
		{"cleanup omitted", "gateway-main.go", "defer bffTLS.close()", ""},
		{"join omitted", "gateway-main.go", "<-bffTLSDone", ""},
		{"fatal omitted", "gateway-main.go", `logger.Error("BFF workload listener failed")`, `return`},
		{"startup refusal omitted", "gateway-main.go", `logger.Error("BFF workload listener initialization failed")`, `return 0`},
		{"credential read", "edge.go", "o := edgeconfig.Default()", "_ = os.Getenv(\"SECRET\"); o := edgeconfig.Default()"},
		{"extra main work", "web-main.go", "os.Exit(run())", "config.Load(); os.Exit(run())"},
		{"other TLS site", "gateway-main.go", "srv.ListenAndServe()", `srv.ListenAndServeTLS("", "")`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range original {
				m[k] = v
			}
			if !strings.Contains(m[c.file], c.old) {
				t.Fatal("mutation missing")
			}
			m[c.file] = strings.Replace(m[c.file], c.old, c.new, 1)
			if checkPairedHTTPRecipes(m) == nil {
				t.Fatal("accepted mutation")
			}
		})
	}
}
