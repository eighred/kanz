//go:build !binance

package main

import (
	"context"
	"log/slog"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
)

// binanceVenues is a no-op when the connector is not compiled in — the default
// (vendor-free) build links no Binance code.
func binanceVenues(context.Context, config.Config, storeAdapter, execution.Publisher, *slog.Logger) []execution.Venue {
	return nil
}
