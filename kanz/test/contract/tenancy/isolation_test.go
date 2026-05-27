// Package tenancy_test is the MT-01g cross-tenant isolation contract tree: the
// capstone proof that the tenant boundary holds end-to-end, across every layer
// the MT epic built. It asserts, as one suite, that an event or query scoped to
// tenant A is provably never visible to tenant B across BUS + STORE + API, that
// an RLS-escape attempt fails, and that replay of legacy untenanted events lands
// in the reserved __system__ tenant rather than leaking into a real one.
//
// Each layer's mechanism lives elsewhere (bus stamping/propagation MT-01a/b,
// broker isolation MT-01c, RLS MT-01d, gateway authz AUTH-01b); this tree is the
// integration contract that pins their COMPOSITION — a regression in any single
// layer that opened a cross-tenant path surfaces here. Sits in an external
// `_test` package so it consumes only the public bus/auth/persist surface, like
// the other contract trees (envelope, replay, serialization).
//
// The store layer needs a real database and is gated on TEST_POSTGRES_URL (it
// skips otherwise, mirroring the persist + bus/Kafka integration tests); the
// bus, replay, and API layers are pure and run on every `go test`.
package tenancy_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/state/persist"
	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/pkg/bus"
)

const (
	tenantA = "acme"
	tenantB = "globex"
)

// ============================ BUS layer ==================================

// A live FACT is always stamped with its producing tenant — the provenance the
// broker isolation (NATS account / Kafka ACL, MT-01c) keys on. Without a faithful
// tenant_id the broker layer can't keep A's stream from B's.
func TestBus_PublishStampsProducingTenant(t *testing.T) {
	cc := &captureClient{}
	p, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "svc/1", ProducerVersion: "v", Tenant: tenantA})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if env.TenantId != tenantA {
		t.Fatalf("published FACT tenant=%q want %q", env.TenantId, tenantA)
	}
}

// An untenanted event cannot enter the LIVE stream: Validate (live path) rejects
// it, so a tenant-less event can never sit on a real tenant's topic and leak.
// The replay path stays tolerant (legacy logs) — proven below.
func TestBus_LiveRequiresTenant(t *testing.T) {
	env := validEnvelope("")
	if err := bus.Validate(env); err == nil {
		t.Fatal("live Validate accepted an untenanted event — it could leak across the boundary")
	}
	// The replay path tolerates a missing tenant (legacy logs); it still requires
	// the REPLAYED flag that marks an event as off the replay stream.
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	if err := bus.ValidateReplay(env); err != nil {
		t.Fatalf("replay Validate rejected a legacy untenanted event: %v", err)
	}
}

// A derived event published inside a handler INHERITS the inbound tenant from
// ctx (MT-01b) — so processing tenant A's event can never silently emit a
// derived FACT under another tenant (or the engine's config fallback).
func TestBus_DerivedEventInheritsTenant(t *testing.T) {
	inbound := frame(t, validEnvelope(tenantB), []byte("p"))
	c, err := bus.NewConsumer(&oneShotSub{msg: bus.Message{Subject: "x", Body: inbound}})
	if err != nil {
		t.Fatal(err)
	}

	// A producer whose CONFIG tenant differs from the inbound one — to prove ctx
	// (the inbound tenant) wins, not the config fallback.
	cc := &captureClient{}
	derived, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "svc/1", ProducerVersion: "v", Tenant: tenantA})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Subscribe(context.Background(), "x", "g", func(ctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
		return derived.Publish(ctx, factEvent()) // no explicit tenant ⇒ inherits ctx
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if env.TenantId != tenantB {
		t.Fatalf("derived FACT tenant=%q want inherited %q (config %q must not win)", env.TenantId, tenantB, tenantA)
	}
}

// ============================ REPLAY layer ===============================

// Legacy untenanted events replayed from an old durable log map to the reserved
// __system__ tenant on the consumer ctx (MT-01a) — never to a real tenant. So
// pre-tenancy data can never masquerade as tenant A or B.
func TestReplay_LegacyUntenantedLandsInSystem(t *testing.T) {
	env := validEnvelope("")
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	c, err := bus.NewConsumer(
		&oneShotSub{msg: bus.Message{Subject: "x", Body: frame(t, env, []byte("p"))}},
		bus.WithValidator(bus.ValidateReplay),
	)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := c.Subscribe(context.Background(), "x", "g", func(ctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
		got = bus.TenantIDFromContext(ctx)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if got != bus.SystemTenant {
		t.Fatalf("legacy replay tenant=%q want %q", got, bus.SystemTenant)
	}
}

// ============================ API layer ==================================

// The API authorization edge (AUTH-01b, the gateway/query path) denies a
// cross-tenant query BEFORE it reaches the engine: a principal in tenant A
// querying a resource in tenant B is denied even with the admin "*" role —
// tenant isolation beats RBAC. Same-tenant access with the role is allowed.
func TestAPI_CrossTenantQueryDenied(t *testing.T) {
	policy, err := auth.LoadPolicy(strings.NewReader(`{"roles":{"admin":["*"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	az := auth.NewPolicyAuthorizer(policy)
	adminA := &auth.Principal{Subject: "u", Tenant: tenantA, Roles: []string{"admin"}}

	cross := az.Authorize(context.Background(), auth.Request{
		Principal: adminA,
		Action:    auth.ActionRiskRead,
		Resource:  auth.Resource{Type: auth.ResourcePortfolio, ID: "PORT-1", Tenant: tenantB},
	})
	if cross.Allow {
		t.Errorf("cross-tenant query allowed for admin: %s", cross.Reason)
	}

	same := az.Authorize(context.Background(), auth.Request{
		Principal: adminA,
		Action:    auth.ActionRiskRead,
		Resource:  auth.Resource{Type: auth.ResourcePortfolio, ID: "PORT-1", Tenant: tenantA},
	})
	if !same.Allow {
		t.Errorf("same-tenant query denied: %s", same.Reason)
	}
}

// ============================ STORE layer (RLS) ==========================

// State written under tenant A's session is invisible to tenant B, and a direct
// RLS-escape (a raw cross-tenant INSERT) is rejected by the WITH CHECK policy
// (MT-01d). Gated on TEST_POSTGRES_URL; skipped under a superuser role, which
// bypasses RLS and would pass falsely.
func TestStore_RLSCrossTenantIsolation(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run the store-layer isolation contract")
	}
	ctx := context.Background()

	base := pool(t, url, bus.SystemTenant)
	applySchema(t, base)
	var superuser bool
	if err := base.QueryRow(ctx, "SELECT current_setting('is_superuser')::bool").Scan(&superuser); err != nil {
		t.Fatalf("is_superuser: %v", err)
	}
	if superuser {
		t.Skip("RLS is bypassed for superusers; run TEST_POSTGRES_URL as a non-superuser role")
	}

	storeA := persist.NewPostgres(pool(t, url, tenantA))
	poolB := pool(t, url, tenantB)
	storeB := persist.NewPostgres(poolB)

	if err := storeA.Save(ctx, persist.PortfolioRecord{ID: v1.PortfolioID("PORT-A")}); err != nil {
		t.Fatalf("tenant A Save: %v", err)
	}

	// B cannot see A's portfolio — not by id, not in bulk.
	if _, err := storeB.Load(ctx, v1.PortfolioID("PORT-A")); err != persist.ErrNotFound {
		t.Errorf("tenant B Load of A's portfolio = %v want ErrNotFound", err)
	}
	if all, err := storeB.LoadAll(ctx); err != nil || len(all) != 0 {
		t.Errorf("tenant B LoadAll = %v (err %v) want empty", all, err)
	}

	// RLS-escape: B forges a row tagged as A. WITH CHECK (tenant_id = the session
	// GUC) rejects it — a tenant cannot write into another's scope.
	_, err := poolB.Exec(ctx,
		`INSERT INTO portfolios (tenant_id, portfolio_id, position_count) VALUES ($1, 'PORT-FORGED', 0)`, tenantA)
	if err == nil {
		t.Error("RLS-escape succeeded: tenant B wrote a row tagged tenant A")
	}
}

// ============================ helpers ====================================

type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error { c.sent = append(c.sent, m); return nil }
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error { return nil }
func (c *captureClient) Close() error                                                 { return nil }

type oneShotSub struct{ msg bus.Message }

func (s *oneShotSub) Subscribe(ctx context.Context, _, _ string, h bus.Handler) error {
	return h(ctx, s.msg)
}

var fixedTime = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

// factEvent is a minimal valid FACT the Producer stamps tenant/identity onto.
func factEvent() bus.Event {
	return bus.Event{
		Subject: "market.equity.trade", EventType: "market.equity.trade",
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "market",
		EventTime: fixedTime, PartitionKey: "AAPL", PayloadSchemaRef: "market.v1.MarketDataEvent:1",
		Payload: timestamppb.New(fixedTime),
	}
}

// validEnvelope builds a fully-populated live FACT envelope with the given
// tenant (empty ⇒ legacy/untenanted). idempotency_key == event_id per the FACT
// invariant so Validate's only complaint is a missing tenant.
func validEnvelope(tenant string) *envelopepb.Envelope {
	ts := timestamppb.New(fixedTime)
	return &envelopepb.Envelope{
		EventId: "evt-1", EventType: "market.equity.trade", SchemaVersion: 1, EnvelopeVersion: 2,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "market",
		EventTime: ts, IngestionTime: ts, PublishTime: ts, CorrelationId: "evt-1",
		Source: "svc/1", ProducerVersion: "v", PartitionKey: "AAPL",
		IdempotencyKey: "evt-1", PayloadSchemaRef: "market.v1.MarketDataEvent:1",
		TenantId: tenant,
	}
}

func frame(t *testing.T, env *envelopepb.Envelope, payload []byte) []byte {
	t.Helper()
	b, err := proto.Marshal(&envelopepb.EventFrame{Envelope: env, Payload: payload})
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	return b
}

// pool returns a pgxpool whose every connection pins app.tenant_id to tenant
// (the authenticated-session-GUC pattern), so RLS scopes its I/O to that tenant.
func pool(t *testing.T, url, tenant string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func applySchema(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.Exec(ctx, `DROP TABLE IF EXISTS applied_keys, positions, portfolios CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob("../../../services/risk-engine/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := p.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
}
