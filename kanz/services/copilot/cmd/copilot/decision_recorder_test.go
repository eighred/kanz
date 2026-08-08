package main

// THE COPILOT'S TOOL AUTHORIZATIONS REACH THE OBSERVATION STREAM (#352).
//
// This was the last of the issue's three sites, and the only one that had no bus
// at all: no NATS in its config, no SVID in tenancy.yaml, no producer anywhere.
// Its decisions went to slog, which means a tool authorization — the record of an
// AI agent being allowed, or refused, access to a portfolio — existed only in a
// pod's stdout and was gone at the next rollout.
//
// The real-broker test is gated on TEST_NATS_URL. It drives buildDecisionRecorder
// rather than assembling a producer, because the failure this service is most
// exposed to lives in that construction: bus.Validate REQUIRES tenant_id on the
// live path, and a tool authorization is raised by an HTTP request with no
// inbound delivery to inherit one from. fakeBus would not catch it — it does not
// validate envelopes.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/copilot/internal/config"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// WITH NO COPILOT_NATS_URL THE SERVICE STILL RECORDS, AND STILL STARTS.
//
// The degradation is the property most likely to be broken by this change: a
// deployment that runs the copilot without a broker must not start failing, and
// must not silently record nothing either. It gets the slog recorder — which is
// exactly the behaviour that predates #352, so this cannot regress an existing
// deployment.
func TestWithNoBusTheRecorderFallsBackToTheLog(t *testing.T) {
	rec, closeRec, err := buildDecisionRecorder(context.Background(), config.Config{},
		prometheus.NewRegistry(), quietLogger())
	if err != nil {
		t.Fatalf("buildDecisionRecorder with no NATS URL: %v\n\n"+
			"A copilot deployed without a broker must still start. Refusing here would turn an "+
			"audit downgrade into an outage.", err)
	}
	if rec == nil {
		t.Fatal("no recorder returned — decisions would reach nowhere at all, which is worse " +
			"than the slog fallback this replaced")
	}
	if closeRec == nil {
		t.Fatal("no shutdown func returned")
	}
	closeRec()

	// It must be a REAL recorder, not a no-op: Record has to accept an entry.
	if err := rec.Record(context.Background(), &observationpb.DecisionLog{Decider: "copilot"}); err != nil {
		t.Fatalf("the fallback recorder refused an entry: %v", err)
	}
}

type copilotSink struct {
	subject string
	mu      sync.Mutex
	got     []*observationpb.DecisionLog
	tenants []string
}

func (c *copilotSink) handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	var entry observationpb.DecisionLog
	if err := proto.Unmarshal(payload, &entry); err != nil {
		return nil
	}
	if entry.GetAttributes()["principal.subject"] != c.subject {
		return nil // another run's residue on a shared, retained stream
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, &entry)
	c.tenants = append(c.tenants, env.GetTenantId())
	return nil
}

func (c *copilotSink) snapshot() ([]*observationpb.DecisionLog, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*observationpb.DecisionLog(nil), c.got...), append([]string(nil), c.tenants...)
}

func TestACopilotToolAuthorizationReachesTheObservationStream(t *testing.T) {
	natsURL := os.Getenv("TEST_NATS_URL")
	if natsURL == "" {
		t.Skip("set TEST_NATS_URL (kanz/test/backing/up.sh) to drive AUTH-01d over a real broker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	principal := "user:analyst-" + suffix

	admin, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	bustest.EnsureSubjects(t, ctx, js, "COPILOT_AUTHZ_IT_"+suffix, []string{auth.AuthzDecisionEventType})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: natsURL, Name: "copilot-authz-it"})
	if err != nil {
		t.Fatalf("dial (consumer): %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sink := &copilotSink{subject: principal}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.Subscribe(subCtx, auth.AuthzDecisionEventType, "copilot-authz-it-"+suffix, sink.handle)
	}()

	// The service's OWN construction, with the service's OWN config defaults —
	// including the fallback tenant, which is the piece a hand-built producer
	// would have quietly supplied differently.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.NATSURL = natsURL
	cfg.SPIFFESocket = "" // plaintext against the dev broker

	rec, closeRec, err := buildDecisionRecorder(ctx, cfg, prometheus.NewRegistry(), quietLogger())
	if err != nil {
		t.Fatalf("buildDecisionRecorder: %v", err)
	}

	// A DENY through the real audited authorizer, on the deny-by-default gate this
	// service ships with (no policy bundle mounted). A refusal nobody can
	// reconstruct is worse than an allow nobody can, which is why AUTH-01d records
	// both — and it is the verdict an investigation asks about first.
	authorizer := auth.NewAuditedAuthorizer(denyAll{}, rec, "copilot", quietLogger())
	decision := authorizer.Authorize(ctx, auth.Request{
		Principal: &auth.Principal{Subject: principal, Tenant: "acme"},
		Action:    auth.ActionRiskRead,
		Resource:  auth.Resource{Type: auth.ResourcePortfolio, ID: "p-" + suffix, Tenant: "acme"},
	})
	if decision.Allow {
		t.Fatal("the deny-all authorizer allowed — this test is asserting on the wrong path")
	}
	closeRec() // drains the queue, waits for the publisher, then closes the connection

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
			"The copilot refused a tool call and the refusal did not reach the observation stream. "+
			"The likeliest cause is envelope validation: bus.Validate requires tenant_id on the live "+
			"path, and a tool authorization is raised by an HTTP request with no inbound delivery to "+
			"inherit one from. Check that pkg/authbus still stamps Event.TenantID.",
			principal, auth.AuthzDecisionEventType)
	}

	entry := got[0]
	if v := entry.GetAttributes()["decision"]; v != "deny" {
		t.Errorf("recorded decision = %q, want deny — the stream disagrees with what the "+
			"authorizer returned", v)
	}
	if entry.GetDecider() != "copilot" {
		t.Errorf("decider = %q, want copilot — AUDIT-01 selects authz decisions by decider",
			entry.GetDecider())
	}
	// The DECIDING PRINCIPAL's tenant, not the deployment's. One copilot pod
	// answers for many tenants, so a per-service default would misfile all but one.
	if tenants[0] != "acme" {
		t.Errorf("envelope tenant_id = %q, want acme — the fallback won over the caller's own "+
			"tenant, which would file every tenant's authorizations under one", tenants[0])
	}
}
