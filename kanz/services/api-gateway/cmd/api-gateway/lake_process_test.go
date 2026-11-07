package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/platform/halt"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/orders"
	"github.com/eighred/kanz/services/api-gateway/internal/proxy"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type lakeProcessSource struct {
	leaf   *x509svid.SVID
	bundle *x509bundle.Bundle
}

func (s *lakeProcessSource) GetX509SVID() (*x509svid.SVID, error) { return s.leaf, nil }
func (s *lakeProcessSource) GetX509BundleForTrustDomain(domain spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if domain != s.leaf.ID.TrustDomain() {
		return nil, errors.New("fixture domain mismatch")
	}
	return s.bundle, nil
}

type lakeProcessOutput struct{ bytes.Buffer }

func (b *lakeProcessOutput) Write(p []byte) (int, error) {
	if len(p) > 1<<20-b.Len() {
		return 0, errors.New("bounded fixture output exceeded")
	}
	return b.Buffer.Write(p)
}

// Invoked only as an owned test subprocess by the actual lake runtime proof.
// Credentials here are ephemeral synthetic fixture leaves/JWTs, never live
// Workload API input; stdout/stderr never contains their bytes.
func TestLakeGatewaySnapshotProcess(t *testing.T) {
	if os.Getenv("KANZ_LAKE_GATEWAY_CHILD") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var input struct {
		Address, CLI, From, Until string
		Cert, Key, Bundle         []byte
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		t.Fatal("fixture input exceeds bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
		t.Fatal("invalid fixture input")
	}
	host, _, err := net.SplitHostPort(input.Address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || !filepath.IsAbs(input.CLI) {
		t.Fatal("fixture must use owned loopback and absolute CLI")
	}
	leaf, err := x509svid.Parse(input.Cert, input.Key)
	if err != nil || leaf.ID.String() != "spiffe://kanz.internal/ns/kanz-services/sa/api-gateway" {
		t.Fatal("fixture delegation identity invalid")
	}
	bundle, err := x509bundle.Parse(leaf.ID.TrustDomain(), input.Bundle)
	if err != nil {
		t.Fatal("fixture trust bundle invalid")
	}
	source := &lakeProcessSource{leaf: leaf, bundle: bundle}
	conn, err := grpc.NewClient(input.Address, transport.ClientDialOption(source, transport.AuthorizeServices(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-services/sa/lake-sink"))), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20), grpc.MaxCallSendMsgSize(64<<10)))
	if err != nil {
		t.Fatal("typed gateway connection unavailable")
	}
	defer conn.Close()
	st := newIdentityStub(t)
	st.feedUp.Store(true)
	authn, revs := buildWired(t, st, &captureHandler{})
	if revs.Refresh(ctx) != nil {
		t.Fatal("actual revocation feed unavailable")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := gateway.New(nil, nil, nil, "", logger)
	if h.WithLakeSnapshots(lakepb.NewLakeSnapshotServiceClient(conn), "tenant-a") != nil {
		t.Fatal("gateway binding failed")
	}
	cfg := config.Config{RequiredRole: "fixture-base", TradeRole: "fixture-trader", OperatorRole: "fixture-operator", LakeSnapshot: &config.LakeSnapshot{Tenant: "tenant-a", CreateRole: "creator", ReadRole: "reader"}}
	var ready atomic.Bool
	ready.Store(true)
	router, err := buildRouter(cfg, h, orders.New(nil, "", halt.OpenGate(nil)), proxy.New(&recordingBackend{}, proxyRoles(cfg)), nil, testObservability(t), &ready, logger, nil, authn, nil)
	if err != nil {
		t.Fatal("actual gateway composition failed")
	}
	server := httptest.NewServer(router)
	defer server.Close()
	mint := func(tenant string, roles, portfolios []string) string {
		token, _, err := st.signer.Mint(&identity.User{Subject: "snapshot-operator", Tenant: tenant, Roles: roles, Portfolios: portfolios, Status: identity.StatusActive})
		if err != nil {
			t.Fatal("synthetic signed token unavailable")
		}
		return token
	}
	roles := []string{"fixture-base", "creator", "reader"}
	token := mint("tenant-a", roles, nil)
	cli := func(operation string, arguments []string, result proto.Message) {
		t.Helper()
		args := append([]string{"-gateway", server.URL, "-operation", operation}, arguments...)
		cmd := exec.CommandContext(ctx, input.CLI, args...)
		cmd.Env = []string{"KANZ_GATEWAY_TOKEN=" + token}
		cmd.WaitDelay = 5 * time.Second
		var output lakeProcessOutput
		cmd.Stdout = &output
		cmd.Stderr = io.Discard
		if cmd.Run() != nil || protojson.Unmarshal(output.Bytes(), result) != nil {
			t.Fatal("actual operator CLI round trip failed")
		}
	}
	created := new(lakepb.CreateSnapshotResponse)
	cli("create", []string{"-request-id", "request-gateway-cli", "-from", input.From, "-until", input.Until, "-event-types", "market.trade"}, created)
	if created.GetSnapshot().GetState() != lakepb.SnapshotState_SNAPSHOT_STATE_REQUESTED {
		t.Fatal("CLI create did not return initial acceptance")
	}
	id := created.Snapshot.SnapshotId
	get := new(lakepb.GetSnapshotResponse)
	for {
		cli("get", []string{"-snapshot-id", id}, get)
		if get.GetSnapshot().GetState() == lakepb.SnapshotState_SNAPSHOT_STATE_READY {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("CLI snapshot never ready")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if get.Snapshot.RowCount != 2 {
		t.Fatal("CLI tenant-pure count differs")
	}
	for _, operation := range []string{"list", "count", "failures"} {
		result := new(lakepb.QuerySnapshotResponse)
		cli(operation, []string{"-snapshot-id", id, "-page-size", "10"}, result)
		if result.GetResult() == nil || !result.Result.Exhausted {
			t.Fatal("CLI finite query failed")
		}
		if operation == "list" && len(result.Result.Events) != 2 {
			t.Fatal("CLI list omitted duplicate")
		}
		if operation == "count" && (len(result.Result.Counts) != 1 || result.Result.Counts[0].Count != 2) {
			t.Fatal("CLI count mismatch")
		}
	}
	// Signed claim controls traverse actual OIDC/JWKS, revocation and the same
	// production router. Forged principal headers never substitute for a token.
	for _, control := range []struct {
		token string
		want  int
	}{
		{"invalid", http.StatusUnauthorized},
		{mint("tenant-b", roles, nil), http.StatusNotFound},
		{mint("tenant-a", roles, []string{"restricted"}), http.StatusNotFound},
		{mint("tenant-a", []string{"fixture-base"}, nil), http.StatusForbidden},
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/lake/snapshots/"+id, nil)
		if err != nil {
			t.Fatal("request construction failed")
		}
		req.Header.Set("Authorization", "Bearer "+control.token)
		req.Header.Set("X-Kanz-Principal-Tenant", "tenant-a")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal("actual authorization control unavailable")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if response.StatusCode != control.want {
			t.Fatal("signed authorization control was bypassed")
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/lake/snapshots", strings.NewReader(`{"caller":{"subject":"forged","tenant":"tenant-b"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal("caller injection control unavailable")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatal("external DTO accepted delegated caller")
	}
	// The authenticated subject was forwarded by the gateway, not accepted from
	// any CLI identity option or inbound X-Kanz-Principal header.
}
