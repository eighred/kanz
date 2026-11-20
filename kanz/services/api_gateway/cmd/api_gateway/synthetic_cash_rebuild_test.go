package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/testidentity"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/api_gateway/internal/config"
	"github.com/eighred/kanz/services/api_gateway/internal/proxy"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/protobuf/proto"
)

func syntheticHistoryUpstream(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Getenv("SYNTHETIC_BOOK_BIN"), "-test.run=^TestSyntheticCashHistoryServer$", "-test.count=1")
	cmd.Env = []string{"SYNTHETIC_BOOK_HISTORY_URL=" + os.Getenv("SYNTHETIC_BOOK_HISTORY_URL")}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		_ = input.Close()
		if err := cmd.Wait(); err != nil {
			t.Error("history process", err)
		}
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		stop()
		t.Fatal("accounting history startup", err)
	}
	addr, err := url.Parse(strings.TrimSpace(line))
	if err != nil || addr.Scheme != "http" || addr.Hostname() != "127.0.0.1" || addr.Port() == "" || addr.Path != "" || addr.User != nil {
		stop()
		t.Fatal("unexpected history endpoint")
	}
	return addr.String(), stop
}

func recoverLegacyCashClock(t *testing.T, ctx context.Context, tenant string, frame *envelopepb.EventFrame) {
	t.Helper()
	const portfolio = "synthetic-portfolio"
	ctx = bus.WithTenantID(ctx, tenant)
	var cash accountingpb.PortfolioCashBalance
	if tenant != "synthetic-cash-clock" || proto.Unmarshal(frame.Payload, &cash) != nil || cash.GetAsOf().AsTime().Sub(frame.GetEnvelope().GetEventTime().AsTime()) != 321*time.Nanosecond {
		t.Fatal("expected the original 7077c75b truncated envelope")
	}
	original := syntheticPool(t, ctx, "CASH", tenant)
	apply := syntheticvenue.CapitalCashHandler(original, tenant)
	if err := apply(ctx, frame.Envelope, frame.Payload); err == nil {
		t.Fatal("historical contradictory frame was accepted")
	}
	var fault []byte
	if err := original.QueryRow(ctx, "SELECT fault_digest FROM capital_source_health WHERE tenant_id=$1", tenant).Scan(&fault); err != nil || len(fault) != 32 {
		t.Fatal("missing durable quarantine", err)
	}

	raw := os.Getenv("SYNTHETIC_REBUILD_URL")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgres" || u.User.Username() != "kanz_synthetic_oms_cash" || u.Host != "127.0.0.1:5432" || u.Path != "/kanz_synthetic_cash_rebuild" || u.RawQuery != "sslmode=disable" {
		t.Fatal("expected a separate disposable cash projection")
	}
	fresh, err := pg.NewTenantPool(ctx, raw, tenant)
	if err != nil {
		t.Fatal("open rebuild projection")
	}
	defer fresh.Close()
	var empty bool
	if err := fresh.QueryRow(ctx, "SELECT NOT EXISTS(SELECT FROM capital_source_health) AND NOT EXISTS(SELECT FROM capital_balances)").Scan(&empty); err != nil || !empty {
		t.Fatal("replacement projection must start empty", err)
	}

	upstream, stop := syntheticHistoryUpstream(t, ctx)
	defer stop()
	backend := proxy.NewMeshBackend(map[proxy.Service]string{proxy.ServiceAccounting: upstream}, &http.Client{Timeout: 5 * time.Second})
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{RequiredRole: baselineRole, TradeRole: traderRole, CashHistoryRole: "oms-history", CashHistorySigningSecret: hex.EncodeToString(key), SigningSecret: "synthetic-public-key", MFARequired: true}
	issuer := newIdentityStub(t)
	issuer.feedUp.Store(true)
	authn, revocations := buildWired(t, issuer, &captureHandler{})
	if err := revocations.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	token, _, err := issuer.signer.Mint(&identity.User{Subject: "workload:oms", Tenant: tenant, Roles: []string{cfg.CashHistoryRole}, Portfolios: []string{portfolio}, Status: identity.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	router, err := buildRouterWithBackend(t, cfg, authn, backend)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := testidentity.NewAuthority(spiffeid.RequireTrustDomainFromString(transport.TrustDomain))
	if err != nil {
		t.Fatal(err)
	}
	gatewayID, err := transport.ServiceID("kanz-services", "api-gateway")
	if err != nil {
		t.Fatal(err)
	}
	omsID, err := transport.ServiceID("kanz-services", "oms")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	gatewaySource, err := ca.Issue(gatewayID, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	omsSource, err := ca.Issue(omsID, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newOMSHistoryServer("127.0.0.1:0", router, gatewaySource)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(ln, srv.TLSConfig)) }()
	defer func() {
		_ = srv.Close()
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: transport.ClientTLSConfig(omsSource, transport.AuthorizeServices(gatewayID))}}
	defer client.CloseIdleConnections()
	for range 2 {
		if err := syntheticvenue.RecoverOpeningCash(ctx, fresh, "https://"+ln.Addr().String(), client, token, cfg.CashHistorySigningSecret, portfolio, frame.Payload); err != nil {
			t.Fatal("recover original history", err)
		}
	}
	var healthy bool
	if err := fresh.QueryRow(ctx, `SELECT revision=1 AND total='1000' AND reserved='0' AND observed_at_ns=$2 AND NOT conflicted
		AND EXISTS(SELECT FROM capital_source_health WHERE fault_digest IS NULL)
		AND (SELECT count(*) FROM capital_cash_events)=1 FROM capital_balances WHERE portfolio_id=$1 AND currency='USD'`, portfolio, cash.GetAsOf().AsTime().UnixNano()).Scan(&healthy); err != nil || !healthy {
		t.Fatal("retained prefix differs from original cash", err)
	}
	var after []byte
	if err := original.QueryRow(ctx, "SELECT fault_digest FROM capital_source_health WHERE tenant_id=$1", tenant).Scan(&after); err != nil || !bytes.Equal(after, fault) {
		t.Fatal("original evidence changed", err)
	}
	t.Log("XSIM legacy cash rebuild original_quarantined=true recovered_USD=1000 original_cutoff_preserved=true revisions=1")
}
