// Package config is venue-binance's runtime configuration (INFRA-M7a-2).
package config

import (
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"

	"github.com/eighred/kanz/pkg/secret"
)

// Config is the venue-binance runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	// GRPCListen is the venue.v1 surface the OMS dials.
	GRPCListen string
	// HTTPListen serves /healthz + /readyz + metrics.
	HTTPListen string
	LogLevel   slog.Level
	Source     string
	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string

	// NATSURL is the spine. This adapter PUBLISHES on it — the ASYNC half of the
	// venue contract. venue.v1 gRPC carries the synchronous submit/cancel; fills
	// arriving later on the user-data websocket, and the StateHealed FACTs the
	// reconciler emits, go to the bus, where the OMS's projector already consumes
	// them. Empty ⇒ the adapter cannot report async fills, so it refuses to start
	// its workers rather than trade blind.
	NATSURL string

	// DatabaseURL backs the adapter's own order view. Empty ⇒ in-memory, which is
	// correct for tests and loses on restart exactly the state the healing seam
	// needs (see internal/orderview).
	DatabaseURL string
	// Tenant is carried as the app.tenant_id GUC on every DB connection (MT-01d)
	// and stamped on the FACTs this adapter publishes.
	Tenant string

	// SPIFFESocket is the workload API socket. The gRPC surface is mTLS when set —
	// this endpoint SUBMITS ORDERS TO A LIVE EXCHANGE, so an unauthenticated peer
	// on it can trade.
	SPIFFESocket string

	// AllowedClients is the comma-separated SPIFFE ID allow-list for the venue.v1
	// listener. REQUIRED whenever SPIFFESocket is set.
	//
	// AUTHENTICATION IS NOT AUTHORIZATION. This listener used to authorize with
	// transport.AuthorizeMesh(), which admits any peer holding a valid SVID in
	// kanz.internal — that is EVERY workload in the trust domain, because issuing
	// them an SVID is what the trust domain does. So the property "only the OMS
	// may submit orders to a live exchange" rested entirely on a NetworkPolicy,
	// one layer down and in a different repository directory, with nothing in the
	// process itself refusing anyone.
	//
	// The narrower helper already existed and was already used twice — the
	// operator's control plane and the api-gateway's listener both allow-list
	// their callers. This adapter is the higher-value target of the two: the
	// operator writes credentials, this one spends money with them.
	AllowedClients string

	// --- the exchange ---

	MIC string
	// Account is the EXCHANGE ACCOUNT the API credential above belongs to — the
	// sub-account whose collateral every fill this adapter produces settles against.
	// An exchange margins and LIQUIDATES per account, so this is the boundary that
	// segregates one fund's capital from another's; the OMS binds portfolios to it.
	// Empty ⇒ the MIC is used, i.e. "this venue is one account", which is a claim, not
	// an absence — the adapter says so at startup.
	Account string
	// AccountUID is BINANCE'S OWN id (the uid) for the account named above — what the
	// label is bound TO, and the only thing that can be checked (SOV-02a). At startup
	// the adapter asks Binance which account its API key belongs to and REFUSES TO
	// SERVE ORDERS if the answer is not this. Without it, Account is a claim nobody has
	// ever tested: a mis-configured adapter and a correct one are the same deployment,
	// and the first books its fills to the wrong fund's ledger while the exchange
	// debits the right one.
	//
	// Empty ⇒ nothing can be proved ⇒ the adapter refuses to start unless
	// AllowUnverifiedAccount says otherwise.
	AccountUID string
	// AllowUnverifiedAccount permits booting with an account NOBODY has confirmed. It
	// is the explicit, affirmative choice the brain's no-simulator-reachable-by-
	// omission rule demands — absence of configuration must never quietly become
	// trust. It does NOT permit booting a WRONG account: if Binance says the key
	// belongs elsewhere, the adapter refuses whatever this says.
	AllowUnverifiedAccount bool
	BaseURL                string
	WSBase                 string
	APIKey                 string
	APISecret              string
	// Symbols maps Kanz instrument_id → Binance symbol ("BTC-USD=BTCUSDT").
	Symbols string

	// RequireQuoteMatch REFUSES TO START when a symbol map entry's canonical id
	// disagrees with the exchange symbol about the quote asset (#407) — "BTC-USD"
	// mapped to BTCUSDT, which trades a stablecoin and records dollars.
	//
	// DEFAULT FALSE, the same stance as every other REQUIRE_ on this platform: a
	// control that refuses to start every adapter nobody has corrected yet is a
	// trading outage, and it is armed WITH the estate in hand, never as a default.
	// Until then the mismatch is NAMED at startup and counted, so "nobody checked"
	// and "checked, and fine" do not look the same.
	RequireQuoteMatch bool
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	databaseURL, err := secret.Read("VENUE_BINANCE_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	// Keys come from a CSI/Vault file mount, never from code and never from a
	// plaintext env in a manifest.
	apiKey, err := secret.Read("BINANCE_API_KEY")
	if err != nil {
		return Config{}, err
	}
	apiSecret, err := secret.Read("BINANCE_API_SECRET")
	if err != nil {
		return Config{}, err
	}
	accountUID, err := secret.Read("BINANCE_VENUE_ACCOUNT_UID")
	if err != nil {
		return Config{}, err
	}

	// PARSED, NOT STRING-COMPARED (#783). It used to read
	// os.Getenv(key) == "true", which makes "True", "TRUE", "1" and a value a
	// mounted file left whitespace on all read as FALSE — a quote-match control an
	// operator armed and that silently was not. env.Bool refuses a value it cannot
	// parse, so a typo is a startup error naming the key rather than a venue
	// adapter accepting fills at a price nobody checked.
	requireQuoteMatch, err := env.Bool("BINANCE_REQUIRE_QUOTE_MATCH", false)
	if err != nil {
		return Config{}, err
	}

	return Config{
		GRPCListen:     env.Or("VENUE_BINANCE_GRPC_LISTEN", ":9000"),
		HTTPListen:     env.Or("VENUE_BINANCE_LISTEN", ":8091"),
		LogLevel:       env.ParseLevelOr(os.Getenv("VENUE_BINANCE_LOG_LEVEL"), slog.LevelInfo),
		Source:         env.Or("VENUE_BINANCE_SOURCE", "venue-binance"),
		OTLPEndpoint:   os.Getenv("VENUE_BINANCE_OTLP_ENDPOINT"),
		NATSURL:        os.Getenv("VENUE_BINANCE_NATS_URL"),
		DatabaseURL:    databaseURL,
		Tenant:         env.Or("VENUE_BINANCE_TENANT", "__system__"),
		SPIFFESocket:   os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		AllowedClients: os.Getenv("VENUE_BINANCE_ALLOWED_CLIENTS"),

		MIC:                    env.Or("BINANCE_MIC", "BINANCE"),
		Account:                env.Or("BINANCE_VENUE_ACCOUNT", env.Or("BINANCE_MIC", "BINANCE")),
		AccountUID:             accountUID,
		AllowUnverifiedAccount: os.Getenv("BINANCE_ALLOW_UNVERIFIED_ACCOUNT") == "true",
		BaseURL:                env.Or("BINANCE_BASE_URL", "https://testnet.binance.vision"),
		WSBase:                 env.Or("BINANCE_WS_BASE", "wss://testnet.binance.vision"),
		APIKey:                 apiKey,
		APISecret:              apiSecret,
		Symbols:                os.Getenv("BINANCE_SYMBOLS"),
		RequireQuoteMatch:      requireQuoteMatch,
	}, nil
}
