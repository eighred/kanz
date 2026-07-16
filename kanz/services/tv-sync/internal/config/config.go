// Package config loads tv-sync's runtime configuration from the environment.
// tv-sync is a pure projection — it needs only where the bus is and where to
// serve the Broker API.
package config

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

// Config is the resolved configuration.
type Config struct {
	Listen       string
	LogLevel     slog.Level
	OTLPEndpoint string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket  string
	NATSURL       string
	Source        string
	ConsumerGroup string
	// PriceSubject is the market price spine tv-sync folds into its MarkSource
	// for live unrealized P&L (M3.5). Default "market.>" catches every market
	// variant, incl. the Binance ticker feed's market.crypto.trade.
	PriceSubject string

	// DatabaseURL is the durable fact log (EXEC-M21). REQUIRED.
	DatabaseURL string
	// Tenant scopes this pod, as it scopes every durable service on this platform
	// (internal/pg.NewTenantPool refuses an empty one). REQUIRED.
	Tenant string
}

// Load reads TV_SYNC_* environment variables with production-safe defaults.
func Load() (Config, error) {
	cfg := Config{
		Listen:        envOr("TV_SYNC_LISTEN", ":8091"),
		LogLevel:      parseLevel(os.Getenv("TV_SYNC_LOG_LEVEL")),
		OTLPEndpoint:  os.Getenv("TV_SYNC_OTLP_ENDPOINT"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:       envOr("TV_SYNC_NATS_URL", "nats://localhost:4222"),
		Source:        envOr("TV_SYNC_SOURCE", "tv-sync"),
		ConsumerGroup: envOr("TV_SYNC_CONSUMER_GROUP", "tv-sync"),
		PriceSubject:  envOr("TV_SYNC_PRICE_SUBJECT", "market.>"),
		DatabaseURL:   secret("TV_SYNC_DATABASE_URL"),
		Tenant:        os.Getenv("TV_SYNC_TENANT"),
	}
	if err := cfg.validateBook(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateBook REFUSES to start a tv-sync that cannot rebuild its book (EXEC-M21).
//
// tv-sync folds the fund's orders, executions and P&L into memory and serves them to
// TradingView. Its bus consumer is a DURABLE GROUP, so a restarted pod resumes at its last
// ack and never re-reads what it folded: with no fact log, a pod roll leaves the trader
// looking at an EMPTY ACCOUNT while the fund's positions sit open at the exchanges — and
// nothing can rebuild it, because the order stream ages off at 24h.
//
// An ephemeral book is not a degraded mode, it is a LIE with a delay on it. There is no
// default that makes this safe, so there is no default.
func (c Config) validateBook() error {
	if c.DatabaseURL == "" {
		return errors.New("tv-sync: TV_SYNC_DATABASE_URL (or _FILE) is required — without a durable fact log the fund's book is lost on every restart and cannot be rebuilt")
	}
	if c.Tenant == "" {
		return errors.New("tv-sync: TV_SYNC_TENANT is required — the fact log is tenant-scoped, and an unscoped session cannot read or write it")
	}
	return nil
}

// secret resolves a sensitive value, preferring a CSI/Vault file mount (SEC-01d: the path in
// <k>_FILE) over a plaintext <k> env var. The DSN carries database credentials and must never
// ride in a pod's env block.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
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
