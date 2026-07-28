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

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
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
	SPIFFESocket string

	NATSURL string
	Source  string

	ReplayWindow time.Duration
	Allowlist    []*net.IPNet

	// RedisURL backs the CROSS-POD nonce store (EXEC-M17). The nonce cache is the
	// replay defence at the internet-facing perimeter, and in-process it is per-pod:
	// two replicas are two caches, so a re-delivered TradingView alert landing on the
	// other pod is admitted a SECOND time and fans out a SECOND set of orders. Nothing
	// downstream can catch that — a fresh claim mints a fresh signal_id, so the OMS's
	// admission gate sees two different orders.
	//
	// Empty ⇒ the in-process store, which is correct for EXACTLY ONE REPLICA and is
	// therefore what pins this service — the one the internet talks to — to a single
	// pod. Set this (with -tags redis) and the pin can be lifted.
	RedisURL string
	// AllowInProcessNonce is the EXPLICIT admission that the replay defence is per-pod.
	// Without it, and without a RedisURL, the service REFUSES TO START: a per-pod replay
	// defence reachable by FORGETTING to configure Redis is indistinguishable from a
	// correct one, and the deployment that forgot is the one running N replicas.
	AllowInProcessNonce bool
	// CloudflareOnly locks the webhook to the Cloudflare Signing Relay edge: the
	// peer (Allowlist, set to Cloudflare's CIDR ranges) must be a Cloudflare IP,
	// and every request must carry CF-Connecting-IP — public traffic bypassing
	// the relay is rejected before any processing (M3.8).
	CloudflareOnly bool

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
	// Resolved before the literal so a DECLARED-but-unreadable secret mount stops
	// Load here. The local helper this replaces answered a failed mount with the
	// plaintext env var and then with "", and "" here selects the IN-PROCESS nonce
	// store — see RedisURL above. A deployment that mounts a Redis URL and also
	// carries AllowInProcessNonce (a leftover from its single-replica days) would
	// therefore survive a broken Vault mount by silently falling back to a per-pod
	// replay defence, on N replicas, at the internet-facing perimeter — the exact
	// state that admits a re-delivered alert twice and fans out a second set of
	// orders. See pkg/secret.
	redisURL, err := secret.Read("WEBHOOK_INGEST_REDIS_URL")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen:         envOr("WEBHOOK_INGEST_LISTEN", ":8090"),
		LogLevel:       parseLevel(os.Getenv("WEBHOOK_INGEST_LOG_LEVEL")),
		OTLPEndpoint:   os.Getenv("WEBHOOK_INGEST_OTLP_ENDPOINT"),
		SPIFFESocket:   os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:        envOr("WEBHOOK_INGEST_NATS_URL", "nats://localhost:4222"),
		Source:         envOr("WEBHOOK_INGEST_SOURCE", "webhook-ingest"),
		ReplayWindow:   parseDuration(os.Getenv("WEBHOOK_INGEST_REPLAY_WINDOW"), 5*time.Minute),
		CloudflareOnly: os.Getenv("WEBHOOK_INGEST_CLOUDFLARE_ONLY") == "1",

		RedisURL:            redisURL,
		AllowInProcessNonce: os.Getenv("WEBHOOK_INGEST_ALLOW_INPROCESS_NONCE") == "true",
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
