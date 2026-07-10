//go:build !binance

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
)

// configuredVenues is the default (vendor-free) venue set: the in-process
// SimVenue. The Binance connector is compiled in only under -tags binance,
// which replaces this file with venues_binance.go — so the default OMS binary
// links no exchange-vendor code.
func configuredVenues(cfg config.Config, _ *slog.Logger) []execution.Venue {
	return []execution.Venue{execution.NewSimVenue(cfg.SimVenueMIC)}
}
