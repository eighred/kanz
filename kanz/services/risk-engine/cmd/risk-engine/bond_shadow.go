package main

import (
	"context"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/services/risk-engine/internal/bondshadow"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
	"github.com/prometheus/client_golang/prometheus"
)

// newBondShadow exposes diagnostic outcomes only; it has no financial publisher.
func newBondShadow(ctx context.Context, cfg config.Config, reg prometheus.Registerer) (*bondshadow.Worker, error) {
	results := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kanz_risk_rust_bond_shadow_results_total", Help: "Diagnostic Rust bond comparisons; no model approval."}, []string{"status"})
	reg.MustRegister(results)
	configured := prometheus.NewGauge(prometheus.GaugeOpts{Name: "kanz_risk_rust_bond_shadow_configured", Help: "Optional Rust bond shadow configuration; not runtime readiness."})
	if cfg.RustBondShadow.Enabled {
		configured.Set(1)
	}
	reg.MustRegister(configured)
	return bondshadow.New(ctx, cfg.RustBondShadow, cfg.Tenant, version.String(), func(status string) { results.WithLabelValues(status).Inc() })
}

// bondShadowCallback leaves the capture hook absent when the worker is disabled.
func bondShadowCallback(w *bondshadow.Worker) func(compute.BondRiskCapture) {
	if w == nil {
		return nil
	}
	return w.Submit
}
