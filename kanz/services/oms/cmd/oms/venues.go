package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/pkg/transport"
	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/order"
)

// closeRegistry is the shared in-flight-close registry — the In-Flight Certainty
// seam's single instance for the process. It is deliberately UNTAGGED and shared:
// the OMS order service Tracks a close here the moment it dispatches a venue
// cancel (the writer), and whichever exchange reconcilers are compiled in drain
// it from their healing watchdogs (the readers). One registry across Binance and
// OKX — the seam is shared, never duplicated per venue.
var closeRegistry = execution.NewCloseRegistry()

// configuredVenues is the multi-venue allocation matrix's composition root. It
// aggregates venues from two sources and falls back to the in-process SimVenue
// when neither yields one, so the default (vendor-free) binary and dev builds
// still run. The router then sends each fanned-out order to its allocated venue
// by MIC — and hard-errors on a MIC it has no venue for, rather than simulating.
//
//   - COMPILED IN (legacy): Binance under -tags binance, OKX under -tags okx.
//     Vendor SDKs, request signing, and exchange websocket loops live inside this
//     process, sharing an address space with order state.
//   - OUT OF PROCESS (INFRA-M7a): OMS_VENUE_ENDPOINTS → one execution.GRPCVenue
//     per MIC, over mTLS. No build tag, no vendor code linked here at all.
//
// The second retires the first. Once an adapter exists for each venue, the tags
// and the connectors under them come out of the OMS entirely (M7a-2/3/4) and this
// function loses half its job.
func configuredVenues(ctx context.Context, cfg config.Config, store order.Store, producer execution.Publisher, logger *slog.Logger) ([]execution.Venue, func()) {
	adapter := storeAdapter{store: store}
	var venues []execution.Venue
	venues = append(venues, okxVenues(ctx, cfg, adapter, producer, logger)...)

	// INFRA-M7a: out-of-process adapters. These need no build tag and link no
	// vendor code — the OMS speaks venue.v1 over mTLS and never imports an
	// exchange SDK. They are the path that retires the tags above.
	grpcVenues, closeConns, err := dialVenues(ctx, cfg, logger)
	if err != nil {
		// A configured venue that will not dial is FATAL, not a degradation. The
		// alternative is booting without it and silently routing its orders
		// nowhere — or worse, to the simulator below.
		logger.Error("venue adapter dial failed", "err", err)
		os.Exit(2)
	}
	venues = append(venues, grpcVenues...)

	if len(venues) == 0 {
		// THIS OMS IS A SIMULATOR. It will accept orders and fill them against
		// nothing. That is correct for tests and local dev and catastrophic in
		// production, so it is stated at WARN, not buried at Info.
		logger.Warn("NO REAL VENUES CONFIGURED — every order will be filled by the in-process SimVenue and NOTHING will reach an exchange",
			"sim_mic", cfg.SimVenueMIC,
			"fix", "set OMS_VENUE_ENDPOINTS (e.g. XBIN=venue-binance.kanz-services.svc:9000)")
		return []execution.Venue{execution.NewSimVenue(cfg.SimVenueMIC)}, closeConns
	}
	return venues, closeConns
}

// dialVenues turns OMS_VENUE_ENDPOINTS ("XBIN=host:port,XOKX=host:port") into
// execution.GRPCVenue clients, one per MIC, and returns a func that closes them.
//
// The dial is mTLS when a SPIFFE socket is configured (SEC-01a: the workload's
// SVID is the credential, authorized against the mesh). Without one it is
// PLAINTEXT, which is a dev-only posture and says so loudly — this connection
// carries live orders to a live exchange, and an unauthenticated peer on it can
// submit trades.
func dialVenues(ctx context.Context, cfg config.Config, logger *slog.Logger) ([]execution.Venue, func(), error) {
	endpoints := parseSymbolMap(cfg.VenueEndpoints) // MIC → address; same "K=V,K=V" form
	if len(endpoints) == 0 {
		return nil, func() {}, nil
	}

	dialOpt := grpc.WithTransportCredentials(insecure.NewCredentials())
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, nil, fmt.Errorf("venue mTLS source: %w", err)
		}
		dialOpt = transport.ClientDialOption(src, transport.AuthorizeMesh())
		logger.Info("venue adapters: mTLS enabled", "venues", len(endpoints))
	} else {
		logger.Warn("VENUE ADAPTERS ARE PLAINTEXT — no SPIFFE_ENDPOINT_SOCKET. This link carries live orders; anyone who can reach it can trade",
			"venues", len(endpoints))
	}

	var (
		venues []execution.Venue
		conns  []*grpc.ClientConn
	)
	closeConns := func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}
	for mic, addr := range endpoints {
		conn, err := grpc.NewClient(addr, dialOpt)
		if err != nil {
			closeConns()
			return nil, nil, fmt.Errorf("venue %s at %s: %w", mic, addr, err)
		}
		conns = append(conns, conn)
		venues = append(venues, execution.NewGRPCVenue(mic, conn, cfg.Tenant))
		logger.Info("venue adapter registered", "mic", mic, "endpoint", addr)
	}
	return venues, closeConns, nil
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
