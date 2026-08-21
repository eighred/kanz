// Package config loads market-ingest's runtime configuration from the
// environment. market-ingest is an outbound edge (it dials exchange depth feeds
// and publishes to the bus); it has no inbound trading API, so there is no
// bootstrap secrets file — only infra + the instrument set to track. Real venue
// depth sources bind at the composition root behind per-venue build tags; the
// default binary tracks the instruments with the vendor-free simulator.
package config

import (
	"github.com/eighred/kanz/internal/env"
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

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string

	NATSURL string
	Source  string

	// Instruments is the set of canonical instrument_ids to track (one book +
	// engine per instrument PER VENUE). Empty ⇒ the service logs and idles
	// (deny-by-default: it never invents instruments).
	Instruments []string

	// Tenant is stamped on every market.v1 FACT this service publishes (MT-01b).
	//
	// It is REQUIRED, not optional: bus.Validate rejects an envelope without a
	// tenant_id, so without this the service connects to the exchange, folds the
	// book correctly, and then fails EVERY publish — while /readyz keeps returning
	// 200. It ingests perfectly and emits nothing.
	Tenant string

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

	// CoverageMaxSilence is how long a feed subscription may go without proving
	// itself alive before the recorder stops crediting it as observed (#591).
	//
	// IT IS A PROPERTY OF THE HEARTBEAT CADENCE, NOT A TASTE SETTING. Both venue
	// trade sources heartbeat at trades.HeartbeatInterval (20s), so this must be
	// long enough to survive one lost beat plus jitter and short enough that a
	// whole silent bucket cannot be credited off one observation on either side
	// of it. coverage.NewRecorder REFUSES a value >= one bucket rather than
	// accepting a tolerance that would vouch for a dead feed.
	CoverageMaxSilence time.Duration
}

// Load reads and validates the environment.
func Load() Config {
	return Config{
		Listen:           env.Or("MARKET_INGEST_LISTEN", ":8091"),
		LogLevel:         env.ParseLevelOr(os.Getenv("MARKET_INGEST_LOG_LEVEL"), slog.LevelInfo),
		OTLPEndpoint:     os.Getenv("MARKET_INGEST_OTLP_ENDPOINT"),
		SPIFFESocket:     os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:          env.Or("MARKET_INGEST_NATS_URL", "nats://localhost:4222"),
		Source:           env.Or("MARKET_INGEST_SOURCE", "market-ingest"),
		Tenant:           env.Or("MARKET_INGEST_TENANT", "__system__"),
		Instruments:      parseList(os.Getenv("MARKET_INGEST_INSTRUMENTS")),
		AllowSim:         os.Getenv("MARKET_INGEST_ALLOW_SIM") == "true",
		SnapshotInterval: parseDuration(os.Getenv("MARKET_INGEST_SNAPSHOT_INTERVAL"), time.Second),
		SnapshotDepth:    parseInt(os.Getenv("MARKET_INGEST_SNAPSHOT_DEPTH"), 20),

		BinanceMIC:      env.Or("MARKET_INGEST_BINANCE_MIC", "BINANCE"),
		BinanceWSBase:   env.Or("MARKET_INGEST_BINANCE_WS_BASE", "wss://stream.binance.com:9443"),
		BinanceRESTBase: env.Or("MARKET_INGEST_BINANCE_REST_BASE", "https://api.binance.com"),
		BinanceSymbols:  parseSymbolMap(os.Getenv("MARKET_INGEST_BINANCE_SYMBOLS")),

		OKXMIC:     env.Or("MARKET_INGEST_OKX_MIC", "OKX"),
		OKXWSURL:   env.Or("MARKET_INGEST_OKX_WS_URL", "wss://ws.okx.com:8443/ws/v5/public"),
		OKXSymbols: parseSymbolMap(os.Getenv("MARKET_INGEST_OKX_SYMBOLS")),

		DepthLimit:     parseInt(os.Getenv("MARKET_INGEST_DEPTH_LIMIT"), 1000),
		DNSTTL:         parseDuration(os.Getenv("MARKET_INGEST_DNS_TTL"), 5*time.Minute),
		TradeRetention: parseDuration(os.Getenv("MARKET_INGEST_TRADE_RETENTION"), time.Minute),
		// 45s = two 20s heartbeats plus jitter, and under the 1-minute bucket the
		// recorder enforces. A misconfiguration here fails at construction, not on
		// the first quiet minute.
		CoverageMaxSilence: parseDuration(os.Getenv("MARKET_INGEST_COVERAGE_MAX_SILENCE"), 45*time.Second),
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
