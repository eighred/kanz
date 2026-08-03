package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/secret"
)

// Config is the accounting (IBOR) service runtime configuration, sourced from the
// environment. The service folds OMS-01 fills and cash/corporate-action events
// into the book-of-record and serves point-in-time NAV and custodian
// reconciliation. The journal store defaults to in-memory (a durable backend
// plugs in behind ledger.Store at the composition root, the PERS-01 stance); the
// bus consumer that feeds the journal is wired there too, so the default boot
// serves the read/reconcile endpoints without a broker.
type Config struct {
	Listen       string
	LogLevel     slog.Level
	BaseCurrency string

	// NATSURL is the live spine the fill-folding consumer subscribes to
	// (WIRE-01b). Empty ⇒ no consumer (the default; the service serves the
	// read/reconcile endpoints on an in-memory journal without a broker).
	NATSURL string
	// Source is the consumer identity (logging / durable consumer name).
	Source string
	// ConsumerGroup is the durable consumer name the fill subjects subscribe under.
	ConsumerGroup string
	// FillSubjects are the order.v1 fill FACT subjects folded into the journal.
	FillSubjects []string
	// CashSubjects are the accounting.v1 cash-movement FACT subjects folded into
	// the journal (WIRE-01f: subscription/redemption/fee). Default the
	// accounting.cash wildcard. Comma-separated.
	CashSubjects []string

	// DatabaseURL is the Postgres DSN for the durable IBOR journal (PARITY-02a).
	// Empty ⇒ the in-memory store: folding works but loses the journal on
	// restart (local/dev).
	DatabaseURL string
	// Tenant is carried as the `app.tenant_id` GUC on every DB connection.
	//
	// It is REQUIRED, not decorative. 0001_ledger.sql runs FORCE ROW LEVEL
	// SECURITY with a policy on current_setting('app.tenant_id'), and Append
	// inserts VALUES (current_setting('app.tenant_id'), ...). Without the GUC set,
	// against the non-superuser role production requires, every write ERRORS and
	// every read returns ZERO ROWS — the ledger silently holds nothing.
	//
	// The Postgres tests set this GUC in their own pool (AfterConnect) and pass.
	// main.go never did. That is why this was invisible.
	Tenant string

	// SnapshotInterval is how often the ledger checkpoint job runs (#229).
	//
	// It is what keeps NAV bounded: without a checkpoint, materializing a book
	// reads the portfolio's ENTIRE lifetime journal on every request, so the
	// service degrades with AGE rather than with load. The job only runs with a
	// durable store — the in-memory journal dies with the pod, so checkpointing
	// it buys nothing.
	//
	// ACCOUNTING_SNAPSHOT_INTERVAL, a Go duration. A zero or negative value
	// DISABLES the job, which Load refuses to infer from a malformed setting: a
	// typo must not silently reinstate the unbounded read.
	SnapshotInterval time.Duration
	// SnapshotBatch caps the portfolios one checkpoint pass will fold
	// (ACCOUNTING_SNAPSHOT_BATCH). Each is a full journal read, so an unbounded
	// pass against a large estate would hold a pool connection for as long as it
	// took — the shape of problem the job exists to remove.
	SnapshotBatch int

	// Live FX for multi-currency NAV (WIRE-01d): a latest-rate cache folded from
	// the market.v1 FX spine, so the NAV endpoint values a multi-currency book
	// with no `fx` in the request. Off unless FXPairs is configured AND a broker
	// is set. A per-request `fx` still overrides the live rates (client-supplied).
	//
	// FXPairs is the raw FX-pair reference spec (ACCOUNTING_FX_PAIRS):
	// comma-separated `<instrument_id>:<foreign_currency>` entries the cache
	// records rates for, parsed by fxfeed.ParsePairs.
	FXPairs string
	// FXSubjects are the market.v1 FX subjects the cache subscribes
	// (ACCOUNTING_FX_SUBJECTS); default the MARKET FX wildcard. Comma-separated.
	FXSubjects []string
	// InstrumentCurrency is the raw instrument→reference-currency spec
	// (ACCOUNTING_INSTRUMENT_CURRENCY): comma-separated `<instrument>:<currency>`
	// entries — the security-master join the multi-currency valuation needs. A
	// per-request map overrides it. The datamaster-backed source plugs in here.
	InstrumentCurrency string

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
}

// DefaultFXSubjects is the FX-cache subscription when none is configured.
var DefaultFXSubjects = []string{"market.fx.>"}

// DefaultFillSubjects are the order.v1 fill FACT subjects the consumer folds
// (mirrors the consume package's wire subjects — the same "mirror, don't import
// the OMS internal package" stance).
var DefaultFillSubjects = []string{"order.order.filled", "order.order.partially_filled"}

// DefaultCashSubjects are the accounting.v1 cash-movement FACT subjects the
// consumer folds (WIRE-01f) — the wildcard over the cashmove.Publisher subjects.
var DefaultCashSubjects = []string{"accounting.cash.>"}

// DefaultSnapshotInterval is how often the ledger checkpoint job runs when
// ACCOUNTING_SNAPSHOT_INTERVAL is unset. Five minutes bounds the worst-case
// tail a NAV request folds to five minutes of fills for one portfolio, at a
// cost of one full journal fold per stale portfolio per five minutes on a
// background goroutine. It is ON by default deliberately: the read path is
// unbounded without it, and an operator who never heard of this setting should
// get the bounded behaviour.
const DefaultSnapshotInterval = 5 * time.Minute

// DefaultSnapshotBatch is the most portfolios one checkpoint pass folds.
const DefaultSnapshotBatch = 64

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	// Resolved before the literal so an ACCOUNTING_DATABASE_URL_FILE that will not
	// read stops Load HERE. The local helper this replaces answered an unreadable
	// mount with the plaintext env and then with "", and an empty DSN is not inert
	// in this service: it SELECTS the in-memory journal. A broken Vault mount would
	// therefore have booked the fund's book-of-record into a map that dies with the
	// pod, and reported a clean start doing it. See pkg/secret.
	databaseURL, err := secret.Read("ACCOUNTING_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	subjects := splitList(os.Getenv("ACCOUNTING_FILL_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultFillSubjects
	}
	fxSubjects := splitList(os.Getenv("ACCOUNTING_FX_SUBJECTS"))
	if len(fxSubjects) == 0 {
		fxSubjects = DefaultFXSubjects
	}
	cashSubjects := splitList(os.Getenv("ACCOUNTING_CASH_SUBJECTS"))
	if len(cashSubjects) == 0 {
		cashSubjects = DefaultCashSubjects
	}

	snapshotInterval, err := durationOr("ACCOUNTING_SNAPSHOT_INTERVAL", DefaultSnapshotInterval)
	if err != nil {
		return Config{}, err
	}
	snapshotBatch, err := intOr("ACCOUNTING_SNAPSHOT_BATCH", DefaultSnapshotBatch)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen:        envOr("ACCOUNTING_LISTEN", ":8080"),
		LogLevel:      parseLevel(os.Getenv("ACCOUNTING_LOG_LEVEL")),
		BaseCurrency:  envOr("ACCOUNTING_BASE_CURRENCY", "USD"),
		NATSURL:       os.Getenv("ACCOUNTING_NATS_URL"),
		Source:        envOr("ACCOUNTING_SOURCE", "accounting"),
		ConsumerGroup: envOr("ACCOUNTING_CONSUMER_GROUP", "accounting"),
		FillSubjects:  subjects,
		CashSubjects:  cashSubjects,
		DatabaseURL:   databaseURL,
		Tenant:        envOr("ACCOUNTING_TENANT", "__system__"),

		SnapshotInterval: snapshotInterval,
		SnapshotBatch:    snapshotBatch,

		OTLPEndpoint: os.Getenv("ACCOUNTING_OTLP_ENDPOINT"),
		SPIFFESocket: os.Getenv("SPIFFE_ENDPOINT_SOCKET"),

		FXPairs:            os.Getenv("ACCOUNTING_FX_PAIRS"),
		FXSubjects:         fxSubjects,
		InstrumentCurrency: os.Getenv("ACCOUNTING_INSTRUMENT_CURRENCY"),
	}, nil
}

// splitList parses a comma-separated env value into a trimmed, non-empty slice.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// durationOr parses a Go duration from the environment, or returns def when
// unset. A MALFORMED value is an error, never the default: silently falling
// back would leave the checkpoint job on a schedule the operator did not choose
// while the deployment reported a clean start.
func durationOr(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a duration: %w", key, raw, err)
	}
	return d, nil
}

// intOr parses an int from the environment, or returns def when unset. A
// malformed value is an error, for the same reason as durationOr.
func intOr(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not an integer: %w", key, raw, err)
	}
	return n, nil
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
