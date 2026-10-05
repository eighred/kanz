package audit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/auditdelivery"
	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
)

type authorityPublisher func(context.Context, bus.Event) error

func (f authorityPublisher) Publish(ctx context.Context, e bus.Event) error { return f(ctx, e) }

// Expiring broker dedup must not weaken central idempotency. Strip that header
// in this transport wrapper so the real broker delivers every retry to the real
// projector; production keeps both broker and central dedup enabled.
type expiredBrokerDedup struct{ bus.Client }

func (c expiredBrokerDedup) Publish(ctx context.Context, m bus.Message) error {
	delete(m.Headers, "Nats-Msg-Id")
	return c.Client.Publish(ctx, m)
}

func authorityMigrations(t *testing.T, pool *pgxpool.Pool, source auditdelivery.Source) []string {
	t.Helper()
	service := "identity"
	if source == auditdelivery.Session {
		service = "web-bff"
	}
	files, err := filepath.Glob(filepath.Join("../../../", service, "migrations/*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	for _, f := range files[:len(files)-1] {
		applyAuthoritySQL(t, pool, f)
	}
	return files
}
func applyAuthoritySQL(t *testing.T, pool *pgxpool.Pool, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err = tx.Exec(t.Context(), string(b)); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func authorityInsert(t *testing.T, ctx context.Context, tx pgx.Tx, source auditdelivery.Source, tenant, subject string) int64 {
	t.Helper()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	d := auth.BuildDecisionLog("authority", auth.Request{Principal: &auth.Principal{Tenant: tenant, Subject: subject}, Action: "identity.test", Resource: auth.Resource{Type: "identity-user", ID: subject, Tenant: tenant}}, auth.Decision{Allow: true, Reason: "committed authority mutation"})
	raw, err := protojson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`INSERT INTO %s(tenant_id,subject,occurred_at,decision) VALUES($1,$2,$3,$4::jsonb) RETURNING sequence`, source), tenant, subject, time.Now().Add(-time.Hour), string(raw)).Scan(&seq)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}
func authorityCommit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, source auditdelivery.Source, tenant, subject string) int64 {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	seq := authorityInsert(t, ctx, tx, source, tenant, subject)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestAuthorityJournalRealSpineAndCentralProjection(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required for real authority audit delivery")
	}
	for _, source := range []auditdelivery.Source{auditdelivery.Identity, auditdelivery.Session} {
		t.Run(string(source), func(t *testing.T) {
			pool := newAuditPool(t)
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			files := authorityMigrations(t, pool, source)
			tenant := fmt.Sprintf("audit1292-%d", time.Now().UnixNano())
			other := tenant + "-other"
			authorityCommit(t, ctx, pool, source, tenant, "legacy-A")
			authorityCommit(t, ctx, pool, source, other, "legacy-B")
			applyAuthoritySQL(t, pool, files[len(files)-1])
			var migrationPolicies int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE schemaname=current_schema() AND policyname='delivery_migration_only'`).Scan(&migrationPolicies); err != nil || migrationPolicies != 0 {
				t.Fatalf("migration policy survived: %d %v", migrationPolicies, err)
			}
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: tenant})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			proxyURL, partition := authorityNetwork(t, url)
			publishing, err := bus.DialNATS(ctx, bus.NATSConfig{URL: proxyURL, Name: tenant + "-publisher", PublishTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = publishing.Close() }()
			producer, err := bus.NewProducer(expiredBrokerDedup{publishing}, bus.ProducerConfig{Source: string(source), ProducerVersion: "test"})
			if err != nil {
				t.Fatal(err)
			}
			store := NewPostgres(pool)
			projector := NewProjector(store, time.Now)
			var deliveries atomic.Int64
			subCtx, stopSub := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() {
				done <- client.Subscribe(subCtx, auditdelivery.Subject, tenant, func(c context.Context, m bus.Message) error {
					env, payload, e := bus.Unframe(m.Body)
					if e != nil {
						return e
					}
					if env.TenantId != tenant && env.TenantId != other {
						return nil
					}
					if e = bus.Validate(env); e != nil {
						return e
					}
					if e = projector.Handle(c, env, payload); e != nil {
						return e
					}
					deliveries.Add(1)
					return nil
				})
			}()
			defer func() {
				stopSub()
				select {
				case e := <-done:
					if e != nil && !errors.Is(e, context.Canceled) {
						t.Error(e)
					}
				case <-time.After(5 * time.Second):
					t.Error("audit consumer did not stop")
				}
			}()
			var ackCrash atomic.Bool
			var mu sync.Mutex
			ids := map[string]int{}
			pub := authorityPublisher(func(c context.Context, e bus.Event) error {
				if err := producer.Publish(c, e); err != nil {
					return err
				}
				id, err := uuid.Parse(e.EventID)
				if err != nil || id.Version() != 7 {
					t.Errorf("unstable event identity: %q", e.EventID)
				}
				mu.Lock()
				ids[e.EventID]++
				mu.Unlock()
				if !ackCrash.Swap(true) {
					return errors.New("crash after broker acknowledgement before queue deletion")
				}
				return nil
			})
			worker, err := auditdelivery.New(ctx, pool, pub, source, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = worker.DeliverOne(ctx, tenant); err == nil {
				t.Fatal("ack crash was not injected")
			}
			if sent, e := worker.DeliverOne(ctx, tenant); e != nil || !sent {
				t.Fatalf("restart lost pending evidence: %v %v", sent, e)
			}
			if sent, e := worker.DeliverOne(ctx, other); e != nil || !sent {
				t.Fatalf("other tenant legacy row missing: %v %v", sent, e)
			}
			// Allocation order is deliberately opposite commit order.
			late, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = late.Rollback(ctx) }()
			authorityInsert(t, ctx, late, source, tenant, "late-commit")
			authorityCommit(t, ctx, pool, source, tenant, "early-commit")
			if sent, e := worker.DeliverOne(ctx, tenant); e != nil || !sent {
				t.Fatalf("committed later allocation invisible: %v %v", sent, e)
			}
			if err = late.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if sent, e := worker.DeliverOne(ctx, tenant); e != nil || !sent {
				t.Fatalf("late commit skipped by progress: %v %v", sent, e)
			}
			// Rolled-back authority evidence cannot escape to the spine.
			rollback, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			authorityInsert(t, ctx, rollback, source, tenant, "rolled-back")
			if err = rollback.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			const batch = 24
			for i := 0; i < batch; i++ {
				authorityCommit(t, ctx, pool, source, tenant, fmt.Sprintf("parallel-%d", i))
			}
			var wg sync.WaitGroup
			errs := make(chan error, 4)
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						sent, e := worker.DeliverOne(ctx, tenant)
						if e != nil {
							errs <- e
							return
						}
						if !sent {
							return
						}
					}
				}()
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				t.Error(e)
			}
			// Disconnect the actual TCP path after committing new authority evidence.
			authorityCommit(t, ctx, pool, source, tenant, "during-network-outage")
			partition()
			if _, e := worker.DeliverOne(ctx, tenant); e == nil {
				t.Fatal("partitioned publisher acknowledged evidence")
			}
			_ = publishing.Close()
			publishing, err = bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: tenant + "-restarted"})
			if err != nil {
				t.Fatal(err)
			}
			producer, err = bus.NewProducer(expiredBrokerDedup{publishing}, bus.ProducerConfig{Source: string(source), ProducerVersion: "test"})
			if err != nil {
				t.Fatal(err)
			}
			worker, err = auditdelivery.New(ctx, pool, pub, source, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sent, e := worker.DeliverOne(ctx, tenant); e != nil || !sent {
				t.Fatalf("outage/restart lost evidence: %v %v", sent, e)
			}
			want := batch + 5
			if source == auditdelivery.Identity {
				const secretMarker = "credential-must-never-be-projected"
				if _, err = pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,credential_hash,status) VALUES('actual-admin',$1,ARRAY['kanz-identity-admin'],$2,'active'),('actual-user',$1,ARRAY['kanz-trader'],$2,'active')`, tenant, secretMarker); err != nil {
					t.Fatal(err)
				}
				if _, err = identity.NewPostgres(pool).SetAccess(ctx, identity.Administration{Subject: "actual-admin", Tenant: tenant}, "actual-user", 0, []string{"kanz-user"}, nil, time.Now()); err != nil {
					t.Fatal(err)
				}
				if sent, e := worker.DeliverOne(ctx, tenant); e != nil || !sent {
					t.Fatalf("actual authority mutation did not deliver: %v %v", sent, e)
				}
				want++
			}

			for {
				records, e := store.Query(ctx, Filter{Limit: 1000})
				if e != nil {
					t.Fatal(e)
				}
				if len(records) == want && deliveries.Load() >= int64(want+1) {
					seen := map[string]bool{}
					for _, rec := range records {
						if rec.Kind != KindAuthzDecision || rec.Attributes["principal.tenant"] != rec.TenantID || seen[rec.EventID] {
							t.Fatalf("invalid projected authority record: %+v", rec)
						}
						seen[rec.EventID] = true
						for _, value := range rec.Attributes {
							if strings.Contains(value, "credential-must-never-be-projected") {
								t.Fatal("credential leaked into central evidence")
							}
						}
						if strings.Contains(rec.Summary, "rolled-back") {
							t.Fatal("rollback leaked")
						}
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("central evidence incomplete: %d want %d; deliveries %d", len(records), want, deliveries.Load())
				case <-time.After(20 * time.Millisecond):
				}
			}
			mu.Lock()
			defer mu.Unlock()
			duplicates := 0
			for _, n := range ids {
				if n == 2 {
					duplicates++
				} else if n != 1 {
					t.Fatalf("unexpected delivery attempts: %d", n)
				}
			}
			if len(ids) != want || duplicates != 1 {
				t.Fatalf("retry identity changed: distinct=%d duplicate=%d", len(ids), duplicates)
			}
			// A foreign tenant cannot see or remove queued evidence.
			authorityCommit(t, ctx, pool, source, other, "isolated")
			if sent, e := worker.DeliverOne(ctx, tenant); e != nil || sent {
				t.Fatalf("cross-tenant delivery: %v %v", sent, e)
			}
			if _, e := pool.Exec(ctx, fmt.Sprintf(`SELECT * FROM %s_pending`, source)); e == nil {
				t.Fatal("unscoped queue read accepted")
			}
		})
	}
}
