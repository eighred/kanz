package main

// A GATEWAY CAPABILITY DECISION REACHES THE OBSERVATION STREAM (#352).
//
// Gated on TEST_NATS_URL (kanz/test/backing/up.sh).
//
// WHY THIS EXISTS SEPARATELY FROM THE authz PACKAGE TESTS. Those prove the mux
// hands a DecisionLog to its recorder. They cannot prove the thing that actually
// breaks in production, because it is not in that package:
//
//  1. THE PRODUCER IS SHARED WITH THE ORDER PATH, and that producer carries
//     VerifyCommandIssuer (the AUTH-01c forged-issuer guard). The guard is
//     documented as COMMAND-only, so an OBSERVATION should pass untouched — but
//     "should" is what this test replaces. If it ever ran on a DecisionLog,
//     every decision publish would be refused and the audit trail would be
//     empty while every order still worked.
//
//  2. THE ENVELOPE MUST VALIDATE. bus.Validate requires tenant_id on the live
//     path, and a gateway decision is raised by an inbound HTTP request with no
//     delivery to inherit one from. The gateway deliberately has NO
//     ProducerConfig.Tenant — a fallback there would let a token with no tenant
//     claim publish an order into a default tenant's book — so the tenant comes
//     from the recorder alone.
//
// fakeBus would prove neither: it does not validate envelopes.
//
// It drives buildBus and buildDecisionRecorder rather than assembling a producer
// of its own, because both defects above live in that construction.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

type streamSink struct {
	subject string
	mu      sync.Mutex
	got     []*observationpb.DecisionLog
	tenants []string
}

func (s *streamSink) handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	var entry observationpb.DecisionLog
	if err := proto.Unmarshal(payload, &entry); err != nil {
		return nil
	}
	if entry.GetAttributes()["principal.subject"] != s.subject {
		return nil // another run's residue on a shared, retained stream
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, &entry)
	s.tenants = append(s.tenants, env.GetTenantId())
	return nil
}

func (s *streamSink) snapshot() ([]*observationpb.DecisionLog, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*observationpb.DecisionLog(nil), s.got...), append([]string(nil), s.tenants...)
}

func TestAGatewayDecisionReachesTheObservationStream(t *testing.T) {
	natsURL := os.Getenv("TEST_NATS_URL")
	if natsURL == "" {
		t.Skip("set TEST_NATS_URL (kanz/test/backing/up.sh) to drive AUTH-01d over a real broker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	principal := "user:trader-" + suffix
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	admin, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	// platform.authz.decision must be carried by a stream, or the publish is a
	// hard failure. infra/nats/tenancy.yaml must ALSO grant the gateway publish
	// on it — that grant cannot be asserted from here (this broker has no
	// accounts), which is why it is called out in the tenancy entry itself.
	bustest.EnsureSubjects(t, ctx, js, "GW_AUTHZ_IT_"+suffix, []string{auth.AuthzDecisionEventType})

	cfg := config.Config{
		Source:  "api-gateway",
		NATSURL: natsURL,
	}
	producer, _, closeBus, err := buildBus(ctx, cfg, halt.NewGate(nil), logger)
	if err != nil {
		t.Fatalf("buildBus: %v", err)
	}
	t.Cleanup(closeBus)
	if producer == nil {
		t.Fatal("buildBus returned no producer with a NATS URL set")
	}

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: natsURL, Name: "gw-authz-it"})
	if err != nil {
		t.Fatalf("dial (consumer): %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sink := &streamSink{subject: principal}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.Subscribe(subCtx, auth.AuthzDecisionEventType, "gw-authz-it-"+suffix, sink.handle)
	}()

	// A fresh registry: buildDecisionRecorder registers the loss counter, and the
	// default registry would reject a second registration in this binary.
	recorder, closeRecorder := buildDecisionRecorder(producer, prometheus.NewRegistry(), logger)

	// The real mux, on a route with a CAPITAL effect, denied — the decision most
	// worth being able to reconstruct.
	m := authz.NewMux(authz.Grants{"kanz-user": {authz.Read}}, recorder)
	m.Handle(authz.Trade, "POST /v1/orders", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`{}`))
	req = req.WithContext(middleware.WithPrincipal(req.Context(), &middleware.Principal{
		Subject: principal, Tenant: "acme", Roles: []string{"kanz-user"},
	}))
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — this test is asserting on the wrong path", rr.Code)
	}

	closeRecorder() // drains the queue and waits for the publisher

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := sink.snapshot(); len(got) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	got, tenants := sink.snapshot()
	if len(got) == 0 {
		t.Fatalf("no DecisionLog for %s reached %s within 30s.\n\n"+
			"The gateway refused a trade and the refusal did not reach the observation stream. Two "+
			"causes are specific to THIS composition root: the shared producer's VerifyCommandIssuer "+
			"running on a non-COMMAND event, or an envelope rejected for a missing tenant_id — the "+
			"gateway has no ProducerConfig.Tenant on purpose, so the recorder's own stamp is the only "+
			"source of one.", principal, auth.AuthzDecisionEventType)
	}

	entry := got[0]
	attrs := entry.GetAttributes()
	if attrs["decision"] != "deny" {
		t.Errorf("decision = %q, want deny — the stream disagrees with what the gateway did", attrs["decision"])
	}
	if entry.GetDecider() != "authz:api-gateway" {
		t.Errorf("decider = %q, want authz:api-gateway — AUDIT-01 selects authz records by it",
			entry.GetDecider())
	}
	if attrs["resource.id"] != "POST /v1/orders" {
		t.Errorf("resource.id = %q, want the route pattern", attrs["resource.id"])
	}
	// The DECIDING PRINCIPAL's tenant, not the platform's: the gateway is
	// multi-tenant, and filing acme's decisions under __system__ would be valid,
	// wrong, and undetectable.
	if tenants[0] != "acme" {
		t.Errorf("envelope tenant_id = %q, want acme — the fallback won over the caller's own tenant",
			tenants[0])
	}
}
