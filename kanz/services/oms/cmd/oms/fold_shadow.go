package main

import (
	"context"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/foldshadow"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/eighred/kanz/services/oms/internal/position"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"log/slog"
)

func persistentStoresWithShadow(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger, reg prometheus.Registerer) (order.Store, position.Store, func(), error) {
	results := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kanz_oms_rust_fill_shadow_results_total", Help: "Diagnostic Rust aggregate fold comparisons (fill/cancel/expire); no execution authority."}, []string{"status"})
	for _, status := range []string{"queue_full", "invalid_input", "invalid_result", "worker_rejected", "matched", "disagreement", "output_limit", "timeout", "worker_failure"} {
		results.WithLabelValues(status)
	}
	reg.MustRegister(results)
	configured := prometheus.NewGauge(prometheus.GaugeOpts{Name: "kanz_oms_rust_fill_shadow_configured", Help: "Optional Rust fill shadow configuration; not runtime readiness."})
	if cfg.FillFoldShadow.Enabled {
		configured.Set(1)
	}
	reg.MustRegister(configured)
	worker, err := foldshadow.New(ctx, cfg.FillFoldShadow, cfg.Tenant, version.String(), func(status string) { results.WithLabelValues(status).Inc() })
	if err != nil {
		pool.Close()
		return nil, nil, nil, err
	}
	store := order.NewPostgres(pool)
	if worker != nil {
		store.SetFillFoldObserver(worker.Submit)
		store.SetTerminalFoldObserver(worker.SubmitTerminal)
	}
	return store, position.NewPostgres(pool, cfg.BaseCurrency), func() { worker.Close(); pool.Close() }, nil
}

func terminalFoldObserver(store order.Store) func(order.TerminalFoldCapture) {
	if source, ok := store.(interface {
		TerminalFoldObserver() func(order.TerminalFoldCapture)
	}); ok {
		return source.TerminalFoldObserver()
	}
	return nil
}

// Both service folds and durable recovery observe through the same initialized
// worker. An unconfigured store has no callback and incurs no capture cost.
func fillFoldObserver(store order.Store) func(order.FillFoldCapture) {
	if source, ok := store.(interface {
		FillFoldObserver() func(order.FillFoldCapture)
	}); ok {
		return source.FillFoldObserver()
	}
	return nil
}
