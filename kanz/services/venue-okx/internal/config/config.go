// Package config is venue-okx's runtime configuration (INFRA-M7a-3).
package config

import (
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strings"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
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

	// TradingMode says which OKX book this adapter reaches (#147). It is a
	// SEPARATE field from BaseURL and cannot be derived from it: OKX serves demo
	// and production from the same www.okx.com and switches on the
	// `x-simulated-trading: 1` request header. Set from OKX_TRADING_MODE, which
	// is required and has no default.
	TradingMode exchangeauth.OKXTradingMode
	APIKey      string
	APISecret   string
	// Passphrase is OKX's THIRD credential. Binance signs with key+secret; OKX
	// additionally requires the passphrase chosen when the API key was created,
	// sent as an OK-ACCESS-PASSPHRASE header. Without it every signed request is
	// rejected — it is as load-bearing as the secret, and mounted the same way.
	Passphrase string
	// Symbols maps Kanz instrument_id → OKX instrument ("BTC-USD=BTC-USDT").
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
	// such host. Demo trading is selected per-request by `x-simulated-trading: 1`.
	//
	// This comment used to end "a header this adapter does not send, so the ONLY
	// thing standing between this process and the live order book is which URL it
	// was handed." That is no longer true: the header IS sent now, driven by
	// OKX_TRADING_MODE below. Left unedited it would have been a comment asserting
	// the absence of the very control the next block configures — the dated-evidence
	// trap AGENTS.md names, and one this file has already sprung once (an arch guard
	// read this paragraph's mention of the header as proof the header existed).
	//
	// The endpoint still has no default, for its own reason: it decides which
	// exchange is reached at all.
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

	// THE ENDPOINT IS NOT THE MODE, AND THIS IS THE HALF THE COMMENT ABOVE COULD
	// ONLY DESCRIBE (#147).
	//
	// Removing the OKX_BASE_URL default stopped "nobody configured this" from
	// meaning live. It could not make demo REACHABLE, because no URL reaches
	// OKX's demo book — the adapter sent no `x-simulated-trading: 1` header, so
	// every authenticated order went to production regardless of the host.
	//
	// OKX_TRADING_MODE is that missing axis, stated rather than inferred, and
	// required for the same reason the endpoint is: a default here is a default
	// about whether orders are real. Note that `demo` with BaseURL
	// https://www.okx.com is CORRECT and expected — that pairing is what demo
	// trading looks like at OKX, which is exactly why the URL cannot be read as
	// evidence of safety by this config, by a reviewer, or by an arch guard.
	tradingMode, err := requiredOKXTradingMode()
	if err != nil {
		return Config{}, err
	}

	// PARSED, NOT STRING-COMPARED (#783). It used to read
	// os.Getenv(key) == "true", which makes "True", "TRUE", "1" and a value a
	// mounted file left whitespace on all read as FALSE — a quote-match control an
	// operator armed and that silently was not. env.Bool refuses a value it cannot
	// parse, so a typo is a startup error naming the key rather than a venue
	// adapter accepting fills at a price nobody checked.
	requireQuoteMatch, err := env.Bool("OKX_REQUIRE_QUOTE_MATCH", false)
	if err != nil {
		return Config{}, err
	}

	return Config{
		GRPCListen:     env.Or("VENUE_OKX_GRPC_LISTEN", ":9000"),
		HTTPListen:     env.Or("VENUE_OKX_LISTEN", ":8092"),
		LogLevel:       env.ParseLevelOr(os.Getenv("VENUE_OKX_LOG_LEVEL"), slog.LevelInfo),
		Source:         env.Or("VENUE_OKX_SOURCE", "venue-okx"),
		OTLPEndpoint:   os.Getenv("VENUE_OKX_OTLP_ENDPOINT"),
		NATSURL:        os.Getenv("VENUE_OKX_NATS_URL"),
		DatabaseURL:    databaseURL,
		Tenant:         env.Or("VENUE_OKX_TENANT", "__system__"),
		SPIFFESocket:   os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		AllowedClients: os.Getenv("VENUE_OKX_ALLOWED_CLIENTS"),

		MIC:                    env.Or("OKX_MIC", "OKX"),
		Account:                env.Or("OKX_VENUE_ACCOUNT", env.Or("OKX_MIC", "OKX")),
		AccountUID:             os.Getenv("OKX_VENUE_ACCOUNT_UID"),
		AllowUnverifiedAccount: os.Getenv("OKX_ALLOW_UNVERIFIED_ACCOUNT") == "true",
		BaseURL:                baseURL,
		WSBase:                 wsBase,
		TradingMode:            tradingMode,
		APIKey:                 apiKey,
		APISecret:              apiSecret,
		Passphrase:             passphrase,
		Symbols:                os.Getenv("OKX_SYMBOLS"),
		RequireQuoteMatch:      requireQuoteMatch,
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

// requiredOKXTradingMode reads OKX_TRADING_MODE and accepts only the two values
// that mean something. An unset or unrecognised value is an error, never a
// fallback (#147).
//
// The rejected-value message lists both options rather than saying "invalid",
// because the person hitting this at 3am during a cutover needs to know that
// `demo` exists at all — the adapter could not reach demo for its whole life
// before this, so nobody's muscle memory includes it.
func requiredOKXTradingMode() (exchangeauth.OKXTradingMode, error) {
	raw := strings.TrimSpace(os.Getenv("OKX_TRADING_MODE"))
	switch exchangeauth.OKXTradingMode(raw) {
	case exchangeauth.OKXLive:
		return exchangeauth.OKXLive, nil
	case exchangeauth.OKXDemo:
		return exchangeauth.OKXDemo, nil
	case "":
		return "", fmt.Errorf("OKX_TRADING_MODE is required and has no default: OKX serves demo and "+
			"production from the SAME host and distinguishes them by the `x-simulated-trading: 1` "+
			"request header, so the endpoint cannot say which book you meant (see #147). Set it to "+
			"%q or %q", string(exchangeauth.OKXDemo), string(exchangeauth.OKXLive))
	default:
		return "", fmt.Errorf("OKX_TRADING_MODE=%q is not a mode: set it to %q (orders reach OKX's "+
			"demo book, requires a demo API key) or %q (orders settle in real money)",
			raw, string(exchangeauth.OKXDemo), string(exchangeauth.OKXLive))
	}
}
