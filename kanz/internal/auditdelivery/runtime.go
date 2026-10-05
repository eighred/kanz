package auditdelivery

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

type Config struct{ NATSURL, SPIFFESocket string }

// Start owns transport and worker shutdown; the caller retains pool ownership.
// An unconfigured development instance exports enabled=0 and logs the missing
// projection. Production manifests explicitly configure the durable transport.
func Start(ctx context.Context, pool *pgxpool.Pool, source Source, cfg Config, registry *prometheus.Registry, logger *slog.Logger, fatal func(error)) (func(), error) {
	enabled := prometheus.NewGauge(prometheus.GaugeOpts{Name: "kanz_authority_audit_enabled", Help: "One when durable authority audit delivery is configured."})
	if err := registry.Register(enabled); err != nil {
		return nil, err
	}
	if cfg.NATSURL == "" {
		logger.Warn("central authority audit delivery disabled; evidence remains in local journal")
		return func() {}, nil
	}
	boot, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mesh, err := transport.NewMesh(boot, cfg.SPIFFESocket)
	if err != nil {
		return nil, err
	}
	var connected atomic.Bool
	connected.Store(true)
	client, err := bus.DialNATS(boot, bus.NATSConfig{URL: cfg.NATSURL, Name: string(source), TLSConfig: mesh.Client, Metrics: bus.NewBusMetrics(registry), OnDisconnect: func(error) { connected.Store(false) }, OnReconnect: func() { connected.Store(true) }})
	if err != nil {
		_ = mesh.Close()
		return nil, err
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: string(source), ProducerVersion: version.String()})
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, err
	}
	worker, err := New(boot, pool, producer, source, registry)
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, err
	}
	worker.connected = connected.Load
	workCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := worker.Run(workCtx, logger); err != nil && workCtx.Err() == nil {
			logger.Error("authority audit worker stopped")
			fatal(err)
		}
	}()
	enabled.Set(1)
	var once sync.Once
	return func() { once.Do(func() { stop(); <-done; _ = client.Close(); _ = mesh.Close(); enabled.Set(0) }) }, nil
}
