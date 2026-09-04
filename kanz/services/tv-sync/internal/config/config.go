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

	// Retention is how much folded history tv-sync keeps RESIDENT (#809).
	//
	// IT BOUNDS MEMORY, NOT THE RECORD. tv_facts keeps every fact forever; this
	// only says how much of the fold stays in RAM. Positions, average cost and
	// realized P&L are unaffected at any window — what leaves is folded into a
	// per-account baseline on the way out — so what this actually buys is the
	// depth of the order and execution BLOTTER and the depth of the bitemporal
	// as-of reads, against the pod's resident set. A pod restarted with a longer
	// window rebuilds the longer one from the log.
	//
	// NON-POSITIVE IS REFUSED rather than treated as "keep everything". Keeping
	// everything is what #809 filed: the heap grows with lifetime order and fill
	// volume until the pod is OOM-killed, and the operator interface disappears
	// exactly when an incident makes somebody want it. There is no off switch
	// here for the same reason there is none for the checkpoint interval.
	//
	// BELOW MinRetention IS ALSO REFUSED, and for a different and sharper reason
	// than memory — see that constant.
	Retention time.Duration
}

// MinRetention is the floor under TV_SYNC_RETENTION, and it is a CORRECTNESS
// floor rather than a comfort one.
//
// The resident window is also the fill-id dedup window: retention drops a fill id
// exactly when the execution it belongs to leaves memory (projection/retention.go
// evict). That set is the dedup for the DUAL fill path — the synchronous venue
// placement response and the asynchronous user-data websocket echo of the SAME
// fill, published as two separate FACTs with two event_ids, so tv_facts's
// (tenant_id, event_id) primary key does not deduplicate them. An id dropped
// before its echo arrives is a fill folded twice: a doubled position and a
// doubled realized P&L on the surface a trader acts from.
//
// 24h, for two reasons that agree:
//
//   - The venue adapters re-report a fill after a user-data websocket reconnect
//     and on the periodic REST reconciliation pass (services/venue-binance/
//     internal/binance/binance_recon.go). Both are minutes-to-hours horizons even
//     through an outage; a day clears them with room.
//   - The EXECUTION stream itself is provisioned with 24h retention
//     (infra/nats/bootstrap-job.yaml), so nothing older than that can still be
//     delivered to this consumer by any path at all.
//
// IT IS A CHOSEN BOUND, NOT A MEASURED ONE. Nothing on this platform instruments
// the actual sync-response-to-echo delay, so the floor is argued from the two
// horizons above rather than from a distribution. Lowering it is a decision about
// double-counted fills, which is why it is a constant with this comment on it
// instead of a default somebody can quietly halve.
const MinRetention = 24 * time.Hour

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

	// 168h: a week of blotter and of as-of depth. The number is a memory
	// trade-off rather than a correctness one (every fold stays exact at any
	// window), so it is set high enough that an operator investigating last
	// week's trading does not hit the horizon, and low enough that the resident
	// set is a function of a week's volume rather than of the fund's life.
	retention, err := time.ParseDuration(env.Or("TV_SYNC_RETENTION", "168h"))
	if err != nil {
		return Config{}, fmt.Errorf("TV_SYNC_RETENTION: %w", err)
	}
	if retention < MinRetention {
		return Config{}, fmt.Errorf("TV_SYNC_RETENTION must be at least %s, got %s: the resident "+
			"window is also the fill-id dedup window for the DUAL fill path (the synchronous venue "+
			"response and the asynchronous websocket echo of the same fill), and an id dropped "+
			"before its echo arrives is a fill folded TWICE — a doubled position and a doubled "+
			"realized P&L. A non-positive value is refused by the same check: keeping everything is "+
			"the unbounded heap #809 exists to end, and it fails by OOM-killing the pod rather than "+
			"by saying anything", MinRetention, retention)
	}
	cfg.Retention = retention

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
