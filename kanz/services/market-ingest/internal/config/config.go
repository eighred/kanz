// Package config loads market-ingest's runtime configuration from the
// environment. market-ingest is an outbound edge (it dials exchange depth feeds
// and publishes to the bus); it has no inbound trading API, so there is no
// bootstrap secrets file — only infra + the instrument set to track. Real venue
// depth sources bind at the composition root behind per-venue build tags; the
// default binary tracks the instruments with the vendor-free simulator.
package config

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

// Config is the resolved configuration.
type Config struct {
	Listen       string // health/readiness listen address
	LogLevel     slog.Level
	OTLPEndpoint string

	NATSURL string
	Source  string

	// Instruments is the set of canonical instrument_ids to track (one book +
	// engine per instrument PER VENUE). Empty ⇒ the service logs and idles
	// (deny-by-default: it never invents instruments).
	Instruments []string

	// AllowSim permits the deterministic SimFeed to stand in when no exchange
	// symbols are mapped. It is OFF by default and must be set deliberately.
	//
	// SimFeed does not read a market — it GENERATES prices. Those prices publish
	// to the bus as market.v1 FACTs and everything downstream marks positions on
	// them: risk, NAV, the OMS's pricing. Stamping MIC "SIM" labels them but
	// nothing filters on it. Ingesting invented prices as though they were
	// observed is exactly the "never fabricate — degrade or emit a correcting
	// FACT, never inject data" rule, so it cannot be something you get by
	// forgetting to set a symbol map.
	AllowSim bool

	SnapshotInterval time.Duration
	SnapshotDepth    int

	// --- live venue depth feeds (bound behind per-venue build tags) ---
	//
	// Exchange depth is PUBLIC market data, so unlike the OMS connectors these
	// carry no API key or secret. An instrument absent from a venue's symbol map is
	// simply not tracked on that venue — the edge never invents a symbol.

	BinanceMIC      string
	BinanceWSBase   string            // websocket origin
	BinanceRESTBase string            // REST origin (the depth-snapshot anchor)
	BinanceSymbols  map[string]string // instrument_id -> venue symbol (BTC-USD=BTCUSDT)

	OKXMIC     string
	OKXWSURL   string            // public v5 websocket
	OKXSymbols map[string]string // instrument_id -> venue instId (BTC-USD=BTC-USDT)

	// DepthLimit is the REST snapshot depth Binance anchors on (<=0 ⇒ 1000).
	DepthLimit int
	// DNSTTL is the DNS-bypass cache TTL on the live exchange path.
	DNSTTL time.Duration
	// TradeRetention bounds the in-memory trade tape (the volume-delta window).
	TradeRetention time.Duration
}

// Load reads and validates the environment.
func Load() Config {
	return Config{
		Listen:           envOr("MARKET_INGEST_LISTEN", ":8091"),
		LogLevel:         parseLevel(os.Getenv("MARKET_INGEST_LOG_LEVEL")),
		OTLPEndpoint:     os.Getenv("MARKET_INGEST_OTLP_ENDPOINT"),
		NATSURL:          envOr("MARKET_INGEST_NATS_URL", "nats://localhost:4222"),
		Source:           envOr("MARKET_INGEST_SOURCE", "market-ingest"),
		Instruments:      parseList(os.Getenv("MARKET_INGEST_INSTRUMENTS")),
		AllowSim:         os.Getenv("MARKET_INGEST_ALLOW_SIM") == "true",
		SnapshotInterval: parseDuration(os.Getenv("MARKET_INGEST_SNAPSHOT_INTERVAL"), time.Second),
		SnapshotDepth:    parseInt(os.Getenv("MARKET_INGEST_SNAPSHOT_DEPTH"), 20),

		BinanceMIC:      envOr("MARKET_INGEST_BINANCE_MIC", "BINANCE"),
		BinanceWSBase:   envOr("MARKET_INGEST_BINANCE_WS_BASE", "wss://stream.binance.com:9443"),
		BinanceRESTBase: envOr("MARKET_INGEST_BINANCE_REST_BASE", "https://api.binance.com"),
		BinanceSymbols:  parseSymbolMap(os.Getenv("MARKET_INGEST_BINANCE_SYMBOLS")),

		OKXMIC:     envOr("MARKET_INGEST_OKX_MIC", "OKX"),
		OKXWSURL:   envOr("MARKET_INGEST_OKX_WS_URL", "wss://ws.okx.com:8443/ws/v5/public"),
		OKXSymbols: parseSymbolMap(os.Getenv("MARKET_INGEST_OKX_SYMBOLS")),

		DepthLimit:     parseInt(os.Getenv("MARKET_INGEST_DEPTH_LIMIT"), 1000),
		DNSTTL:         parseDuration(os.Getenv("MARKET_INGEST_DNS_TTL"), 5*time.Minute),
		TradeRetention: parseDuration(os.Getenv("MARKET_INGEST_TRADE_RETENTION"), time.Minute),
	}
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

func parseList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
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
