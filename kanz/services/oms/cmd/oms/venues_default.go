//go:build !binance

package main

import (
	"context"
	"log/slog"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
	"github.com/kanz-eng/kanz/services/oms/internal/order"
)

// configuredVenues is the default (vendor-free) venue set: the in-process
// SimVenue. The Binance connector — and its user-data/reconciliation/ticker
// workers — are compiled in only under -tags binance, which replaces this file
// with venues_binance.go. The extra args are unused here.
func configuredVenues(_ context.Context, cfg config.Config, _ order.Store, _ execution.Publisher, _ *slog.Logger) []execution.Venue {
	return []execution.Venue{execution.NewSimVenue(cfg.SimVenueMIC)}
}
