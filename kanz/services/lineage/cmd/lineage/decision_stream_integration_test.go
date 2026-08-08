package main

// AN AUTHORIZATION DECISION REACHES THE OBSERVATION STREAM, OVER A REAL BROKER (#352).
//
// Gated on TEST_NATS_URL (kanz/test/backing/up.sh).
//
// WHY A REAL BROKER AND NOT fakeBus. fakeBus does not validate envelopes, so it
// accepts what a real broker rejects — and the defect this test was written to
// catch is exactly an envelope a real broker rejects:
//
//	bus.Validate REQUIRES tenant_id on the live path. The producer resolves it as
//	explicit Event field > ctx > ProducerConfig.Tenant. A decision is raised by an
//	HTTP request to the read API, so there is no inbound delivery to put a tenant
//	in ctx — the mechanism services/compliance relies on, which is why compliance
//	publishes decisions with no fallback configured and is nonetheless fine.
//
// With no fallback on lineage's producer, EVERY decision publish fails
// validation. The error handler counts it, the log fills, and the service looks
// completely wired: authbus.NewBusRecorder is called, the recorder is injected,
// the counter exists. Only a real broker says no. A green fakeBus suite here
// would have shipped a feature with a 100% failure rate.
//
// It drives newBusRecorder rather than building a producer of its own, because
// the defect LIVES in that ProducerConfig — a test that constructed its own
// would have been green against a service that could not publish at all.

import (
	"context"
	"fmt"
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
	"github.com/eighred/kanz/services/lineage/internal/config"
	"github.com/eighred/kanz/services/lineage/internal/governance"
	"github.com/eighred/kanz/services/lineage/internal/graph"
)

// decisionSink collects DecisionLogs for one run. The observation stream is
// shared and retained, so it filters on this run's principal — without that,
// another run's residue could satisfy the assertion.
type decisionSink struct {
	subject string
	mu      sync.Mutex
	got     []*observationpb.DecisionLog
	tenants []string
}

func (d *decisionSink) handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	var entry observationpb.DecisionLog
	if err := proto.Unmarshal(payload, &entry); err != nil {
		return nil // not ours to interpret
	}
	if entry.GetAttributes()["principal.subject"] != d.subject {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, &entry)
	d.tenants = append(d.tenants, env.GetTenantId())
	return nil
}

func (d *decisionSink) snapshot() ([]*observationpb.DecisionLog, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*observationpb.DecisionLog(nil), d.got...), append([]string(nil), d.tenants...)
}

func TestAnAuthorizationDecisionReachesTheObservationStream(t *testing.T) {
	natsURL := os.Getenv("TEST_NATS_URL")
	if natsURL == "" {
		t.Skip("set TEST_NATS_URL (kanz/test/backing/up.sh) to drive AUTH-01d over a real broker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	principal := "user:auditor-" + suffix
	const dataset = "pii-ns.customers"

	admin, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	// THE ASSERTION NO UNIT TEST CAN MAKE: platform.authz.decision is bound to a
	// stream. On a provisioned topology this binds the real PLATFORM stream
	// (infra/nats/bootstrap-job.yaml: `ensure_stream PLATFORM "platform.>"`); on a
	// bare broker it creates a scratch one. An unbound subject is a HARD publish
	// failure, which is the whole audit trail going nowhere.
	bustest.EnsureSubjects(t, ctx, js, "LINEAGE_AUTHZ_IT_"+suffix,
		[]string{auth.AuthzDecisionEventType})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: natsURL, Name: "lineage-authz-it"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	sink := &decisionSink{subject: principal}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.Subscribe(subCtx, auth.AuthzDecisionEventType,
			"lineage-authz-it-"+suffix, sink.handle)
	}()

	// The service's OWN construction, with the service's OWN config defaults —
	// including the tenant fallback this test exists to hold in place.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Source = "lineage"
	cfg.GovernanceFile = piiConfig(t, dataset)

	rec, err := newBusRecorder(client, cfg, bus.NewBusMetrics(prometheus.NewRegistry()), discardLogger())
	if err != nil {
		t.Fatalf("newBusRecorder: %v", err)
	}
	gov, err := buildGovernor(cfg, discardLogger(), rec)
	if err != nil {
		t.Fatalf("buildGovernor: %v", err)
	}

	// A real access check on the PII surface: deny-by-default, since no policy
	// bundle is mounted. A DENY is the more important one to record — a refusal
	// nobody can reconstruct is worse than an allow nobody can.
	decision, sens := gov.CheckAccess(ctx,
		&auth.Principal{Subject: principal, Tenant: "acme"},
		graph.DatasetID{Namespace: "pii-ns", Name: "customers"}, "")

	// NON-VACUITY: a public dataset short-circuits before the authorizer, and no
	// decision would be recorded at all — this test would then be asserting on a
	// path it never took.
	if sens != governance.SensitivityPII {
		t.Fatalf("dataset classified as %v, want PII — CheckAccess returns before the "+
			"authorizer for a public dataset, so nothing would be recorded", sens)
	}
	if decision.Allow {
		t.Fatal("access allowed with no policy bundle — deny-by-default is not holding")
	}

	// Close drains the recorder's queue and waits for the publisher goroutine, so
	// after this the publish has either happened or failed.
	rec.Close()

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
			"The decision was made — CheckAccess returned a DENY above — and it did not reach the "+
			"observation stream. The likeliest cause is envelope validation: bus.Validate requires "+
			"tenant_id on the live path, and an authorization decision is raised by an HTTP request "+
			"with no inbound delivery to inherit one from. Check ProducerConfig.Tenant in "+
			"newBusRecorder and LINEAGE_TENANT in config.\n\n"+
			"This is the failure mode that looks fully wired from inside the process.",
			principal, auth.AuthzDecisionEventType)
	}

	entry := got[0]
	if v := entry.GetAttributes()["decision"]; v != "deny" {
		t.Errorf("recorded decision = %q, want deny — the stream disagrees with what the "+
			"authorizer actually returned, which makes the audit trail worse than none", v)
	}
	if entry.GetDecider() != "lineage" {
		t.Errorf("decider = %q, want lineage — AUDIT-01 selects authz decisions by decider",
			entry.GetDecider())
	}
	if entry.GetAttributes()["principal.tenant"] != "acme" {
		t.Errorf("principal.tenant = %q, want acme",
			entry.GetAttributes()["principal.tenant"])
	}
	// The ENVELOPE tenant is what internal/topic routes the archive by, and it is
	// the field that was empty and rejected before the fallback existed.
	if tenants[0] == "" {
		t.Error("the envelope carried an EMPTY tenant_id — it should have been rejected by " +
			"bus.Validate, so either validation regressed or this arrived off the replay path")
	}
}
