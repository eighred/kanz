package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/kanz-eng/kanz/services/oms/internal/order"
)

// Config is the oms runtime configuration, sourced from the environment so it
// composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	Listen   string
	LogLevel slog.Level
	Source   string
	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string
	// NATSURL is the spine. Empty ⇒ the service runs HTTP/probes only (no
	// command consumption), a read-only/degraded posture.
	NATSURL string
	// ConsumerGroup is the durable group the command + fill consumers share.
	ConsumerGroup string
	// DatabaseURL is the durable order store (EXEC-M7c). Empty ⇒ the in-memory
	// store, whose admission gate is a process-local mutex — correct for tests
	// and a single replica ONLY. A multi-replica deployment MUST set this: two
	// pods over two in-memory maps both admit the same order_id and both route
	// it to the venue.
	DatabaseURL string
	// Tenant is the owning tenant of this OMS deployment, carried as the
	// `app.tenant_id` GUC on every DB connection so Postgres RLS scopes the order
	// store (MT-01d). Defaults to __system__, the risk-engine convention; a
	// per-tenant deployment overrides it.
	Tenant string
	// RequireMandate refuses an order for a portfolio NO MANDATE GOVERNS, instead of
	// admitting it. Default FALSE, and that default is a deliberate, uncomfortable
	// choice: turning it on rejects every order for every portfolio nobody has run
	// kanz-mandate for yet — a trading outage dressed as a control. The posture is
	// logged loudly at startup either way, and an ungoverned order is always counted
	// and warned about (EXEC-M14).
	RequireMandate bool

	// SimVenueMIC is the simulation execution venue's MIC, or a COMMA-SEPARATED
	// LIST of them ("XNAS,XLON") — one SimVenue per MIC. A fanned-out allocation
	// stamps each leg with its target venue and the router matches on MIC, so a
	// single-venue simulator cannot work a multi-venue allocation at all. Empty ⇒ a
	// default sim venue; a deployment swaps in a real venue adapter.
	SimVenueMIC string
	// VenueEndpoints maps MIC → adapter address for OUT-OF-PROCESS venues
	// (INFRA-M7a): "XBIN=venue-binance.kanz-services.svc:9000,XOKX=venue-okx...".
	// Each becomes an execution.GRPCVenue. This is how a venue reaches the OMS
	// without a line of vendor code being linked into it.
	VenueEndpoints string
	// VenueAccounts binds each portfolio to the EXCHANGE ACCOUNT it may execute
	// against, per venue:
	//
	//	OMS_VENUE_ACCOUNTS="acme/fund-alpha@XNAS=okx-sub-1,acme/fund-beta@XLON=binance-main"
	//
	// An exchange liquidates per ACCOUNT, so an account is a collateral pool and two
	// portfolios in one pool are not segregated, whatever the ledger says. An account
	// bound to two portfolios is a startup ERROR, not a warning.
	//
	// Empty ⇒ nothing is bound: every portfolio trades whatever account its adapter
	// holds, sharing one pool per venue. The OMS says so, loudly, at startup.
	VenueAccounts string
	// RequireVenueAccount REFUSES an order whose portfolio is bound to no account at
	// its target venue (VENUE_ACCOUNT_UNBOUND), instead of executing it against a
	// shared one. Deny-by-default is the correct posture, and it defaults to FALSE for
	// the same reason OMS_REQUIRE_MANDATE does: switching it on refuses every order for
	// every portfolio nobody has bound yet, which is a trading outage, and that must be
	// a decision somebody makes with the list of accounts in hand.
	RequireVenueAccount bool
	// RequireVerifiedAccount REFUSES TO START on any venue adapter that has not PROVED
	// its exchange account against the exchange itself (SOV-02a) — i.e. one whose
	// venue.v1 Describe answers account_verified=false.
	//
	// An adapter's account is otherwise a CLAIM: it reads the account from its own
	// config, so a mis-configured adapter and a correct one are indistinguishable, and
	// the fills of the first land in the wrong fund's ledger rows while the exchange
	// debits the right one. Only the exchange can settle it, so the adapter asks it at
	// startup and reports the answer here.
	//
	// FALSE by default, for the same reason as RequireVenueAccount and RequireMandate:
	// arming it refuses every adapter nobody has bound an exchange uid to yet, which is
	// a trading outage dressed as a control. With it off the adapter still trades, the
	// unproven state is WARNed by name and counted
	// (kanz_oms_unverified_venue_account_total) — visible, rather than assumed.
	//
	// A MISMATCH is always fatal regardless of this flag: an adapter that names a
	// different account than the OMS was told is not unproven, it is wrong.
	RequireVerifiedAccount bool
	// SPIFFESocket is the workload API socket used to mTLS the venue dials
	// (SEC-01a). Empty ⇒ plaintext, which is a DEV-ONLY posture: the venue
	// connection carries live orders.
	SPIFFESocket string
	// BaseCurrency stamps Money on projected positions until a reference-data
	// currency join lands (OMS-01e).
	BaseCurrency string

	// PriceSubject is the market-data spine the OMS folds into its reference-mark
	// source, so the pre-trade gate can value MARKET/STOP orders (COMP-M2).
	PriceSubject string

	// PriceMaxAge is how old a mark may be and still value an order. It is a
	// SAFETY BOUND, not a tuning knob: widening it to quiet PRICE_UNAVAILABLE
	// refusals does not fix the feed, it just admits orders priced off a feed
	// that is no longer reporting. Zero disables expiry entirely and is
	// deliberately NOT reachable from the environment (an unparseable value is
	// an error, not a fallback to zero).
	PriceMaxAge time.Duration
}

// CommandSubjects are the order command subjects the OMS consumes.
func (Config) CommandSubjects() []string {
	return []string{order.SubjectSubmit, order.SubjectAmend, order.SubjectCancel}
}

// FillSubjects are the fill FACTs the position projector consumes (OMS-01e).
func (Config) FillSubjects() []string {
	return []string{order.EventTypePartiallyFilled, order.EventTypeFilled}
}

func Load() (Config, error) {
	cfg := Config{
		Listen:                 envOr("OMS_LISTEN", ":8090"),
		LogLevel:               parseLevel(envOr("OMS_LOG_LEVEL", "info")),
		Source:                 envOr("OMS_SOURCE", "oms"),
		OTLPEndpoint:           os.Getenv("OMS_OTLP_ENDPOINT"),
		NATSURL:                os.Getenv("OMS_NATS_URL"),
		ConsumerGroup:          envOr("OMS_CONSUMER_GROUP", "oms"),
		DatabaseURL:            secret("OMS_DATABASE_URL"),
		Tenant:                 envOr("OMS_TENANT", "__system__"),
		RequireMandate:         os.Getenv("OMS_REQUIRE_MANDATE") == "true",
		VenueAccounts:          os.Getenv("OMS_VENUE_ACCOUNTS"),
		RequireVenueAccount:    os.Getenv("OMS_REQUIRE_VENUE_ACCOUNT") == "true",
		RequireVerifiedAccount: os.Getenv("OMS_REQUIRE_VERIFIED_ACCOUNT") == "true",
		SimVenueMIC:            envOr("OMS_SIM_VENUE_MIC", "XSIM"),
		VenueEndpoints:         os.Getenv("OMS_VENUE_ENDPOINTS"),
		SPIFFESocket:           os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		BaseCurrency:           envOr("OMS_BASE_CURRENCY", "USD"),
		PriceSubject:           envOr("OMS_PRICE_SUBJECT", "market.>"),
	}

	maxAge, err := time.ParseDuration(envOr("OMS_PRICE_MAX_AGE", "30s"))
	if err != nil {
		return Config{}, fmt.Errorf("OMS_PRICE_MAX_AGE: %w", err)
	}
	cfg.PriceMaxAge = maxAge

	return cfg, nil
}

// secret resolves a sensitive value, preferring a CSI/Vault file mount
// (SEC-01d: the path in <k>_FILE) over a plaintext <k> env var. The DSN carries
// database credentials and must never ride in a pod's env block.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
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
