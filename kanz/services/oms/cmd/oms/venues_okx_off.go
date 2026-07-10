//go:build !okx

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
)

// okxVenues is a no-op when the OKX connector is not compiled in.
func okxVenues(config.Config, *slog.Logger) []execution.Venue { return nil }
