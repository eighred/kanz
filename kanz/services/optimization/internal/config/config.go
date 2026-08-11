package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the optimization service runtime configuration, sourced from the
// environment (the same stateless reporting/compute shape as the performance
// service — OPT-01 is a construction/proposal plane). The service optimizes
// target weights and materializes a proposal's trades into OMS-01 commands; the
// concrete bus publisher + pre-trade gate are wired at the composition root
// behind the bridge seams (DEBT-02), so the default boot serves the
// optimize/propose + dry-run-materialize endpoints without a broker.
type Config struct {
	// Listen is the API port, and it is 8100 rather than the platform's usual
	// 8080 for a reason that is not stylistic (#409).
	//
	// allow-observability-scrape selects EVERY pod in kanz-services
	// (podSelector: {}) and admits a list of ports, 8080 among them — so any
	// service listening on 8080 is reachable from the whole kanz-observability
	// namespace whatever it serves there. Moving /metrics to its own port
	// therefore buys nothing while the API stays on 8080: the API would still be
	// open to the monitoring plane, and these routes decide whose name goes on an
	// order from a header the api-gateway is supposed to be the only source of.
	//
	// 8100 is outside that list, and test/arch asserts it stays outside.
	Listen string

	// MetricsListen is a SEPARATE PORT for /metrics, and the separation is a
	// security boundary rather than tidiness (#409).
	//
	// This service decides WHOSE NAME goes on an order from the principal header
	// the api-gateway injects, so its API must be reachable by the gateway and
	// nothing else. Every other header-trusting service on this platform serves
	// its tenant-scoped routes on the same port as /metrics, and
	// allow-observability-scrape must admit that port — so a pod in
	// kanz-observability can choose a principal and use those routes. That is the
	// exposure #232 could not close, because closing it needs a second listener in
	// each service rather than a manifest edit.
	//
	// This is that second listener. The trading surface is the one place the
	// platform cannot afford to inherit the gap, so it is the first service that
	// does not. The precedent is inference, whose gRPC prediction API is
	// deliberately kept off the scrape list for the same reason.
	MetricsListen string
	LogLevel      slog.Level

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	return Config{
		Listen:        envOr("OPTIMIZATION_LISTEN", ":8100"),
		MetricsListen: envOr("OPTIMIZATION_METRICS_LISTEN", ":8094"),
		LogLevel:      parseLevel(os.Getenv("OPTIMIZATION_LOG_LEVEL")),
		OTLPEndpoint:  os.Getenv("OPTIMIZATION_OTLP_ENDPOINT"),
	}, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
