// lake-sink binary entrypoint (LAKE-01a). Streams the durable Kafka log (EVT-09,
// the system of record) and lands every event into the lakehouse, decoding each
// payload against the EVT-16 schema registry so the table columns track the
// published schema — schema-evolution-aware CDC. The default sink writes
// Hive-partitioned NDJSON a catalog ingest (Iceberg/Delta) commits as tables.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/lake-sink/internal/cdc"
	"github.com/eighred/kanz/services/lake-sink/internal/config"
	"github.com/eighred/kanz/services/lake-sink/internal/decode"
	"github.com/eighred/kanz/services/lake-sink/internal/server"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    cfg.Source,
		ServiceVersion: version(),
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRatio:    1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		os.Exit(2)
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	fileSink, err := sink.NewFileSink(cfg.OutputDir, cfg.Source)
	if err != nil {
		logger.Error("sink open failed", "err", err)
		os.Exit(2)
	}

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("lake-sink listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	runErr := runSink(ctx, cfg, fileSink, readiness, logger, obs)
	if runErr != nil {
		logger.Error("sink stopped with error", "err", runErr)
	}

	// Flush and close the sink before exit so buffered rows are durable.
	if err := fileSink.Close(); err != nil {
		logger.Error("sink close error", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// runSink dials Kafka, subscribes every configured topic on the one consumer
// group, and folds each event into the sink. Durability has no lag to bound:
// cdc.EventSink.Handle flushes (bufio.Flush + fsync) every row before acking,
// so a row is durable by the time its offset commits (DATA-M5) — there is no
// periodic flush here because there is nothing left for one to do. The first
// non-cancellation subscription error cancels the siblings and is returned —
// fail-fast, so a broken subscription brings the sink down rather than
// silently losing the log.
func runSink(ctx context.Context, cfg config.Config, fileSink sink.Sink, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)
	metrics := cdc.NewMetrics(obs.Registry)

	var resolver decode.Resolver
	if cfg.RegistryURL != "" {
		resolver = decode.NewCachingResolver(decode.NewHTTPResolver(cfg.RegistryURL, nil))
	} else {
		logger.Warn("no LAKE_SINK_REGISTRY_URL — landing envelope-only (payloads undecoded)")
	}
	eventSink := cdc.NewEventSink(decode.NewDecoder(resolver), fileSink, time.Now, logger, metrics)

	client, err := bus.DialKafka(bus.KafkaConfig{Brokers: cfg.Brokers, ClientID: cfg.Source})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, topic := range cfg.Topics {
		wg.Add(1)
		go func(topic string) {
			defer wg.Done()
			logger.Info("lake-sink subscribing", "topic", topic, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, topic, cfg.ConsumerGroup, eventSink.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(topic)
	}
	readiness.Set(true)
	wg.Wait()
	readiness.Set(false)
	return firstErr
}

// version is the service version stamped on telemetry. Hardcoded until the build
// injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }
