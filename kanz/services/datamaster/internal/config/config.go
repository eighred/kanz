package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kanz-eng/kanz/pkg/secret"
)

// Config is the datamaster (golden-source) service runtime configuration, sourced
// from the environment. The service resolves a golden security master across
// vendor feeds and arbitrates multi-source prices with an exception queue
// (MASTER-01). A real Bloomberg/Refinitiv/ICE adapter plugs in behind the
// feed.RefSource seam at the composition root.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string

	// DatabaseURL selects the durable golden store and exception queue. Empty ⇒
	// the in-memory stores, which lose every operator override on restart.
	DatabaseURL string
	// Tenant is carried as the `app.tenant_id` GUC on every DB connection so
	// Postgres RLS scopes all reads/writes to it. Forgetting this GUC is what
	// silently emptied the accounting ledger.
	Tenant string

	// RefreshInterval is how often the projector re-resolves the golden records
	// from the vendor feeds.
	RefreshInterval time.Duration

	// RefFiles maps a vendor to its mounted reference extract
	// ("BLOOMBERG=/vendor/bloomberg/reference.csv,ICE=/vendor/ice/reference.csv").
	// This is how institutional reference data actually arrives: a scheduled
	// flat-file drop (Bloomberg Data License, Refinitiv DataScope) on a mounted
	// volume.
	RefFiles map[string]string
	// PriceFiles maps a vendor to its mounted price extract. A vendor may supply
	// reference data, prices, or both — they are different files with different
	// cadences.
	PriceFiles map[string]string
	// VendorPriority is each REFERENCE vendor's survivorship trust rank; LOWER
	// WINS (0 = most trusted). It has no default: which vendor's ISIN beats
	// which is a business decision about who the fund believes, and a silent
	// default would pick that for them. A reference vendor with no priority is a
	// startup error. Prices need none — a consensus is a median, not the opinion
	// of the most trusted source.
	VendorPriority map[string]int

	// AllowSim permits the dependency-free SimFeed to be the source of the golden
	// master. It is OFF by default and the service REFUSES TO START with a SimFeed
	// wired unless it is on: canned vendor data resolved into golden_records is
	// indistinguishable, once stored, from mastered reference data, and every
	// analytic downstream treats the golden record as trusted. Sim data is for a
	// developer laptop, not for anything that persists.
	AllowSim bool
}

// Load reads the configuration from the environment with production-safe
// defaults. A malformed vendor spec is an error, not a warning: a service that
// boots with half its vendors quietly unwired would master the book from whoever
// happened to parse.
func Load() (Config, error) {
	// Resolved before the literal so a declared-but-unreadable
	// DATAMASTER_DATABASE_URL_FILE stops Load HERE, on the same principle as the
	// vendor specs below: a half-wired boot is worse than no boot. The local helper
	// this replaces answered a failed mount with the plaintext env and then with "",
	// and "" selects the in-memory golden store and exception queue — so a broken
	// CSI mount would master the book into memory and throw away every operator
	// override at the next restart. See pkg/secret.
	databaseURL, err := secret.Read("DATAMASTER_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen:          envOr("DATAMASTER_LISTEN", ":8080"),
		LogLevel:        parseLevel(os.Getenv("DATAMASTER_LOG_LEVEL")),
		OTLPEndpoint:    os.Getenv("DATAMASTER_OTLP_ENDPOINT"),
		DatabaseURL:     databaseURL,
		Tenant:          envOr("DATAMASTER_TENANT", "__system__"),
		RefreshInterval: durationOr("DATAMASTER_REFRESH_INTERVAL", 5*time.Minute),
		AllowSim:        boolOr("DATAMASTER_ALLOW_SIM", false),
		RefFiles:        parseVendorMap(os.Getenv("DATAMASTER_REF_FILES")),
		PriceFiles:      parseVendorMap(os.Getenv("DATAMASTER_PRICE_FILES")),
	}
	if cfg.VendorPriority, err = parsePriorities(os.Getenv("DATAMASTER_VENDOR_PRIORITY")); err != nil {
		return Config{}, err
	}
	for vendor := range cfg.RefFiles {
		if _, ok := cfg.VendorPriority[vendor]; !ok {
			return Config{}, fmt.Errorf("config: reference vendor %q has no survivorship priority: "+
				"set DATAMASTER_VENDOR_PRIORITY (e.g. %q) — which vendor the fund believes is not something this service may decide for it", vendor, vendor+"=0")
		}
	}
	return cfg, nil
}

// parseVendorMap parses "VENDOR=/path,VENDOR=/path" — the same K=V,K=V form the
// OMS uses for its venue endpoints.
func parseVendorMap(s string) map[string]string {
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

// parsePriorities parses "BLOOMBERG=0,ICE=1" into survivorship ranks.
func parsePriorities(s string) (map[string]int, error) {
	out := map[string]int{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("config: vendor priority %q is not VENDOR=RANK", pair)
		}
		rank, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || rank < 0 {
			return nil, fmt.Errorf("config: vendor %q has priority %q: want a non-negative rank (0 = most trusted)", strings.TrimSpace(k), strings.TrimSpace(v))
		}
		out[strings.TrimSpace(k)] = rank
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func boolOr(key string, def bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return def
	}
	return v
}

func durationOr(key string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(key)))
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
