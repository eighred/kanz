package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state"
)

func TestPortfolioInventoryObservesTheLiveStore(t *testing.T) {
	reg := prometheus.NewRegistry()
	store := state.NewStore()
	registerPortfolioInventory(reg, store)

	if got := portfolioInventoryValue(reg, t); got != 0 {
		t.Fatalf("empty store inventory = %v, want 0", got)
	}
	if err := store.Restore(domain.NewPortfolio(v1.PortfolioID("portfolio-1"), domain.CurrencyCode("USD")), nil); err != nil {
		t.Fatalf("restore portfolio: %v", err)
	}
	if got := portfolioInventoryValue(reg, t); got != 1 {
		t.Fatalf("one-portfolio store inventory = %v, want 1", got)
	}
}

func portfolioInventoryValue(reg *prometheus.Registry, t *testing.T) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() == "kanz_risk_portfolios_loaded" {
			return family.Metric[0].Gauge.GetValue()
		}
	}
	t.Fatal("kanz_risk_portfolios_loaded missing")
	return 0
}
