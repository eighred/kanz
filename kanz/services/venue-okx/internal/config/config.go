// Package config is venue-okx's runtime configuration (INFRA-M7a-3).
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/eighred/kanz/pkg/secret"
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
	databaseURL, err := secret.Read("VENUE_OKX_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	// Keys come from a CSI/Vault file mount, never from code and never from a
	// plaintext env in a manifest.
	apiKey, err := secret.Read("OKX_API_KEY")
	if err != nil {
		return Config{}, err
	}
	apiSecret, err := secret.Read("OKX_API_SECRET")
	if err != nil {
		return Config{}, err
	}
	passphrase, err := secret.Read("OKX_API_PASSPHRASE")
	if err != nil {
		return Config{}, err
	}

	// THE ENDPOINT HAS NO SAFE DEFAULT, SO IT HAS NO DEFAULT (#147).
	//
	// Binance can default to testnet.binance.vision and fail safe: that is a
	// physically separate exchange, and a key valid there is rejected by
	// api.binance.com, so a misdirected order fails rather than fills. OKX has no
	// such host. Demo trading is selected per-request by `x-simulated-trading: 1`,
	// a header this adapter does not send, so the ONLY thing standing between this
	// process and the live order book is which URL it was handed.
	//
	// These used to default to https://www.okx.com / wss://ws.okx.com:8443 — which
	// made "nobody configured this" and "somebody chose production" the same state,
	// on the one code path where that distinction is measured in real money. Deleting
	// the env var did not degrade the adapter, it promoted it to live.
	//
	// An adapter that refuses to boot is recoverable in a way one that quietly went
	// live is not, so this fails closed. Whoever runs it must SAY which exchange
	// they mean, and that statement is then visible in the manifest and in `env`.
	baseURL, err := requiredEnv("OKX_BASE_URL")
	if err != nil {
		return Config{}, err
	}
	wsBase, err := requiredEnv("OKX_WS_BASE")
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
		BaseURL:                baseURL,
		WSBase:                 wsBase,
		APIKey:                 apiKey,
		APISecret:              apiSecret,
		Passphrase:             passphrase,
		Symbols:                os.Getenv("OKX_SYMBOLS"),
	}, nil
}

// requiredEnv is envOr's counterpart for settings whose wrong value is worse
// than no value. The error names the variable, because a pod that will not start
// is only actionable if the log says which line of the manifest is missing.
func requiredEnv(k string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is required and has no default: OKX exposes no demo hostname, "+
		"so defaulting it would silently select the LIVE exchange (see #147). Set it explicitly "+
		"to the endpoint you intend to trade against", k)
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
