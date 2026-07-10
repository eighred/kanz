package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
	"github.com/kanz-eng/kanz/services/oms/internal/order"
)

// configuredVenues is the multi-venue allocation matrix's composition root. It
// aggregates whatever exchange venues are compiled in — Binance under
// -tags binance, OKX under -tags okx (each contributed by a build-tag-split
// helper) — and falls back to the in-process SimVenue when none are configured,
// so the default (vendor-free) binary and dev builds still run. The router then
// sends each fanned-out order to its allocated venue by MIC.
func configuredVenues(ctx context.Context, cfg config.Config, store order.Store, producer execution.Publisher, logger *slog.Logger) []execution.Venue {
	adapter := storeAdapter{store: store}
	var venues []execution.Venue
	venues = append(venues, binanceVenues(ctx, cfg, adapter, producer, logger)...)
	venues = append(venues, okxVenues(ctx, cfg, adapter, producer, logger)...)
	if len(venues) == 0 {
		logger.Info("no exchange venues configured — routing to SimVenue")
		return []execution.Venue{execution.NewSimVenue(cfg.SimVenueMIC)}
	}
	return venues
}

// storeAdapter surfaces the OMS order store to the connectors' background
// workers (OrderLookup + ExpectedOrders). It lives here (the composition root),
// not in execution, because execution must not import order — order already
// imports execution (an import cycle).
type storeAdapter struct{ store order.Store }

func (a storeAdapter) Lookup(orderID string) (*orderpb.OrderState, bool) {
	st, err := a.store.Load(context.Background(), orderID)
	if err != nil {
		return nil, false
	}
	return st, true
}

func (a storeAdapter) OpenOrders() []*orderpb.OrderState {
	all, err := a.store.List(context.Background())
	if err != nil {
		return nil
	}
	var open []*orderpb.OrderState
	for _, st := range all {
		if !terminalStatus(st.GetStatus()) {
			open = append(open, st)
		}
	}
	return open
}

func terminalStatus(s orderpb.OrderStatus) bool {
	switch s {
	case orderpb.OrderStatus_ORDER_STATUS_FILLED,
		orderpb.OrderStatus_ORDER_STATUS_CANCELLED,
		orderpb.OrderStatus_ORDER_STATUS_REJECTED,
		orderpb.OrderStatus_ORDER_STATUS_EXPIRED:
		return true
	default:
		return false
	}
}

// --- shared composition-root helpers (used by both venue builders) ---

// parseSymbolMap parses "BTC-USD=BTCUSDT,ETH-USD=ETHUSDT" into a map.
func parseSymbolMap(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// secretEnv prefers a CSI/Vault file mount (<k>_FILE) over a plaintext env var.
func secretEnv(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
