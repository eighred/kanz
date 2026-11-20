package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/platform/halt"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/api_gateway/internal/config"
	"github.com/eighred/kanz/services/api_gateway/internal/gateway"
	"github.com/eighred/kanz/services/api_gateway/internal/orders"
	"github.com/eighred/kanz/services/api_gateway/internal/proxy"
	blotter "github.com/eighred/kanz/services/oms/tools/synthetic_blotter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// TestPersistedNativeOrderBlotter can run independently against an existing
// disposable native receipt. The harness supplies only that order on stdin.
func TestPersistedNativeOrderBlotter(t *testing.T) {
	if os.Getenv("SYNTHETIC_BLOTTER_RECEIPT_STDIN") != "1" {
		t.Skip("requires a committed synthetic order in scoped PostgreSQL")
	}
	var receipt struct {
		Tenant, Portfolio string
		State             []byte
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 512<<10))
	if err != nil || len(raw) >= 512<<10 || json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("invalid synthetic receipt input")
	}
	state := new(orderpb.OrderState)
	if proto.Unmarshal(receipt.State, state) != nil {
		t.Fatal("invalid native state")
	}
	assertSyntheticOrderBlotter(t, os.Getenv("SYNTHETIC_OMS_READ_URL"), receipt.Tenant, receipt.Portfolio, state)
}

// The expected aggregate comes from the native commit receipt. Nothing in this
// helper seeds an order or consults a second writer to obtain the expectation.
func assertSyntheticOrderBlotter(t *testing.T, dsn, tenant, portfolio string, expected *orderpb.OrderState) {
	t.Helper()
	if expected.GetOrderId() == "" || expected.GetPortfolioId() != portfolio {
		t.Fatal("native state must name the expected portfolio and order")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	target, stop, err := blotter.Start(ctx, dsn, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal("cannot connect to disposable OMS read service")
	}
	defer func() { _ = conn.Close() }()
	id := newIdentityStub(t)
	id.feedUp.Store(true)
	authn, revocations := buildWired(t, id, &captureHandler{})
	if revocations.Refresh(ctx) != nil {
		t.Fatal("synthetic identity revocations unavailable")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{RequiredRole: "synthetic-reader"}
	h := gateway.New(nil, orderpb.NewOrderQueryServiceClient(conn), nil, "", logger)
	var ready atomic.Bool
	ready.Store(true)
	router, err := buildRouter(cfg, h, orders.New(nil, "", halt.OpenGate(nil)), proxy.New(&recordingBackend{}, proxyRoles(cfg)), nil, testObservability(t), &ready, logger, nil, authn, nil)
	if err != nil {
		t.Fatal("cannot compose gateway order read")
	}
	server := httptest.NewServer(router)
	defer server.Close()
	mint := func(owner, scope string) string {
		token, _, err := id.signer.Mint(&identity.User{Subject: "blotter-reader", Tenant: owner, Roles: []string{cfg.RequiredRole}, Portfolios: []string{scope}, Status: identity.StatusActive})
		if err != nil {
			t.Fatal("cannot mint synthetic scoped reader")
		}
		return token
	}
	allowed := mint(tenant, portfolio)
	foreign := mint(tenant+"-other", portfolio)
	get := func(token string) (int, []byte) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/portfolios/"+url.PathEscape(portfolio)+"/orders?limit=200", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal("synthetic gateway unavailable")
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil || len(body) >= 2<<20 {
			t.Fatal("invalid gateway response length")
		}
		return response.StatusCode, body
	}
	status, raw := get(allowed)
	page := new(orderpb.ListOrdersResponse)
	if status != http.StatusOK || protojson.Unmarshal(raw, page) != nil || page.GetOwnerTenant() != tenant {
		t.Fatalf("authorized gateway read did not return the tenant's order page (HTTP %d)", status)
	}
	match := 0
	for _, row := range page.GetOrders() {
		if row.GetPortfolioId() != portfolio {
			t.Fatal("foreign portfolio in returned page")
		}
		if row.GetOrderId() == expected.GetOrderId() {
			match++
			if !proto.Equal(row, expected) {
				t.Fatal("stored order returned through the gateway differs from native receipt")
			}
		}
	}
	if match != 1 {
		t.Fatalf("native order occurred %d times in gateway history", match)
	}
	foreignStatus, foreignBody := get(foreign)
	scopeStatus, scopeBody := get(mint(tenant, portfolio+"-excluded"))
	absentStatus, _ := get("invalid")
	if foreignStatus != 404 || scopeStatus != 404 || !bytes.Equal(foreignBody, scopeBody) || bytes.Contains(foreignBody, []byte(expected.GetOrderId())) || absentStatus != 401 {
		t.Fatal("gateway must hide the order from other tenants and portfolio scopes, and require authentication")
	}
	if probe := os.Getenv("SYNTHETIC_BLOTTER_BROWSER_PROBE"); probe != "" {
		// Only ephemeral test-session material crosses the child stdin. Do not
		// put tokens in process arguments, command output or retained files.
		wire, err := proto.Marshal(expected)
		if err != nil {
			t.Fatal("cannot encode native receipt for the browser")
		}
		input, err := json.Marshal(map[string]any{"gateway": server.URL, "tenant": tenant, "portfolio": portfolio,
			"authorized": allowed, "foreign": foreign, "state": wire})
		if err != nil {
			t.Fatal("cannot encode browser input")
		}
		cmd := exec.CommandContext(ctx, probe, "-test.run=^TestNativeSyntheticBlotterBrowser$", "-test.count=1", "-test.v")
		cmd.Env = []string{"SYNTHETIC_BLOTTER_PROBE=1", "TEST_MFA_NODE=" + os.Getenv("TEST_MFA_NODE"),
			"TEST_MFA_PLAYWRIGHT=" + os.Getenv("TEST_MFA_PLAYWRIGHT"), "TEST_MFA_STATIC_DIR=" + os.Getenv("TEST_MFA_STATIC_DIR"),
			"TEST_NATIVE_BLOTTER_SCRIPT=" + os.Getenv("TEST_NATIVE_BLOTTER_SCRIPT"), "PATH=" + os.Getenv("PATH"),
			"SystemRoot=" + os.Getenv("SystemRoot"), "LOCALAPPDATA=" + os.Getenv("LOCALAPPDATA"), "TEMP=" + os.Getenv("TEMP"),
			"TMP=" + os.Getenv("TMP"), "HOME=" + os.Getenv("HOME"), "PLAYWRIGHT_BROWSERS_PATH=" + os.Getenv("PLAYWRIGHT_BROWSERS_PATH")}
		cmd.Stdin = bytes.NewReader(input)
		output, err := cmd.Output()
		if err != nil || !bytes.Contains(output, []byte("native synthetic blotter browser passed")) {
			t.Fatal("native state failed actual BFF and Chromium verification")
		}
	}
}
