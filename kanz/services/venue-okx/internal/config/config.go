// Package config is venue-okx's runtime configuration (INFRA-M7a-3).
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Config is the venue-okx runtime configuration, sourced from the
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

	// --- the exchange ---

	MIC string
	// Account is the EXCHANGE ACCOUNT the API credential above belongs to — the
	// sub-account whose collateral every fill this adapter produces settles against.
	// An exchange margins and LIQUIDATES per account, so this is the boundary that
	// segregates one fund's capital from another's; the OMS binds portfolios to it.
	// Empty ⇒ the MIC is used, i.e. "this venue is one account", which is a claim, not
	// an absence — the adapter says so at startup.
	Account string
	// AccountUID is OKX'S OWN id (the uid) for the account named above — what the label
	// is bound TO, and the only thing that can be checked (SOV-02a). At startup the
	// adapter asks OKX which account its API key belongs to and REFUSES TO SERVE ORDERS
	// if the answer is not this. Without it, Account is a claim nobody has ever tested:
	// a mis-configured adapter and a correct one are the same deployment, and the first
	// books its fills to the wrong fund's ledger while the exchange debits the right one.
	//
	// Empty ⇒ nothing can be proved ⇒ the adapter refuses to start unless
	// AllowUnverifiedAccount says otherwise.
	AccountUID string
	// AllowUnverifiedAccount permits booting with an account NOBODY has confirmed — the
	// explicit, affirmative choice the brain's no-simulator-reachable-by-omission rule
	// demands, since absence of configuration must never quietly become trust. It does
	// NOT permit booting a WRONG account: if OKX says the key belongs elsewhere, the
	// adapter refuses whatever this says.
	AllowUnverifiedAccount bool
	BaseURL                string
	WSBase                 string
	APIKey                 string
	APISecret              string
	// Passphrase is OKX's THIRD credential. Binance signs with key+secret; OKX
	// additionally requires the passphrase chosen when the API key was created,
	// sent as an OK-ACCESS-PASSPHRASE header. Without it every signed request is
	// rejected — it is as load-bearing as the secret, and mounted the same way.
	Passphrase string
	// Symbols maps Kanz instrument_id → OKX instrument ("BTC-USD=BTC-USDT").
	Symbols string
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	databaseURL, err := secret("VENUE_OKX_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	// Keys come from a CSI/Vault file mount, never from code and never from a
	// plaintext env in a manifest.
	apiKey, err := secret("OKX_API_KEY")
	if err != nil {
		return Config{}, err
	}
	apiSecret, err := secret("OKX_API_SECRET")
	if err != nil {
		return Config{}, err
	}
	passphrase, err := secret("OKX_API_PASSPHRASE")
	if err != nil {
		return Config{}, err
	}

	return Config{
		GRPCListen:   envOr("VENUE_OKX_GRPC_LISTEN", ":9000"),
		HTTPListen:   envOr("VENUE_OKX_LISTEN", ":8092"),
		LogLevel:     parseLevel(os.Getenv("VENUE_OKX_LOG_LEVEL")),
		Source:       envOr("VENUE_OKX_SOURCE", "venue-okx"),
		OTLPEndpoint: os.Getenv("VENUE_OKX_OTLP_ENDPOINT"),
		NATSURL:      os.Getenv("VENUE_OKX_NATS_URL"),
		DatabaseURL:  databaseURL,
		Tenant:       envOr("VENUE_OKX_TENANT", "__system__"),
		SPIFFESocket: os.Getenv("SPIFFE_ENDPOINT_SOCKET"),

		MIC:                    envOr("OKX_MIC", "OKX"),
		Account:                envOr("OKX_VENUE_ACCOUNT", envOr("OKX_MIC", "OKX")),
		AccountUID:             os.Getenv("OKX_VENUE_ACCOUNT_UID"),
		AllowUnverifiedAccount: os.Getenv("OKX_ALLOW_UNVERIFIED_ACCOUNT") == "true",
		BaseURL:                envOr("OKX_BASE_URL", "https://www.okx.com"),
		WSBase:                 envOr("OKX_WS_BASE", "wss://ws.okx.com:8443"),
		APIKey:                 apiKey,
		APISecret:              apiSecret,
		Passphrase:             passphrase,
		Symbols:                os.Getenv("OKX_SYMBOLS"),
	}, nil
}

// secret prefers a CSI/Vault file mount (<k>_FILE) over a plaintext <k> env var
// (SEC-01d). Setting <k>_FILE is the deployment's declaration that a durable
// secret mount was intended (see e.g. venue-okx-deploy.yaml's
// VENUE_OKX_DATABASE_URL_FILE): if that path is set but unreadable — file
// missing, wrong permissions, a bad mount — that is a deployment fault, not an
// absent secret, and must fail loudly rather than fall through to the plaintext
// env and then to "". A mounted-but-unreadable secret is otherwise
// indistinguishable from one that was never configured at all. A missing file
// and a permission error are both deployment faults; neither is special-cased.
func secret(k string) (string, error) {
	p := os.Getenv(k + "_FILE")
	if p == "" {
		return os.Getenv(k), nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("%s_FILE=%q: declared secret mount is unreadable: %w", k, p, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
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
