//go:build !okx

package main

import (
	"context"
	"log/slog"

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/services/oms/internal/config"
)

// okxVenues is a no-op when the OKX connector is not compiled in.
func okxVenues(context.Context, config.Config, storeAdapter, execution.Publisher, *slog.Logger) []execution.Venue {
	return nil
}
