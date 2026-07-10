//go:build binance

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

// configuredVenues (binance build) routes to Binance Spot when credentials are
// present and starts the M3.6 background workers — the user-data websocket
// ingester, the periodic REST reconciliation loop, and the ticker feed — wired
// to the OMS order store (OrderLookup + ExpectedOrders) and the bus producer.
// Falls back to SimVenue with no credentials so a tagged binary still runs in
// dev. Keys come from env / a mock Vault mount, never from code.
func configuredVenues(ctx context.Context, cfg config.Config, store order.Store, producer execution.Publisher, logger *slog.Logger) []execution.Venue {
	apiKey := secretEnv("BINANCE_API_KEY")
	apiSecret := secretEnv("BINANCE_API_SECRET")
	if apiKey == "" || apiSecret == "" {
		logger.Warn("binance: no API credentials — routing to SimVenue (set BINANCE_API_KEY/_SECRET for testnet)")
		return []execution.Venue{execution.NewSimVenue(cfg.SimVenueMIC)}
	}
	baseURL := envOr("BINANCE_BASE_URL", "https://testnet.binance.vision")
	wsBase := envOr("BINANCE_WS_BASE", "wss://testnet.binance.vision")
	settings := execution.VenueSettings{
		MIC:          envOr("BINANCE_MIC", "BINANCE"),
		BaseURL:      baseURL,
		APIKey:       apiKey,
		APISecret:    apiSecret,
		Symbols:      parseSymbolMap(os.Getenv("BINANCE_SYMBOLS")),
		WeightBudget: 1200,
		OnThrottle: func() {
			logger.Error("binance: REST weight budget exhausted — backing off (structural alert)")
		},
	}
	conn := execution.NewBinanceConnector(settings, wsBase)

	// Surface the OMS order store as the workers' OrderLookup + ExpectedOrders.
	adapter := storeAdapter{store: store}
	conn.Start(ctx, execution.WorkerDeps{
		Publisher: producer,
		Lookup:    adapter,
		Expected:  adapter,
		Tenant:    os.Getenv("BINANCE_TENANT"),
		Logger:    logger,
	})
	logger.Info("binance connector wired", "base_url", baseURL, "ws_base", wsBase)
	return []execution.Venue{conn.Venue()}
}

// storeAdapter surfaces the OMS order store to the connector's workers. It lives
// here (the composition root), not in execution, because execution must not
// import order (order already imports execution — an import cycle).
type storeAdapter struct{ store order.Store }

// Lookup enriches an exchange fill with Kanz's order context.
func (a storeAdapter) Lookup(orderID string) (*orderpb.OrderState, bool) {
	st, err := a.store.Load(context.Background(), orderID)
	if err != nil {
		return nil, false
	}
	return st, true
}

// OpenOrders returns the non-terminal orders the reconciler audits.
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
