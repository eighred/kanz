package config

import (
	"log/slog"
	"os"
	"strings"
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

	// DatabaseURL is the Postgres DSN for the durable IBOR journal (PARITY-02a).
	// Empty ⇒ the in-memory store: folding works but loses the journal on
	// restart (local/dev).
	DatabaseURL string

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
}

// DefaultFXSubjects is the FX-cache subscription when none is configured.
var DefaultFXSubjects = []string{"market.fx.>"}

// DefaultFillSubjects are the order.v1 fill FACT subjects the consumer folds
// (mirrors the consume package's wire subjects — the same "mirror, don't import
// the OMS internal package" stance).
var DefaultFillSubjects = []string{"order.order.filled", "order.order.partially_filled"}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	subjects := splitList(os.Getenv("ACCOUNTING_FILL_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultFillSubjects
	}
	fxSubjects := splitList(os.Getenv("ACCOUNTING_FX_SUBJECTS"))
	if len(fxSubjects) == 0 {
		fxSubjects = DefaultFXSubjects
	}
	return Config{
		Listen:        envOr("ACCOUNTING_LISTEN", ":8080"),
		LogLevel:      parseLevel(os.Getenv("ACCOUNTING_LOG_LEVEL")),
		BaseCurrency:  envOr("ACCOUNTING_BASE_CURRENCY", "USD"),
		NATSURL:       os.Getenv("ACCOUNTING_NATS_URL"),
		Source:        envOr("ACCOUNTING_SOURCE", "accounting"),
		ConsumerGroup: envOr("ACCOUNTING_CONSUMER_GROUP", "accounting"),
		FillSubjects:  subjects,
		DatabaseURL:   secret("ACCOUNTING_DATABASE_URL"),
		OTLPEndpoint:  os.Getenv("ACCOUNTING_OTLP_ENDPOINT"),

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

// secret resolves a sensitive value, preferring a CSI/Vault file mount
// (SEC-01d: the path in <k>_FILE) over a plaintext <k> env var.
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
