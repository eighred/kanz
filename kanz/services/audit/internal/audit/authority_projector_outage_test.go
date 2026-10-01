package audit

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/auditdelivery"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func TestAuthorityCentralDatabaseOutageIsRetriedWithoutRedrive(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required")
	}
	pool := newAuditPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	files := authorityMigrations(t, pool, auditdelivery.Session)
	applyAuthoritySQL(t, pool, files[len(files)-1])
	tenant := fmt.Sprintf("authority-db-outage-%d", time.Now().UnixNano())
	authorityCommit(t, ctx, pool, auditdelivery.Session, tenant, "operator")
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: tenant})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "authority-test", ProducerVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := auditdelivery.New(ctx, pool, producer, auditdelivery.Session, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client), bus.WithRetainedRetries(auditdelivery.Subject))
	if err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	projector := NewProjector(store, time.Now)
	blocked, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocked.Rollback(ctx) }()
	if _, err = blocked.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(advisoryLockKey)); err != nil {
		t.Fatal(err)
	}
	failure := make(chan struct{}, 1)
	success := make(chan struct{}, 1)
	done := make(chan error, 1)
	subCtx, stop := context.WithCancel(ctx)
	go func() {
		done <- consumer.Subscribe(subCtx, auditdelivery.Subject, tenant, func(c context.Context, e *envelopepb.Envelope, b []byte) error {
			if e.TenantId != tenant {
				return nil
			}
			attempt, end := context.WithTimeout(c, 200*time.Millisecond)
			defer end()
			err := projector.Handle(attempt, e, b)
			if err != nil {
				select {
				case failure <- struct{}{}:
				default:
				}
			} else {
				select {
				case success <- struct{}{}:
				default:
				}
			}
			return err
		})
	}()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("projector failed to stop")
		}
	}()
	if sent, e := worker.DeliverOne(ctx, tenant); e != nil || !sent {
		t.Fatalf("source delivery failed: %v %v", sent, e)
	}
	select {
	case <-failure:
	case <-ctx.Done():
		t.Fatal("actual database timeout was not exercised")
	}
	if err = blocked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-success:
	case <-ctx.Done():
		t.Fatal("database recovered but evidence was parked or abandoned")
	}
	records, err := store.Query(ctx, Filter{Tenant: tenant})
	if err != nil || len(records) != 1 {
		t.Fatalf("central recovery: %d records, %v", len(records), err)
	}
}
