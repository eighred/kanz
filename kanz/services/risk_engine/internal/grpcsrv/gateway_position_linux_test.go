//go:build linux && state_read_integration

package grpcsrv_test

import (
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
func TestAuthorizedGatewayPositionReadAndBrokerDecision(t *testing.T) {
	const tenant, portfolio, principal = "tenant_a", "PF_A", "agent:read-proof"
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

	policy := filepath.Join(t.TempDir(), "read-policy.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"reader":["risk.read"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	mcp := localAddress(t)
	startReadProcess(t, "TEST_MCP_BINARY", mcp, []string{
		"MCP_LISTEN=" + mcp, "MCP_METRICS_LISTEN=" + localAddress(t),
		"MCP_POLICY_FILE=" + policy, "MCP_STATE_READS_ENABLED=true", "MCP_TENANT=" + tenant,
		"SPIFFE_ENDPOINT_SOCKET=" + socket, "MCP_RISK_QUERY_ADDR=" + riskListener.Addr().String(),
		// This scenario selects only get_positions. No order endpoint is running.
		"MCP_ORDER_STATE_QUERY_ADDR=" + localAddress(t),
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
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_positions","arguments":{"portfolio_id":"PF_A"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+gateway+"/v1/mcp", strings.NewReader(call))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+issuer.token(t, principal, tenant, []string{"reader"}, []string{portfolio}))
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
	if err := json.Unmarshal([]byte(reply.Result.Content[0].Text), &page); err != nil || page.PortfolioID != portfolio || page.Scope != "OBSERVED_RISK_STATE" || len(page.Positions) != 1 {
		t.Fatal("position projection does not identify the requested portfolio and observed scope")
	}
	position := page.Positions[0]
	if position.InstrumentID != "FIXTURE" || position.Quantity.Status != "SUPPORTED" || position.Quantity.Value.Coefficient != "9007199254740993" || position.Quantity.Value.Exponent != -11 {
		t.Fatal("position projection changed the instrument or exact observed quantity")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		env, payload, seq, err := client.LastOnSubject(ctx, auth.AuthzDecisionEventType)
		if err == nil && seq > 0 {
			var decision observationpb.DecisionLog
			if err := proto.Unmarshal(payload, &decision); err != nil {
				t.Fatal(err)
			}
			a := decision.GetAttributes()
			if env.GetTenantId() != tenant || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_OBSERVATION || env.GetEventType() != auth.AuthzDecisionEventType || env.GetPayloadSchemaRef() != "observation.v1.DecisionLog:1" || a["principal.subject"] != principal || a["principal.tenant"] != tenant || a["resource.tenant"] != tenant || a["resource.id"] != "POST /v1/mcp" || a["decision"] != "allow" {
				t.Fatal("retained decision does not match the authenticated gateway read")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no matching gateway decision was retained")
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
