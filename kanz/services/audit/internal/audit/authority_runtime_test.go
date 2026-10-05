package audit

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/auditdelivery"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
)

func authorityMetric(t *testing.T, r *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "kanz_authority_audit_"+name {
			if len(f.Metric) == 1 {
				m := f.Metric[0]
				if m.Counter != nil {
					return m.Counter.GetValue()
				}
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("missing metric %s", name)
	return 0
}

func TestAuthorityRuntimeNotificationShutdownAndRetention(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required")
	}
	pool := newAuditPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	files := authorityMigrations(t, pool, auditdelivery.Session)
	applyAuthoritySQL(t, pool, files[len(files)-1])
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "AUTHORITY_AUDIT")
	if err != nil {
		t.Fatal("production bootstrap must provision authority retention", err)
	}
	cfg := stream.CachedInfo().Config
	if cfg.MaxAge != 0 || cfg.Storage != jetstream.FileStorage || cfg.Discard != jetstream.DiscardNew || cfg.MaxMsgs > 0 || cfg.MaxBytes > 0 || cfg.MaxMsgsPerSubject > 0 {
		t.Fatalf("acknowledged evidence can expire or be evicted: %+v", cfg)
	}
	registry := prometheus.NewRegistry()
	fatal := make(chan error, 1)
	stop, err := auditdelivery.Start(ctx, pool, auditdelivery.Session, auditdelivery.Config{NATSURL: url}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), func(e error) { fatal <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	await := func(name string, want float64) {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if authorityMetric(t, registry, name) == want {
				return
			}
			select {
			case err := <-fatal:
				t.Fatal(err)
			case <-deadline.C:
				t.Fatalf("%s never reached %v", name, want)
			case <-ticker.C:
			}
		}
	}
	await("scan_healthy", 1)
	for i := 1; i <= 2; i++ {
		authorityCommit(t, ctx, pool, auditdelivery.Session, "runtime-tenant", "runtime-operator")
		// Recovery scan waits 30 seconds; a three-second completion proves that
		// a live journal notification wakes the worker rather than being polled.
		await("delivered_total", float64(i))
		await("pending", 0)
	}
	if authorityMetric(t, registry, "enabled") != 1 || authorityMetric(t, registry, "last_scan_timestamp_seconds") <= 0 {
		t.Fatal("delivery health unavailable")
	}
	stop()
	stop()
	if authorityMetric(t, registry, "enabled") != 0 {
		t.Fatal("stopped worker still claims active delivery")
	}
	// Cancellation may close a connection in flight. pgxpool.Release then
	// delegates to puddle.Resource.Destroy, whose destructor runs in its own
	// goroutine and retains the acquired count until it finishes. Joining the
	// worker guarantees release was called, not that pool housekeeping already
	// ran. A bounded drain still fails on an actual retained connection.
	drain := time.NewTimer(3 * time.Second)
	defer drain.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for pool.Stat().AcquiredConns() != 0 {
		select {
		case <-drain.C:
			t.Fatalf("shutdown retained %d pool connections", pool.Stat().AcquiredConns())
		case <-tick.C:
		}
	}
	if _, err = pool.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatal("worker closed caller-owned pool", err)
	}
}
