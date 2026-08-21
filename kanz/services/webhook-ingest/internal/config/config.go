// Package config loads webhook-ingest's runtime configuration. Infra settings
// come from the environment; the trading configuration (per-strategy secrets,
// symbol map, fund→venue allocation, and the M1 sim price/equity providers)
// comes from a bootstrap JSON file. In later milestones the price/equity
// providers bind to the live market-data and accounting projections at the
// composition root; the bootstrap file is the M1 simulation source.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
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
	SPIFFESocket string

	NATSURL string
	Source  string

	ReplayWindow time.Duration

	// MaxSignalAge bounds how old a TradingView alert may be when it is acted on
	// (#416).
	//
	// IT IS NOT THE REPLAY WINDOW ABOVE, and conflating them is the bug. The
	// replay window is NONCE DEDUP: it stops the same alert being delivered twice.
	// It says nothing about a FIRST delivery that took thirty minutes — and that
	// alert was acted on at the size the strategy chose for a price that has since
	// moved, with nothing on the path noticing.
	//
	// DEFAULTED TO A REAL BOUND, not to zero. A safety control that ships disabled
	// and waits for someone to set it is the shape of every "we had the fix but it
	// was not turned on" incident. Two minutes is generous for a webhook retry and
	// for modest clock skew, and far short of the delay that makes a stale alert
	// dangerous. Set it explicitly to "0" to disable — that is then a decision
	// someone took, not one they inherited.
	MaxSignalAge time.Duration

	// RequireSignalTS refuses an alert that carries no `ts` at all. ON by default.
	//
	// An alert with no timestamp has no age, so the bound above cannot judge it —
	// and a freshness rule a sender opts out of by omitting a field is not a rule.
	//
	// IT DEFAULTS ON BECAUSE OF WHEN THIS LANDED. Owner ruling, 2026-08-12: no
	// strategies are live yet, so `ts` becomes mandatory BEFORE onboarding rather
	// than being tightened underneath running traffic. Making it strict later
	// means choosing a day to break whichever senders never sent it; making it
	// strict now costs nothing and every strategy is written against the strict
	// contract from its first alert.
	//
	// WEBHOOK_INGEST_REQUIRE_SIGNAL_TS=false relaxes it — for onboarding a sender
	// that genuinely cannot stamp its alerts, and preferable to disabling the
	// whole bound. An unparseable value keeps the STRICT default: a typo must not
	// silently relax a trading control.
	RequireSignalTS bool
	Allowlist       []*net.IPNet

	// RedisURL backs the CROSS-POD nonce store (EXEC-M17). The nonce cache is the
	// replay defence at the internet-facing perimeter, and in-process it is per-pod:
	// two replicas are two caches, so a re-delivered TradingView alert landing on the
	// other pod is admitted a SECOND time and fans out a SECOND set of orders. Nothing
	// downstream can catch that — a fresh claim mints a fresh signal_id, so the OMS's
	// admission gate sees two different orders.
	//
	// Empty ⇒ the in-process store, which is correct for EXACTLY ONE REPLICA and is
	// therefore what pins this service — the one the internet talks to — to a single
	// pod. Set this (with -tags redis) and the pin can be lifted.
	RedisURL string
	// AllowInProcessNonce is the EXPLICIT admission that the replay defence is per-pod.
	// Without it, and without a RedisURL, the service REFUSES TO START: a per-pod replay
	// defence reachable by FORGETTING to configure Redis is indistinguishable from a
	// correct one, and the deployment that forgot is the one running N replicas.
	AllowInProcessNonce bool
	// CloudflareOnly locks the webhook to the Cloudflare Signing Relay edge: the
	// peer (Allowlist, set to Cloudflare's CIDR ranges) must be a Cloudflare IP,
	// and every request must carry CF-Connecting-IP — public traffic bypassing
	// the relay is rejected before any processing (M3.8).
	CloudflareOnly bool

	// Trading configuration from the bootstrap file.
	Secrets ingest.StaticSecrets
	Symbols ingest.StaticSymbols
	Alloc   ingest.StaticAllocation
	Prices  ingest.StaticPrices
	Equity  ingest.StaticEquity

	// Authority is the strategy→fund→tenant binding — the ONLY thing that decides
	// whose book a signal lands on (#632).
	//
	// It is built from the same two entries the secrets and the allocations come
	// from, and applyBootstrap cross-checks them: a strategy that declares no funds,
	// or names one no `funds` entry declares, or a fund that declares no tenant, is
	// a LOAD ERROR. The service exits 2 rather than start, because the failure this
	// replaces was precisely a deployment that had configured neither and looked
	// identical to one that had.
	Authority ingest.FundAuthority

	// MaxQuantity bounds the RESOLVED base-asset quantity of one alert — units of
	// the instrument, after size_type has been applied. See translate.Qty.
	MaxQuantity ingest.Qty
	// MaxLeverage is currently subordinate to a hard refusal of any leverage != 1;
	// see ingest.Options.MaxLeverage (#240).
	MaxLeverage *big.Rat
}

// bootstrap is the JSON shape of the trading config file. All decimals are
// strings (exact; no float).
type bootstrap struct {
	Strategies map[string]strategyEntry `json:"strategies"` // strategy_id -> secret + the funds it may trade
	Symbols    map[string]string        `json:"symbols"`    // tv symbol -> instrument_id
	Funds      map[string]fundEntry     `json:"funds"`      // fund_id -> owning tenant + allocation
	Prices     map[string]string        `json:"prices"`     // instrument_id -> price
	Equity     map[string]string        `json:"equity"`     // fund_id -> NAV
	// MaxQuantity is a bound on the RESOLVED base-asset quantity — "never more than
	// N units of the instrument per alert", checked after size_type is applied.
	MaxQuantity string `json:"max_quantity"`
	// LegacyMaxSize captures the RETIRED `max_size` key so its presence is an ERROR
	// rather than a silently ignored field. json.Unmarshal drops unknown keys, so
	// renaming the key without this would take a deployment that HAD a bound and
	// leave it with NONE — a worse outcome than the defect being fixed (#240).
	LegacyMaxSize string `json:"max_size"`
	MaxLeverage   string `json:"max_leverage"`
}

type venueWeight struct {
	Venue  string `json:"venue"`
	Weight string `json:"weight"`
}

// strategyEntry is one sender: the secret that authenticates it, and the funds it
// is entitled to trade.
//
// THE SECRET AND THE ENTITLEMENT ARE ONE DECLARATION, not two maps that happen to
// share a key. That shape is the defect (#632): `strategies` (id → secret) and
// `funds` (id → allocation) sat beside each other with no relation declared and
// nothing cross-checking them, so the platform authenticated a STRATEGY and then
// took the tenant from whatever `fund_id` the same request body carried. Written
// this way, adding a strategy without deciding which funds it may trade is not
// something an operator can forget — it does not parse.
type strategyEntry struct {
	Secret string   `json:"secret"`
	Funds  []string `json:"funds"`
}

// UnmarshalJSON accepts the object form and REFUSES the retired bare-string form
// loudly.
//
// json.Unmarshal would otherwise answer a pre-#632 file — `"strategies": {"dev":
// "secret"}` — with a type error naming a Go type, which tells an operator
// nothing about what changed or what to write. Worse, silently tolerating it
// would leave the deployment with a strategy bound to NO funds, which now denies
// every alert: a security fix that reads as an outage.
func (e *strategyEntry) UnmarshalJSON(raw []byte) error {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return fmt.Errorf("bootstrap config: a `strategies` entry is a bare secret string, which is "+
			"the RETIRED pre-#632 shape. An HMAC authenticates the STRATEGY, never a fund — so a "+
			"strategy that does not say which funds it may trade could name any fund in this file "+
			"and have its orders published into that fund's tenant. Write "+
			`{"<strategy_id>": {"secret": %q, "funds": ["<fund_id>", ...]}}`+" and decide, per "+
			"strategy, whose capital it is allowed to commit", asString)
	}
	type plain strategyEntry // no recursion through this method
	var p plain
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	*e = strategyEntry(p)
	return nil
}

// fundEntry is one fund: the TENANT THAT OWNS IT and its venue allocation.
//
// The tenant is here — beside the allocation, in configuration — because it is
// the routing key onto that tenant's own broker account and therefore the one
// value a request must never be able to choose. Before #632 there was no such
// key: `translate` defaulted to "the fund_id is the tenant" and no composition
// root overrode it, so the wire subject of every signal-originated order was a
// string out of the request body.
type fundEntry struct {
	Tenant string        `json:"tenant"`
	Venues []venueWeight `json:"venues"`
}

// UnmarshalJSON accepts the object form and REFUSES the retired bare-array form
// loudly — same reasoning as strategyEntry's, and the same failure if it were
// tolerated: a fund with no tenant cannot be published for at all.
func (f *fundEntry) UnmarshalJSON(raw []byte) error {
	var asLegs []venueWeight
	if err := json.Unmarshal(raw, &asLegs); err == nil {
		return fmt.Errorf("bootstrap config: a `funds` entry is a bare venue array, which is the " +
			"RETIRED pre-#632 shape. A fund must name the TENANT that owns it — that tenant is the " +
			"broker subject a signal-originated order is routed on, and taking it from the request " +
			"body (which is what happened when it was absent) let one strategy secret place orders " +
			"in any tenant. Write " +
			`{"<fund_id>": {"tenant": "<tenant_id>", "venues": [{"venue": "...", "weight": "..."}]}}`)
	}
	type plain fundEntry
	var p plain
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	*f = fundEntry(p)
	return nil
}

// Load reads env + the bootstrap file (WEBHOOK_INGEST_CONFIG) and validates it.
func Load() (Config, error) {
	// Resolved before the literal so a DECLARED-but-unreadable secret mount stops
	// Load here. The local helper this replaces answered a failed mount with the
	// plaintext env var and then with "", and "" here selects the IN-PROCESS nonce
	// store — see RedisURL above. A deployment that mounts a Redis URL and also
	// carries AllowInProcessNonce (a leftover from its single-replica days) would
	// therefore survive a broken Vault mount by silently falling back to a per-pod
	// replay defence, on N replicas, at the internet-facing perimeter — the exact
	// state that admits a re-delivered alert twice and fans out a second set of
	// orders. See pkg/secret.
	redisURL, err := secret.Read("WEBHOOK_INGEST_REDIS_URL")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen:          envOr("WEBHOOK_INGEST_LISTEN", ":8090"),
		LogLevel:        parseLevel(os.Getenv("WEBHOOK_INGEST_LOG_LEVEL")),
		OTLPEndpoint:    os.Getenv("WEBHOOK_INGEST_OTLP_ENDPOINT"),
		SPIFFESocket:    os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:         envOr("WEBHOOK_INGEST_NATS_URL", "nats://localhost:4222"),
		Source:          envOr("WEBHOOK_INGEST_SOURCE", "webhook-ingest"),
		ReplayWindow:    parseDuration(os.Getenv("WEBHOOK_INGEST_REPLAY_WINDOW"), 5*time.Minute),
		MaxSignalAge:    parseSignalAge(os.Getenv("WEBHOOK_INGEST_MAX_SIGNAL_AGE"), 2*time.Minute),
		RequireSignalTS: os.Getenv("WEBHOOK_INGEST_REQUIRE_SIGNAL_TS") != "false",
		CloudflareOnly:  os.Getenv("WEBHOOK_INGEST_CLOUDFLARE_ONLY") == "1",

		RedisURL:            redisURL,
		AllowInProcessNonce: os.Getenv("WEBHOOK_INGEST_ALLOW_INPROCESS_NONCE") == "true",
	}
	allow, err := parseAllowlist(os.Getenv("WEBHOOK_INGEST_IP_ALLOWLIST"))
	if err != nil {
		return Config{}, err
	}
	cfg.Allowlist = allow

	path := os.Getenv("WEBHOOK_INGEST_CONFIG")
	if path == "" {
		return Config{}, errors.New("WEBHOOK_INGEST_CONFIG is required (path to the bootstrap trading config)")
	}
	b, err := loadBootstrap(path)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.applyBootstrap(b); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadBootstrap(path string) (*bootstrap, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap config: %w", err)
	}
	var b bootstrap
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse bootstrap config: %w", err)
	}
	return &b, nil
}

func (cfg *Config) applyBootstrap(b *bootstrap) error {
	cfg.Secrets = make(ingest.StaticSecrets, len(b.Strategies))
	strategyFunds := make(map[string][]string, len(b.Strategies))
	for id, s := range b.Strategies {
		if strings.TrimSpace(s.Secret) == "" {
			return fmt.Errorf("bootstrap config: strategy %q has no secret — an empty HMAC key "+
				"authenticates a signature computed with an empty key, which any sender can produce", id)
		}
		cfg.Secrets[id] = s.Secret
		strategyFunds[id] = s.Funds
	}
	cfg.Symbols = ingest.StaticSymbols(b.Symbols)

	cfg.Prices = make(ingest.StaticPrices, len(b.Prices))
	for inst, s := range b.Prices {
		r, err := dec.ParseRat(s)
		if err != nil {
			return fmt.Errorf("price for %s: %w", inst, err)
		}
		cfg.Prices[inst] = r
	}
	cfg.Equity = make(ingest.StaticEquity, len(b.Equity))
	for fund, s := range b.Equity {
		r, err := dec.ParseRat(s)
		if err != nil {
			return fmt.Errorf("equity for %s: %w", fund, err)
		}
		cfg.Equity[fund] = r
	}
	cfg.Alloc = make(ingest.StaticAllocation, len(b.Funds))
	fundTenant := make(map[string]string, len(b.Funds))
	for fund, entry := range b.Funds {
		fundTenant[fund] = entry.Tenant
		out := make([]ingest.VenueAllocation, 0, len(entry.Venues))
		for _, l := range entry.Venues {
			w, err := dec.ParseRat(l.Weight)
			if err != nil {
				return fmt.Errorf("weight for fund %s venue %s: %w", fund, l.Venue, err)
			}
			out = append(out, ingest.VenueAllocation{Venue: l.Venue, Weight: w})
		}
		// REFUSE TO START on a split that is not a split. Weights were parsed here and
		// never summed, so a fund written 60/40 instead of 0.6/0.4 loaded clean and
		// multiplied every order it ever placed by 100 (#240).
		if err := ingest.ValidateAllocation(fund, out); err != nil {
			return err
		}
		cfg.Alloc[fund] = out
	}
	// The retired key. `max_size` bounded the RAW size before size_type was applied,
	// which made it three different units at once; the replacement bounds the
	// resolved base-asset quantity. Reinterpreting an operator's old number silently
	// would change what their cap means without telling them — a `max_size` meant as
	// a $1,000,000 notional cap becomes 1,000,000 UNITS, effectively no cap at all.
	// So it is an error, and the operator re-expresses it.
	if b.LegacyMaxSize != "" {
		return fmt.Errorf("bootstrap config: `max_size` is retired and MUST be re-expressed as "+
			"`max_quantity` (#240). It bounded the raw `size` BEFORE size_type was applied, so it "+
			"was a quantity, a notional and a percentage at once — a cap of 10 meaning 10 units "+
			"admitted {\"size\":\"9\",\"size_type\":\"pct_of_equity\"} and fanned out 1800. "+
			"`max_quantity` bounds the RESOLVED quantity, in units of the instrument. The old "+
			"value %q is NOT carried over: decide what it should mean in units and set it",
			b.LegacyMaxSize)
	}
	if b.MaxQuantity != "" {
		r, err := dec.ParseRat(b.MaxQuantity)
		if err != nil {
			return fmt.Errorf("max_quantity: %w", err)
		}
		if r.Sign() <= 0 {
			return fmt.Errorf("max_quantity: %s is not positive — a non-positive cap refuses every "+
				"order; omit the key to run unbounded", r.RatString())
		}
		cfg.MaxQuantity = ingest.NewQty(r)
	}
	if b.MaxLeverage != "" {
		r, err := dec.ParseRat(b.MaxLeverage)
		if err != nil {
			return fmt.Errorf("max_leverage: %w", err)
		}
		cfg.MaxLeverage = r
	}
	// REFUSE TO START WITHOUT THE BINDING (#632).
	//
	// Every check that could have stopped the cross-tenant injection lives in
	// NewFundAuthority, and it runs HERE — at config load, before the listener
	// exists — so an unconfigured or incoherent deployment exits 2 instead of
	// serving the internet with a tenant nobody chose. There is no per-signal
	// fallback for it to fall back to; that fallback WAS the defect.
	//
	// LAST, so the two maps it cross-checks have both been read and every
	// field-level error above (a bad weight, the retired max_size key) still reports
	// itself rather than being pre-empted by a binding complaint about the same file.
	authority, err := ingest.NewFundAuthority(fundTenant, strategyFunds)
	if err != nil {
		return err
	}
	cfg.Authority = authority
	return nil
}

func parseAllowlist(s string) ([]*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			part += "/32" // bare IP ⇒ host CIDR
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid allowlist CIDR %q: %w", part, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// parseSignalAge reads the freshness bound, and it does NOT use parseDuration
// below — because that function maps every non-positive value to its default.
//
// That rule is right for the replay window (a zero-length nonce cache is
// nonsense, and a typo must not silently disable dedup) and wrong here, where
// zero is a MEANINGFUL value: it turns the bound off. Sharing the parser made
// this setting undisableable while its own documentation said otherwise — a
// comment that lied about the code beside it, which is the shape this repository
// treats as a defect in its own right.
//
// An UNPARSEABLE value still falls back to the default, deliberately: a typo
// must not disable a safety bound. Only an explicit, valid zero does.
func parseSignalAge(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return def
	}
	return d // 0 ⇒ disabled, and that is an explicit act
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
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
