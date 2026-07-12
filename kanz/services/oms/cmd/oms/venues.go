package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

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

// configuredVenues is the venue composition root. Every venue is now OUT OF
// PROCESS (INFRA-M7a): OMS_VENUE_ENDPOINTS maps each MIC to an adapter, dialed
// over mTLS as an execution.GRPCVenue.
//
// There are no build tags left. There is no Binance code and no OKX code in this
// binary — no vendor SDK, no request signing, no exchange websocket. The OMS
// cannot reach an exchange except through an adapter, which is the entire point:
// a crash or a compromise in vendor client code no longer shares an address space
// with the process that owns order state.
//
// The SimVenue fallback survives for tests and local dev, and ONLY for that. If it
// is ever reached in production the OMS is filling orders against nothing, so it
// says so at WARN in as many words, and the router hard-errors on any MIC it has
// no venue for rather than quietly routing there.
func configuredVenues(ctx context.Context, cfg config.Config, store order.Store, producer execution.Publisher, logger *slog.Logger) ([]execution.Venue, func()) {
	var venues []execution.Venue

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

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
