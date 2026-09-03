// Package config loads tv-sync's runtime configuration from the environment.
// tv-sync is a pure projection — it needs only where the bus is and where to
// serve the Broker API.
package config

import (
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/secret"
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
	// PriceSubjects are the market-data subjects tv-sync folds into its
	// MarkSource for live unrealized P&L (M3.5).
	//
	// This mirrors services/oms/internal/config/config.go's PriceSubjects field
	// (same default, same comma-separated parsing, same empty-is-an-error
	// posture) rather than inventing a second convention — the OMS's copy was
	// added first, to close the identical bug there. The two packages cannot
	// share code (Go's internal rule makes anything under
	// services/oms/internal reachable only from the OMS, and the reverse for
	// tv-sync), so this is a deliberate parallel implementation, not drift.
	//
	// The default is the mark fold's CONSUMPTION SET expressed as subjects,
	// not the convenient wildcard "market.>" this field used to default to.
	// market.v1.MarketDataEvent publishes on market.<assetClass>.<variant>
	// (services/market-data/internal/feed/bussink.go), and the fold
	// (internal/marketdata/mark) uses Trade and Quote only. NATS `*` matches
	// exactly one token, so these two subjects cover every asset class —
	// present and future — while structurally excluding market.*.bar and,
	// critically, market.book.snapshot: that subject carries an
	// OrderBookSnapshot, a message WIRE-COMPATIBLE with MarketDataEvent by
	// construction, and a one-sided (bids-only) snapshot silently decoded as
	// a Trade at the deepest resting bid price — poisoning tv-sync's
	// unrealized P&L with a mark below mid. mark.Source now also refuses that
	// payload by its EventType regardless of subscription (the durable half
	// of the fix), but this subject list is the first, cheaper line of
	// defense: it stops the highest-volume stream in the estate from being
	// decoded and discarded at all.
	PriceSubjects []string

	// DatabaseURL is the durable fact log (EXEC-M21). REQUIRED.
	DatabaseURL string
	// Tenant scopes this pod, as it scopes every durable service on this platform
	// (internal/pg.NewTenantPool refuses an empty one). REQUIRED.
	Tenant string

	// CheckpointInterval is how often the fold's state is written so the next boot
	// resumes from it rather than replaying the fund's entire history (#809).
	//
	// NON-POSITIVE IS REFUSED rather than treated as "off". There is no off switch:
	// a pod that never checkpoints boots by replaying everything, which is the
	// unbounded startup this exists to end — and it degrades SILENTLY, because the
	// book it rebuilds is correct, just slower to reach every time.
	CheckpointInterval time.Duration
}

// Load reads TV_SYNC_* environment variables with production-safe defaults.
func Load() (Config, error) {
	// The DSN carries database credentials and must never ride in a pod's env
	// block, so a CSI/Vault file mount (SEC-01d) wins over the plaintext var.
	// Resolved before the literal so an unreadable mount stops Load HERE rather
	// than resolving to "": that empty value reaches validateBook below, which
	// refuses the boot with "TV_SYNC_DATABASE_URL is required" — the wrong
	// diagnosis for a DSN that WAS configured, and one that points an operator at
	// the manifest instead of at the mount that failed. See pkg/secret.
	databaseURL, err := secret.Read("TV_SYNC_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen:        env.Or("TV_SYNC_LISTEN", ":8091"),
		LogLevel:      env.ParseLevelOr(os.Getenv("TV_SYNC_LOG_LEVEL"), slog.LevelInfo),
		OTLPEndpoint:  os.Getenv("TV_SYNC_OTLP_ENDPOINT"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:       env.Or("TV_SYNC_NATS_URL", "nats://localhost:4222"),
		Source:        env.Or("TV_SYNC_SOURCE", "tv-sync"),
		ConsumerGroup: env.Or("TV_SYNC_CONSUMER_GROUP", "tv-sync"),
		PriceSubjects: splitSubjects(priceSubjectsEnv()),
		DatabaseURL:   databaseURL,
		Tenant:        os.Getenv("TV_SYNC_TENANT"),
	}

	// 60s: a full replay of the tail is at most a minute of facts, and a checkpoint
	// of a book this size is cheap enough that the interval is bounded by how much
	// re-fold a restart should ever have to pay rather than by the write cost.
	interval, err := time.ParseDuration(env.Or("TV_SYNC_CHECKPOINT_INTERVAL", "60s"))
	if err != nil {
		return Config{}, fmt.Errorf("TV_SYNC_CHECKPOINT_INTERVAL: %w", err)
	}
	if interval <= 0 {
		return Config{}, fmt.Errorf("TV_SYNC_CHECKPOINT_INTERVAL must be positive, got %s: a pod that "+
			"never checkpoints rebuilds the fund's ENTIRE history on every boot, which is the "+
			"unbounded startup #809 exists to end — and it fails silently, because the book is "+
			"correct and only the outage is longer", interval)
	}
	cfg.CheckpointInterval = interval
	if len(cfg.PriceSubjects) == 0 {
		return Config{}, fmt.Errorf("TV_SYNC_PRICE_SUBJECTS: at least one subject is required; " +
			"a pod subscribing to nothing folds no marks and silently shows no unrealized P&L")
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

// priceSubjectsEnv reads TV_SYNC_PRICE_SUBJECTS WITHOUT trimming first,
// unlike this package's envOr. Trimming here would let a whitespace-only
// override ("TV_SYNC_PRICE_SUBJECTS= ") silently fall back to the default —
// exactly the config typo the empty-subjects check in Load exists to catch.
// splitSubjects does its own per-entry trimming; a whitespace-only override
// must reach it intact, not vanish before it. Mirrors the raw
// os.LookupEnv check services/oms/internal/config/config.go uses for the
// identical field.
func priceSubjectsEnv() string {
	if v, ok := os.LookupEnv("TV_SYNC_PRICE_SUBJECTS"); ok && v != "" {
		return v
	}
	return "market.*.trade,market.*.quote"
}

// splitSubjects parses a comma-separated subject list, trimming whitespace
// and dropping empty entries. An all-empty input yields an empty slice, which
// Load rejects — subscribing to nothing is a silent P&L outage, not a
// default. Mirrors services/oms/internal/config/config.go's splitSubjects;
// see the PriceSubjects doc comment above for why this is a parallel
// implementation rather than a shared one.
func splitSubjects(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
