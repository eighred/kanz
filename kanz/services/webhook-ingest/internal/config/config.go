// Package config loads webhook-ingest's runtime configuration. Infra settings
// come from the environment; the trading configuration (per-strategy secrets,
// symbol map, fund→venue allocation, and the M1 sim price/equity providers)
// comes from a bootstrap JSON file. In later milestones the price/equity
// providers bind to the live market-data and accounting projections at the
// composition root; the bootstrap file is the M1 simulation source.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"time"

	"github.com/kanz-eng/kanz/services/webhook-ingest/internal/dec"
	"github.com/kanz-eng/kanz/services/webhook-ingest/internal/ingest"
)

// Config is the resolved configuration.
type Config struct {
	Listen       string
	LogLevel     slog.Level
	OTLPEndpoint string

	NATSURL string
	Source  string

	ReplayWindow time.Duration
	Allowlist    []*net.IPNet

	// Trading configuration from the bootstrap file.
	Secrets ingest.StaticSecrets
	Symbols ingest.StaticSymbols
	Alloc   ingest.StaticAllocation
	Prices  ingest.StaticPrices
	Equity  ingest.StaticEquity

	MaxSize     *big.Rat
	MaxLeverage *big.Rat
}

// bootstrap is the JSON shape of the trading config file. All decimals are
// strings (exact; no float).
type bootstrap struct {
	Strategies  map[string]string        `json:"strategies"` // strategy_id -> hmac secret
	Symbols     map[string]string        `json:"symbols"`    // tv symbol -> instrument_id
	Funds       map[string][]venueWeight `json:"funds"`      // fund_id -> allocation
	Prices      map[string]string        `json:"prices"`     // instrument_id -> price
	Equity      map[string]string        `json:"equity"`     // fund_id -> NAV
	MaxSize     string                   `json:"max_size"`
	MaxLeverage string                   `json:"max_leverage"`
}

type venueWeight struct {
	Venue  string `json:"venue"`
	Weight string `json:"weight"`
}

// Load reads env + the bootstrap file (WEBHOOK_INGEST_CONFIG) and validates it.
func Load() (Config, error) {
	cfg := Config{
		Listen:       envOr("WEBHOOK_INGEST_LISTEN", ":8090"),
		LogLevel:     parseLevel(os.Getenv("WEBHOOK_INGEST_LOG_LEVEL")),
		OTLPEndpoint: os.Getenv("WEBHOOK_INGEST_OTLP_ENDPOINT"),
		NATSURL:      envOr("WEBHOOK_INGEST_NATS_URL", "nats://localhost:4222"),
		Source:       envOr("WEBHOOK_INGEST_SOURCE", "webhook-ingest"),
		ReplayWindow: parseDuration(os.Getenv("WEBHOOK_INGEST_REPLAY_WINDOW"), 5*time.Minute),
	}
	allow, err := parseAllowlist(os.Getenv("WEBHOOK_INGEST_IP_ALLOWLIST"))
	if err != nil {
		return Config{}, err
	}
	cfg.Allowlist = allow

	path := os.Getenv("WEBHOOK_INGEST_CONFIG")
	if path == "" {
		return Config{}, errors.New("WEBHOOK_INGEST_CONFIG is required (path to the bootstrap trading config)")
	}
	b, err := loadBootstrap(path)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.applyBootstrap(b); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadBootstrap(path string) (*bootstrap, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap config: %w", err)
	}
	var b bootstrap
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse bootstrap config: %w", err)
	}
	return &b, nil
}

func (cfg *Config) applyBootstrap(b *bootstrap) error {
	cfg.Secrets = ingest.StaticSecrets(b.Strategies)
	cfg.Symbols = ingest.StaticSymbols(b.Symbols)

	cfg.Prices = make(ingest.StaticPrices, len(b.Prices))
	for inst, s := range b.Prices {
		r, err := dec.ParseRat(s)
		if err != nil {
			return fmt.Errorf("price for %s: %w", inst, err)
		}
		cfg.Prices[inst] = r
	}
	cfg.Equity = make(ingest.StaticEquity, len(b.Equity))
	for fund, s := range b.Equity {
		r, err := dec.ParseRat(s)
		if err != nil {
			return fmt.Errorf("equity for %s: %w", fund, err)
		}
		cfg.Equity[fund] = r
	}
	cfg.Alloc = make(ingest.StaticAllocation, len(b.Funds))
	for fund, legs := range b.Funds {
		out := make([]ingest.VenueAllocation, 0, len(legs))
		for _, l := range legs {
			w, err := dec.ParseRat(l.Weight)
			if err != nil {
				return fmt.Errorf("weight for fund %s venue %s: %w", fund, l.Venue, err)
			}
			out = append(out, ingest.VenueAllocation{Venue: l.Venue, Weight: w})
		}
		cfg.Alloc[fund] = out
	}
	if b.MaxSize != "" {
		r, err := dec.ParseRat(b.MaxSize)
		if err != nil {
			return fmt.Errorf("max_size: %w", err)
		}
		cfg.MaxSize = r
	}
	if b.MaxLeverage != "" {
		r, err := dec.ParseRat(b.MaxLeverage)
		if err != nil {
			return fmt.Errorf("max_leverage: %w", err)
		}
		cfg.MaxLeverage = r
	}
	return nil
}

func parseAllowlist(s string) ([]*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			part += "/32" // bare IP ⇒ host CIDR
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid allowlist CIDR %q: %w", part, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
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
