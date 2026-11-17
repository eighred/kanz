//go:build linux && state_read_integration

package grpcsrv_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	risk "github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/risk_engine/internal/grpcsrv"
)

// Run in a disposable loopback namespace with a fresh canonical broker and
// the gateway and MCP binaries built from the tested revision.
func TestAuthorizedGatewayStateReadsAndBrokerDecisions(t *testing.T) {
	const tenant, portfolio = "tenant_a", "PF_A"
	const brokerURL = "nats://127.0.0.1:4222"
	if os.Getenv("TEST_NATS_URL") != brokerURL {
		t.Fatal("a fresh loopback JetStream fixture is required")
	}
	client, err := bus.DialNATS(t.Context(), bus.NATSConfig{URL: brokerURL, Name: "read-proof-observer"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	authority, err := testidentity.NewAuthority(spiffeid.RequireTrustDomainFromString("kanz.internal"))
	if err != nil {
		t.Fatal(err)
	}
	issue := func(service string) *testidentity.Source {
		id, err := transport.ServiceID("kanz-services", service)
		if err != nil {
			t.Fatal(err)
		}
		source, err := authority.Issue(id, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	mcpSource := issue("mcp")
	_, socket := testidentity.StartWorkload(t, mcpSource)
	store := state.NewStore()
	stamp := time.Now().UTC().Add(-time.Second)
	book := domain.NewPortfolio(portfolio, "USD")
	book.SetAggregate(domain.AggregateUpdate{AsOf: stamp})
	book.SetPosition(domain.Position{InstrumentID: "FIXTURE", Quantity: &commonpb.Decimal{Coefficient: 9007199254740993, Exponent: -11}, AsOf: stamp})
	if err := store.Restore(book, nil); err != nil {
		t.Fatal(err)
	}
	riskListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(transport.ServerOption(issue("risk-engine"), transport.AuthorizeMesh()))
	grpcsrv.New(engine.New(store, compute.DefaultRegistry(), risk.NewCache(), risk.NewDetector()), tenant).Register(server)
	joined := make(chan struct{})
	go func() { defer close(joined); _ = server.Serve(riskListener) }()
	t.Cleanup(func() { server.Stop(); <-joined })

	_, omsSocket := testidentity.StartWorkload(t, issue("oms"))
	oms := startRecordedState(t, omsSocket)
	policy := filepath.Join(t.TempDir(), "read-policy.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"reader":["risk.read","order.read"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	mcp := localAddress(t)
	startReadProcess(t, "TEST_MCP_BINARY", mcp, []string{
		"MCP_LISTEN=" + mcp, "MCP_METRICS_LISTEN=" + localAddress(t),
		"MCP_POLICY_FILE=" + policy, "MCP_STATE_READS_ENABLED=true", "MCP_TENANT=" + tenant,
		"SPIFFE_ENDPOINT_SOCKET=" + socket, "MCP_RISK_QUERY_ADDR=" + riskListener.Addr().String(),
		"MCP_ORDER_STATE_QUERY_ADDR=" + oms,
	})
	issuer := newReadIssuer(t)
	gateway := localAddress(t)
	startReadProcess(t, "TEST_GATEWAY_BINARY", gateway, []string{
		"API_GATEWAY_LISTEN=" + gateway, "API_GATEWAY_RISK_ENGINE_ADDR=" + riskListener.Addr().String(),
		"API_GATEWAY_MCP_ADDR=http://" + mcp, "API_GATEWAY_NATS_URL=" + brokerURL,
		"API_GATEWAY_OIDC_ISSUER=" + issuer.server.URL, "API_GATEWAY_OIDC_AUDIENCE=read-proof",
		"API_GATEWAY_OIDC_JWKS_URI=" + issuer.server.URL + "/jwks", "API_GATEWAY_REVOCATIONS_URI=" + issuer.server.URL + "/revocations",
		"API_GATEWAY_REQUIRED_ROLE=reader", "API_GATEWAY_TRADE_ROLE=unassigned.trade",
	})
	var page struct {
		PortfolioID string `json:"portfolio_id"`
		Scope       string
		Positions   []struct {
			InstrumentID string `json:"instrument_id"`
			Quantity     struct {
				Status string
				Value  struct {
					Coefficient string
					Exponent    int32
				}
			}
		}
	}
	const positionPrincipal = "agent:position-reader"
	readProjection(t, gateway, issuer.token(t, positionPrincipal, tenant, []string{"reader"}, []string{portfolio}),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_positions","arguments":{"portfolio_id":"PF_A"}}}`, &page)
	if page.PortfolioID != portfolio || page.Scope != "OBSERVED_RISK_STATE" || len(page.Positions) != 1 {
		t.Fatal("position projection does not identify the requested portfolio and observed scope")
	}
	position := page.Positions[0]
	if position.InstrumentID != "FIXTURE" || position.Quantity.Status != "SUPPORTED" || position.Quantity.Value.Coefficient != "9007199254740993" || position.Quantity.Value.Exponent != -11 {
		t.Fatal("position projection changed the instrument or exact observed quantity")
	}

	posSeq := recordedRead(t, client, tenant, positionPrincipal, 0)
	var orderState struct {
		PortfolioID    string `json:"portfolio_id"`
		OrderID        string `json:"order_id"`
		InstrumentID   string `json:"instrument_id"`
		RecordedStatus string `json:"recorded_status"`
		Scope          string
		Status         string
		Ordered        struct {
			Status string
			Value  struct {
				Coefficient string
				Exponent    int32
			}
		} `json:"ordered_quantity"`
	}
	const orderPrincipal = "agent:order-reader"
	readProjection(t, gateway, issuer.token(t, orderPrincipal, tenant, []string{"reader"}, []string{portfolio}),
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_order_state","arguments":{"portfolio_id":"PF_A","order_id":"recorded-order"}}}`, &orderState)
	if orderState.PortfolioID != portfolio || orderState.OrderID != "recorded-order" || orderState.InstrumentID != "FIXTURE" || orderState.Scope != "RECORDED_OMS_STATE" || orderState.Status != "SUPPORTED" || orderState.RecordedStatus != "ORDER_STATUS_ROUTED" || orderState.Ordered.Status != "SUPPORTED" || orderState.Ordered.Value.Coefficient != "9007199254740995" || orderState.Ordered.Value.Exponent != -8 {
		t.Fatal("recorded order projection changed the stored identity, lifecycle or exact quantity")
	}
	recordedRead(t, client, tenant, orderPrincipal, posSeq)
}
func readProjection(t *testing.T, gateway, token, call string, result any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+gateway+"/v1/mcp", strings.NewReader(call))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply struct {
		Error  json.RawMessage
		Result struct {
			IsError bool
			Content []struct{ Text string }
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&reply); err != nil || resp.StatusCode != 200 || reply.Error != nil || reply.Result.IsError || len(reply.Result.Content) != 1 {
		t.Fatalf("authorized MCP read failed, HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal([]byte(reply.Result.Content[0].Text), result); err != nil {
		t.Fatal(err)
	}
}
func recordedRead(t *testing.T, client *bus.NATSClient, tenant, principal string, previousSeq uint64) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		env, payload, seq, err := client.LastOnSubject(ctx, auth.AuthzDecisionEventType)
		if err == nil && seq > previousSeq {
			var decision observationpb.DecisionLog
			if err := proto.Unmarshal(payload, &decision); err != nil {
				t.Fatal(err)
			}
			a := decision.GetAttributes()
			if decision.GetDecider() != "authz:api-gateway" || a["action"] != "read" || env.GetSource() != "api-gateway" || env.GetTenantId() != tenant || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_OBSERVATION || env.GetEventType() != auth.AuthzDecisionEventType || env.GetPayloadSchemaRef() != "observation.v1.DecisionLog:1" || a["principal.subject"] != principal || a["principal.tenant"] != tenant || a["resource.tenant"] != tenant || a["resource.id"] != "POST /v1/mcp" || a["decision"] != "allow" {
				t.Fatal("retained decision does not match the authenticated gateway read")
			}
			return seq
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no matching gateway decision was retained")
	return 0
}

func localAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func startReadProcess(t *testing.T, key, address string, env []string) {
	t.Helper()
	binary := os.Getenv(key)
	if !filepath.IsAbs(binary) {
		t.Fatalf("%s must select the tested binary", key)
	}
	cmd := exec.Command(binary)
	cmd.Env = env
	done := runReadProcess(t, key, cmd)
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-done:
			t.Fatalf("%s exited before readiness: %v", key, err)
		default:
		}
		resp, err := probe.Get("http://" + address + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready", key)
}
func runReadProcess(t *testing.T, key string, cmd *exec.Cmd) <-chan error {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s did not shut down cleanly", key)
			}
		case <-time.After(12 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Errorf("%s shutdown exceeded budget", key)
		}
	})
	return done
}

func startRecordedState(t *testing.T, socket string) string {
	t.Helper()
	const key = "TEST_OMS_STATE_FIXTURE_BINARY"
	binary := os.Getenv(key)
	if !filepath.IsAbs(binary) {
		t.Fatalf("%s must select the tested binary", key)
	}
	addr := localAddress(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cmd := exec.Command(binary, "-test.run", "^TestServeRecordedStateForGatewayRead$", "-test.timeout", "60s")
	cmd.Dir = filepath.Join("..", "..", "..", "oms", "cmd", "oms")
	cmd.Env = []string{"TEST_OMS_READ_CHILD=1", "TEST_OMS_READ_SOCKET=" + socket, "TEST_OMS_READ_LISTEN=" + addr, "TEST_POSTGRES_URL=" + os.Getenv("TEST_POSTGRES_URL")}
	cmd.ExtraFiles = []*os.File{writer}
	runReadProcess(t, key, cmd)
	writer.Close()
	if err := reader.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ready, err := bufio.NewReader(io.LimitReader(reader, 16)).ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatal("recorded state service did not complete startup")
	}
	return addr
}
