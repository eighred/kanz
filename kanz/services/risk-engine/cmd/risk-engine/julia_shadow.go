package main

import (
	"context"
	"log/slog"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk-engine/internal/calibrationshadow"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
	"github.com/prometheus/client_golang/prometheus"
)

// newJuliaShadow wires diagnostic-only counters; the caller owns worker shutdown.
func newJuliaShadow(ctx context.Context, cfg config.Config, reg prometheus.Registerer) (*calibrationshadow.Worker, error) {
	shadowResults := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kanz_risk_julia_shadow_results_total", Help: "Diagnostic outcomes of optional Julia curve comparisons; no model approval."}, []string{"status"})
	reg.MustRegister(shadowResults)
	shadow, err := calibrationshadow.New(ctx, cfg.JuliaShadow, func(status string) { shadowResults.WithLabelValues(status).Inc() })
	if err != nil {
		return nil, err
	}
	shadowConfigured := prometheus.NewGauge(prometheus.GaugeOpts{Name: "kanz_risk_julia_shadow_configured", Help: "Whether optional Julia shadow comparisons are configured; not worker readiness."})
	if cfg.JuliaShadow.Enabled {
		shadowConfigured.Set(1)
	}
	reg.MustRegister(shadowConfigured)
	return shadow, nil
}

// startConfiguredCalibration reports only schedulers that actually started.
func startConfiguredCalibration(ctx context.Context, cfg config.Config, client *bus.NATSClient, busMetrics *bus.BusMetrics, logger *slog.Logger, reg prometheus.Registerer, store *curve.Store, artifacts *modelRecorder, shadow *calibrationshadow.Worker) map[string]string {
	scheduled := map[string]string{}
	if cfg.CalibrationInterval > 0 && cfg.CalibrationRates != "" {
		if err := startCalibration(ctx, cfg, client, busMetrics, logger, reg, store, artifacts, shadow); err != nil {
			logger.Error("calibration scheduler disabled", "err", err)
		} else {
			scheduled["curve"] = "rates"
		}
	}
	return scheduled
}
