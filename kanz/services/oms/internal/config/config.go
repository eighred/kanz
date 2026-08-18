package config

import (
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/oms/internal/order"
)

// Config is the oms runtime configuration, sourced from the environment so it
// composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	Listen string
	// GRPCListen is where the order-history read surface serves (#399). Empty
	// disables it, which is the shape a deployment that fronts no web app runs
	// in — an unserved port is better than a read surface nobody asked for.
	GRPCListen string
	LogLevel   slog.Level
	Source     string
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
	// DefaultVenueMIC is the venue an order carrying NO target is worked at
	// (#437). Empty is fine with a single venue — that choice is unambiguous —
	// and with several it makes an untargeted order a loud refusal naming the
	// candidates, rather than a silent pick of whichever the config listed first.
	//
	// Almost nothing reaches it today: both fan-out producers stamp a venue on
	// every leg. That is exactly why it was safe to be wrong for so long, and why
	// the day something does arrive untargeted is the wrong day to discover the
	// destination was chosen by slice order.
	DefaultVenueMIC string
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
	//
	// A BASKET CHANGE IS A DEPLOYMENT, not an API call (#68, owner ruling
	// 2026-07-27). Edit this value and redeploy; the whole set is re-read and
	// re-checked at startup. There is no runtime endpoint that rebinds a
	// portfolio, and that absence is the feature — see internal/execution's
	// account.go for why, and test/arch/basket_contract_test.go for the guard
	// that keeps it true.
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

	// RequireOrderTypeSupport REFUSES TO START on any venue adapter that does not
	// DECLARE which order types it can place (#405).
	//
	// The OMS refuses an undeclarable order type at admission, but only for an
	// adapter that answered. An adapter predating venue.v1's supported_order_types
	// answers with an empty list, and empty means "did not say" — so the gate is
	// open for it, and a stop routed there is admitted, announced, and refused only
	// at the exchange. That is exactly the state this was built to end.
	//
	// DEFAULT FALSE, and the reason is the same one OMS_REQUIRE_VERIFIED_ACCOUNT
	// carries: a control that refuses every order for every adapter nobody has
	// upgraded yet is a trading outage, and it is armed WITH the fleet in hand,
	// never as a default. Until then the OMS names each silent adapter at startup
	// and counts it, so "nothing configured" and "checked, and fine" do not look
	// the same.
	RequireOrderTypeSupport bool
	// RequireDualControl arms maker-checker on ORDER SUBMISSION (#410): an order at
	// or above DualControlMinNotional takes two different authenticated people.
	//
	// IT CANNOT BE SET TODAY, AND THAT IS A REFUSAL RATHER THAN AN OMISSION. There
	// is nowhere to put an order awaiting approval: the ruling is a PROPOSALS TABLE
	// in this service, on the shape of services/datamaster/internal/store/proposals
	// .go, and it is not built yet — so arming the refusal would
	// REJECT every order at or above the threshold rather than hold it. Load says
	// so and refuses to start, which is one line for the placement PR to delete.
	//
	// The threshold ALONE is the shipped posture, and it is not a no-op: every
	// order is still admitted on one signature, each is classified against the
	// threshold, and kanz_oms_order_signatures_total makes "how much of the flow
	// was large and single-signed" readable before anybody arms anything. Same
	// stance as OMS_REQUIRE_MANDATE, OMS_REQUIRE_VENUE_ACCOUNT,
	// OMS_REQUIRE_VERIFIED_ACCOUNT and DATAMASTER_REQUIRE_DUAL_CONTROL: build the
	// control, count the gap, arm it with the list in hand.
	RequireDualControl bool

	// DualControlMinNotional is the order notional (quantity x price) at or above
	// which dual control applies. nil means the control is ABSENT — no threshold
	// is configured, nothing is compared, and the metric says so under
	// posture="absent" rather than letting it read as "checked, and fine".
	//
	// THERE IS NO DEFAULT, and the absence of one is the whole ruling. A default
	// would silently pick a number nobody chose, on a control whose entire purpose
	// is that a human decided the number — so OMS_REQUIRE_DUAL_CONTROL set without
	// this is a refusal to start naming both variables, never a fallback.
	//
	// IT IS SET AS AN AMOUNT AND A CURRENCY TOGETHER — OMS_DUAL_CONTROL_MIN_NOTIONAL
	// = "1000000 USD" — and a bare number is REFUSED. This is the estate's first
	// money-denominated configuration value (every other numeric OMS_* var is a
	// count or a duration) and it lands on a platform with no FX on the admission
	// path: common.v1.Decimal carries no currency, and the compliance engine never
	// reads GetCurrencyCode() at all, so it neither converts nor refuses. A bare
	// "1000000" would therefore compare a JPY order against a limit somebody meant
	// in USD and silently pass it — off by a factor of 150 in the permissive
	// direction, on the control that exists to catch large orders. Naming the
	// currency in the same string makes the two impossible to change independently,
	// and Load refuses any currency other than OMS_BASE_CURRENCY, which is the only
	// currency an order's notional is expressed in on this path today.
	//
	// It is valued the same way the pre-trade compliance gate values an order
	// (compliance.OrderPrice): a LIMIT at its own limit, a MARKET at the reference
	// mark. An order that cannot be valued is counted posture="unvaluable" and,
	// once armed, requires dual control — never quietly treated as small.
	DualControlMinNotional *big.Rat
	// DualControlNotionalCurrency is the currency the threshold above is expressed
	// in. Empty exactly when the threshold is nil; otherwise equal to BaseCurrency,
	// which Load enforces. It is carried rather than discarded so the startup log
	// states the threshold WITH its unit — a number in a log with no currency
	// beside it is the same defect one layer out.
	DualControlNotionalCurrency string

	// SPIFFESocket is the workload API socket used to mTLS the venue dials
	// (SEC-01a). Empty ⇒ plaintext, which is a DEV-ONLY posture: the venue
	// connection carries live orders.
	SPIFFESocket string
	// BaseCurrency stamps Money on projected positions until a reference-data
	// currency join lands (OMS-01e).
	BaseCurrency string

	// PriceSubjects are the market-data subjects the OMS folds into its
	// reference-mark source, so the pre-trade gate can value MARKET/STOP
	// orders (COMP-M2).
	//
	// The default is the mark fold's CONSUMPTION SET expressed as subjects, not
	// a convenient wildcard. market.v1.MarketDataEvent publishes on
	// market.<assetClass>.<variant> where the variant token comes from the
	// payload oneof (bussink.go: Trade→trade, Quote→quote, Bar→bar), and the
	// fold uses Trade and Quote only. NATS `*` matches exactly one token, so
	// these two subjects cover every asset class — present and future — while
	// structurally excluding market.*.bar and, critically, market.book.snapshot.
	//
	// That last one is why this is not `market.>`, and the reason is worse than
	// wasted CPU. Book snapshots carry an OrderBookSnapshot, a different message
	// that is WIRE-COMPATIBLE with MarketDataEvent by construction: fields 1-4
	// match, field 6 is `Trade` against `repeated PriceLevel bids`, and
	// Trade.price and PriceLevel.price are both field 1 common.v1.Decimal. A
	// one-sided, bids-only snapshot therefore decodes cleanly into a "Trade" at
	// the DEEPEST resting bid and POISONS the mark below mid. (A two-sided one is
	// rejected only by luck — its asks land in the `quote` oneof arm, win over
	// trade, and fail the both-sides check.)
	//
	// An earlier version of this comment said such a snapshot "finds empty, and
	// discards" — that was written before the behaviour was measured, and it is
	// false. mark.Handle now refuses non-mark-bearing event types outright, so
	// this subject list is the second of two layers, not the only one.
	PriceSubjects []string

	// PriceMaxAge is how old a mark may be and still value an order. It is a
	// SAFETY BOUND, not a tuning knob: widening it to quiet PRICE_UNAVAILABLE
	// refusals does not fix the feed, it just admits orders priced off a feed
	// that is no longer reporting.
	//
	// mark.Source treats any maxAge <= 0 as "never expires", so EVERY route to a
	// non-positive value is refused here: unparseable is an error rather than a
	// fallback to zero, and zero and negative durations are rejected outright.
	// They parse cleanly, which is what makes them dangerous — `-5s` is exactly
	// what someone reaching for an off switch would set, and accepting it would
	// disable the bound in silence. There is no off switch.
	PriceMaxAge time.Duration

	// SweepInterval is how often the OMS re-runs its in-flight reconciliation
	// WHILE RUNNING, on top of the mandatory one at startup (#238).
	//
	// It was the recovery latency for an order the store committed and the bus
	// never heard about: admission WAS store.Create followed by EmitAccepted with
	// no outbox between them, and when the publish failed the row was durable
	// while risk, compliance and the audit log carried nothing for it. Before
	// this, the only compensator ran at startup, so that window closed "whenever
	// this pod next restarts" — days on a healthy deployment. This bounds it at
	// SweepMinAge + SweepInterval.
	//
	// #292 CLOSED THE ADMISSION GAP ITSELF: the ACCEPTED FACT is committed in the
	// same transaction as the order and published by the outbox relay, whose own
	// latency is OutboxInterval below. What this sweep still recovers is an order
	// left mid-flight AT A VENUE, and the pre-outbox rows that predate migration
	// 0006 — see order.SweepOlderThan. Do not read the shorter remaining job as a
	// reason to disable it; see the warning cmd/oms/main.go logs when it is.
	//
	// It is NOT free, and the cost is what an operator tunes against: one index
	// scan over the open orders per tick, plus one store.Load per order older
	// than SweepMinAge. A book with many orders RESTING at PENDING_NEW (a
	// deployment with no venue wired) pays that on every tick forever.
	//
	// Zero DISABLES the periodic sweep and is a deliberate, stated choice — the
	// startup sweep still runs, and the OMS says so at boot rather than leaving
	// "off" and "on" looking identical in the logs.
	SweepInterval time.Duration

	// SweepMinAge is how old an order must be before the PERIODIC sweep will
	// touch it. It is a safety bound, not a tuning knob: handleSubmit holds no
	// per-order claim between store.Create and the claim it takes after
	// EmitAccepted, so a sweep with no age floor can drive an order a live
	// delivery is still admitting. See order.SweepOlderThan, which refuses a
	// non-positive value outright, for what it must clear (the 75s claim lease,
	// the 60s order-subject AckWait, and the 2m consumer dedup TTL).
	SweepMinAge time.Duration

	// ScheduleInterval is how often the execution-algorithm driver advances the
	// parent orders this OMS is working (#435).
	//
	// IT IS THE WORST-CASE LATENESS OF EVERY SLICE, and that makes it a
	// correctness setting rather than a tuning knob. A child is sent on the first
	// tick at or after it becomes due, so working an order in one-minute slices on
	// a five-minute tick quietly turns a 60-slice TWAP into a 12-slice one. Every
	// slice still goes out, the quantities still sum to the parent, and nothing
	// raises an error — the order simply was not worked the way somebody asked for
	// it. The OMS therefore compares this against the tightest schedule it is
	// actually working and warns when it cannot honour it, rather than leaving
	// that to be inferred from fill timestamps.
	//
	// It has NO minimum age, unlike SweepMinAge, and does not need one: the sweep
	// races live admissions because it re-drives orders somebody else may be
	// working, while this only CREATES children, and a child's id is derived — so
	// two ticks racing produce one order, refused at the primary key.
	//
	// The cost is one indexed lookup per tick, plus one child query per parent
	// being worked. A deployment working no schedules pays the first and nothing
	// else, which is why the default is short.
	//
	// Zero DISABLES the driver — and every parent order then rests forever, which
	// the OMS says out loud at boot rather than letting "off" and "nothing to do"
	// look the same.
	ScheduleInterval time.Duration

	// ProposalExpiryInterval is how often the OMS announces held orders whose
	// deadline has passed (#539). Zero DISABLES the sweep.
	//
	// OFF MEANS A HELD ORDER DIES IN SILENCE, which is the state this repository
	// shipped between #537 and #539: the proposal leaves the pending queue at
	// expires_at, enters no other queue, and the trader who submitted it sees
	// ORDER_PENDING_APPROVAL and then nothing, ever. main logs loudly when it is
	// off, because off and armed must not look the same.
	//
	// IT IS SAFE ON EVERY REPLICA. The sweep takes no lease and elects no leader;
	// ProposalStore.AnnounceExpiry is a conditional UPDATE whose rows-affected
	// decides, so two pods finding the same expired row produce one announcement.
	ProposalExpiryInterval time.Duration

	// OutboxInterval is how often the outbox relay drains (#292).
	//
	// IT IS NOT THE PUBLISH LATENCY ON THE HAPPY PATH. A handler that commits a
	// FACT kicks the relay, so the normal case is "immediately". This bounds the
	// cases nothing kicks: records left by a pod that died mid-drain, a record
	// whose first publish failed and is being retried, and any future enqueue
	// site that forgets to kick. That last one is why the tick has no off
	// switch — a forgotten kick must cost latency, never a FACT.
	//
	// There is no zero-disables option, unlike SweepInterval. A disabled sweep
	// leaves a compensator un-run; a disabled relay leaves the ONLY publisher of
	// committed FACTs un-run, which is an OMS that admits orders and tells
	// nobody. A non-positive value is refused at parse.
	OutboxInterval time.Duration
}

// CommandSubjects are the order command subjects the OMS consumes.
func (Config) CommandSubjects() []string {
	return []string{order.SubjectSubmit, order.SubjectAmend, order.SubjectCancel, order.SubjectApprove}
}

// FillSubjects are the fill FACTs the position projector consumes (OMS-01e).
func (Config) FillSubjects() []string {
	return []string{order.EventTypePartiallyFilled, order.EventTypeFilled}
}

func Load() (Config, error) {
	// Resolved before the literal so a declared-but-unreadable secret mount
	// stops Load HERE. The local helper this replaces answered that case with
	// the plaintext env var and then with "", so a failed Vault mount and an
	// unconfigured DSN produced the identical clean start. See pkg/secret.
	databaseURL, err := secret.Read("OMS_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen: envOr("OMS_LISTEN", ":8090"),
		// NO DEFAULT, DELIBERATELY. A read surface that appears because nobody
		// set a variable is a port opened by omission; the api-gateway has to be
		// pointed at it either way, so naming it is one line of config and the
		// difference between "serving" and "configured to serve".
		GRPCListen:              os.Getenv("OMS_GRPC_LISTEN"),
		LogLevel:                parseLevel(envOr("OMS_LOG_LEVEL", "info")),
		Source:                  envOr("OMS_SOURCE", "oms"),
		OTLPEndpoint:            os.Getenv("OMS_OTLP_ENDPOINT"),
		NATSURL:                 os.Getenv("OMS_NATS_URL"),
		ConsumerGroup:           envOr("OMS_CONSUMER_GROUP", "oms"),
		DatabaseURL:             databaseURL,
		Tenant:                  envOr("OMS_TENANT", "__system__"),
		RequireMandate:          os.Getenv("OMS_REQUIRE_MANDATE") == "true",
		VenueAccounts:           os.Getenv("OMS_VENUE_ACCOUNTS"),
		RequireVenueAccount:     os.Getenv("OMS_REQUIRE_VENUE_ACCOUNT") == "true",
		RequireVerifiedAccount:  os.Getenv("OMS_REQUIRE_VERIFIED_ACCOUNT") == "true",
		RequireOrderTypeSupport: os.Getenv("OMS_REQUIRE_ORDER_TYPE_SUPPORT") == "true",
		SimVenueMIC:             envOr("OMS_SIM_VENUE_MIC", "XSIM"),
		DefaultVenueMIC:         os.Getenv("OMS_DEFAULT_VENUE_MIC"),
		VenueEndpoints:          os.Getenv("OMS_VENUE_ENDPOINTS"),
		SPIFFESocket:            os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		BaseCurrency:            envOr("OMS_BASE_CURRENCY", "USD"),
		PriceSubjects:           splitSubjects(envOr("OMS_PRICE_SUBJECTS", "market.*.trade,market.*.quote")),
	}

	if len(cfg.PriceSubjects) == 0 {
		return Config{}, fmt.Errorf("OMS_PRICE_SUBJECTS: at least one subject is required; " +
			"a pod subscribing to nothing folds no marks and refuses every MARKET/STOP order")
	}

	maxAge, err := time.ParseDuration(envOr("OMS_PRICE_MAX_AGE", "30s"))
	if err != nil {
		return Config{}, fmt.Errorf("OMS_PRICE_MAX_AGE: %w", err)
	}
	if maxAge <= 0 {
		return Config{}, fmt.Errorf("OMS_PRICE_MAX_AGE: must be positive (got %v); a non-positive value would disable the staleness bound and admit orders against arbitrarily old prices", maxAge)
	}
	cfg.PriceMaxAge = maxAge

	// OMS_SWEEP_INTERVAL: 60s. The exposure this bounds is an admitted order that
	// no downstream service has heard of, so the window is measured against the
	// fund's risk being wrong, not against a scrape budget. A minute plus the 2m
	// floor below puts the worst case at three minutes, where it was previously
	// bounded only by the next pod restart. "0" turns it off, and is the value an
	// operator with a very large resting book sets deliberately after reading
	// what a tick costs (see Config.SweepInterval).
	sweepInterval, err := time.ParseDuration(envOr("OMS_SWEEP_INTERVAL", "60s"))
	if err != nil {
		return Config{}, fmt.Errorf("OMS_SWEEP_INTERVAL: %w", err)
	}
	if sweepInterval < 0 {
		return Config{}, fmt.Errorf("OMS_SWEEP_INTERVAL: must not be negative (got %v); "+
			"use 0 to disable the periodic sweep", sweepInterval)
	}
	cfg.SweepInterval = sweepInterval

	// OMS_SCHEDULE_INTERVAL: 10s (#435). Short, because this is not a compensator
	// bounding how long a fault goes unnoticed — it is the granularity at which
	// orders are actually worked, and an operator asking for one-minute slices
	// must get something recognisably close to them. A tick that finds no parent
	// costs one indexed lookup.
	//
	// "0" turns the driver off, which means every parent order rests forever. It
	// is a deliberate, stated choice and the OMS warns at boot when it is set.
	scheduleInterval, err := time.ParseDuration(envOr("OMS_SCHEDULE_INTERVAL", "10s"))
	if err != nil {
		return Config{}, fmt.Errorf("OMS_SCHEDULE_INTERVAL: %w", err)
	}
	if scheduleInterval < 0 {
		return Config{}, fmt.Errorf("OMS_SCHEDULE_INTERVAL: must not be negative (got %v); "+
			"use 0 to disable the execution-algorithm driver", scheduleInterval)
	}
	cfg.ScheduleInterval = scheduleInterval

	// OMS_SWEEP_MIN_AGE: 2m. Longer than every recovery already in flight for a
	// young order — the 60s AckWait on order subjects and the 75s in-process
	// dedup claim lease derived from it — so the periodic sweep never competes
	// with the broker's own redelivery or with a live admission that has not yet
	// taken its claim. Lowering it below those does not make recovery faster; it
	// makes the compensator race the thing already recovering.
	sweepMinAge, err := time.ParseDuration(envOr("OMS_SWEEP_MIN_AGE", "2m"))
	if err != nil {
		return Config{}, fmt.Errorf("OMS_SWEEP_MIN_AGE: %w", err)
	}
	if cfg.SweepInterval > 0 && sweepMinAge <= 0 {
		return Config{}, fmt.Errorf("OMS_SWEEP_MIN_AGE: must be positive (got %v) while "+
			"OMS_SWEEP_INTERVAL is set; a periodic sweep with no age floor can claim an order a "+
			"live delivery is still admitting, and both would announce it", sweepMinAge)
	}
	cfg.SweepMinAge = sweepMinAge

	// OMS_OUTBOX_INTERVAL: 1s. Short, because this is the retry cadence for a
	// FACT the estate is currently missing and the drain costs one partial-index
	// scan over an empty backlog on a healthy pod. It is NOT the happy-path
	// latency — an admitting handler kicks the relay directly.
	//
	// ZERO IS REFUSED, unlike OMS_SWEEP_INTERVAL. Zero there disables a
	// compensator; zero here disables the only publisher of FACTs the store has
	// already committed, which would leave the OMS admitting orders and telling
	// nobody — permanently, and with the store looking perfectly healthy. There
	// is no off switch, so nobody can reach for one.
	outboxInterval, err := time.ParseDuration(envOr("OMS_OUTBOX_INTERVAL", "1s"))
	if err != nil {
		return Config{}, fmt.Errorf("OMS_OUTBOX_INTERVAL: %w", err)
	}
	if outboxInterval <= 0 {
		return Config{}, fmt.Errorf("OMS_OUTBOX_INTERVAL: must be positive (got %v); the outbox relay is "+
			"the only thing that publishes a FACT the order store has committed, so there is no value that "+
			"turns it off — an OMS with no relay admits orders no downstream service ever hears about",
			outboxInterval)
	}
	cfg.OutboxInterval = outboxInterval

	// MAKER-CHECKER ON ORDER SUBMISSION (#410, act three). Three states, and the
	// third is the one the owner ruled on explicitly.
	// Default 60s, matching OMS_SWEEP_INTERVAL: both are background passes whose
	// cost is one indexed query, and a deadline measured in hours does not need a
	// tighter tick than that.
	cfg.ProposalExpiryInterval = 60 * time.Second
	if raw := strings.TrimSpace(os.Getenv("OMS_PROPOSAL_EXPIRY_INTERVAL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("OMS_PROPOSAL_EXPIRY_INTERVAL: %w", err)
		}
		if d < 0 {
			return Config{}, fmt.Errorf("OMS_PROPOSAL_EXPIRY_INTERVAL: must not be negative (got %s); "+
				"use 0 to disable the sweep deliberately", raw)
		}
		cfg.ProposalExpiryInterval = d
	}
	cfg.RequireDualControl = os.Getenv("OMS_REQUIRE_DUAL_CONTROL") == "true"
	if raw := strings.TrimSpace(os.Getenv("OMS_DUAL_CONTROL_MIN_NOTIONAL")); raw != "" {
		amount, currency, err := parseNotional(raw)
		if err != nil {
			return Config{}, fmt.Errorf("OMS_DUAL_CONTROL_MIN_NOTIONAL: %w", err)
		}
		if amount.Sign() <= 0 {
			return Config{}, fmt.Errorf("OMS_DUAL_CONTROL_MIN_NOTIONAL: must be positive (got %s); a "+
				"non-positive threshold puts EVERY order at or above it, which is not 'dual control on "+
				"large orders' but 'dual control on everything' arrived at by arithmetic", amount.RatString())
		}
		// THE CURRENCY MUST BE THE ONE ORDERS ARE VALUED IN. There is no FX layer
		// on the admission path (RISK-06) and common.v1.Decimal carries no
		// currency, so a threshold in another currency would be compared against
		// a number that means something else — silently, and in whichever
		// direction the rate happens to run.
		if currency != cfg.BaseCurrency {
			return Config{}, fmt.Errorf("OMS_DUAL_CONTROL_MIN_NOTIONAL is denominated in %s but orders "+
				"are valued in OMS_BASE_CURRENCY=%s, and there is no FX conversion on the admission path "+
				"— comparing them would silently mis-scale the threshold. Set both to the same currency",
				currency, cfg.BaseCurrency)
		}
		cfg.DualControlMinNotional = amount
		cfg.DualControlNotionalCurrency = currency
	}
	// THE RULING, VERBATIM: "require dual control" set with NO threshold refuses to
	// start, naming both variables. Not a default — a default here silently picks a
	// number nobody chose, on a control whose whole purpose is that a human decided.
	// Same shape as the OMS_SWEEP_MIN_AGE check above: a feature armed without its
	// numeric bound is a Load failure, not a runtime surprise.
	if cfg.RequireDualControl && cfg.DualControlMinNotional == nil {
		return Config{}, fmt.Errorf("OMS_REQUIRE_DUAL_CONTROL=true requires OMS_DUAL_CONTROL_MIN_NOTIONAL " +
			"to be set (e.g. \"1000000 USD\"): there is no threshold to compare an order against, and this " +
			"service will not default one — the number is the decision")
	}
	// THE "IT CANNOT BE ARMED AT ALL YET" REFUSAL THAT USED TO SIT HERE IS GONE.
	// It existed because the OMS had nowhere to record an order awaiting approval,
	// so arming would have REJECTED every order at or above the threshold instead
	// of holding it. migrations/0009_order_proposals.sql is that place, and
	// internal/order's hold() writes to it in the same transaction as the FACT
	// announcing the order is held. The flag is now a real posture.
	//
	// WHAT ARMING STILL COSTS AN OPERATOR, because a control nobody watches stops
	// being a control (#495 said this of the override queue and it is truer here):
	// a held order does not trade until a DIFFERENT authenticated subject approves
	// it, and it EXPIRES if nobody does. Arm this when somebody owns the pending
	// queue.

	// THE COLLATERAL-SEGREGATION REFUSAL BELONGS HERE, NOT 200 LINES INTO STARTUP
	// (#68).
	//
	// An exchange margins, nets and LIQUIDATES per account, so two portfolios bound
	// to one exchange account are not segregated whatever the ledger says: a
	// drawdown in the first consumes the second's margin while both books still
	// show their own cash. The platform's answer is to refuse to start, and that
	// refusal is the feature the deploy-time basket contract rests on.
	//
	// It was only reached AFTER the OMS had connected to NATS and opened Postgres.
	// ParseBindings is a pure function of a string that is already in hand here, so
	// nothing about that ordering was necessary — and it cost two things:
	//
	//   - AN OPERATOR LEARNS LATE. With a broker that is not up yet, a shared
	//     binding hides behind a connection error: fix the broker, redeploy, and
	//     only then find out the config was never safe to trade on.
	//   - AND IT COULD NOT BE PROVEN. Reaching the refusal required a live broker
	//     and database, so the one guarantee the basket contract depends on had no
	//     test that ran it — while ParseBindings' own unit test proved the parser,
	//     not the refusal to start.
	//
	// Validating at Load makes it the FIRST thing that fails and the easiest thing
	// to test: config_binding_test.go now runs the refusal with no I/O at all.
	// main.go still parses (it needs the bindings themselves); it can simply no
	// longer be the place this is first discovered.
	if _, err := execution.ParseBindings(cfg.VenueAccounts); err != nil {
		return Config{}, fmt.Errorf("OMS_VENUE_ACCOUNTS is not safe to trade on: %w", err)
	}

	return cfg, nil
}

// parseNotional parses "<amount> <CURRENCY>" — a money value that cannot be set
// without its unit.
//
// A BARE NUMBER IS AN ERROR, NOT A NUMBER IN THE DEFAULT CURRENCY. That is the
// whole reason this is one string rather than two variables: two variables can be
// changed independently, and the failure mode of changing OMS_BASE_CURRENCY and
// forgetting the threshold is a control that silently applies at the wrong scale.
// One string makes the amount and its unit impossible to separate, and makes
// forgetting the unit a refusal to start.
func parseNotional(s string) (*big.Rat, string, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return nil, "", fmt.Errorf("must be an amount AND a currency, e.g. \"1000000 USD\" (got %q); "+
			"a bare number would be compared against order notionals with no currency attached, and "+
			"this platform has no FX on the admission path to reconcile them", s)
	}
	amount, err := dec.ParseRat(fields[0])
	if err != nil {
		return nil, "", fmt.Errorf("%q is not a decimal amount", fields[0])
	}
	currency := strings.ToUpper(fields[1])
	return amount, currency, nil
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

// splitSubjects parses a comma-separated subject list, trimming whitespace and
// dropping empty entries. An all-empty input yields an empty slice, which Load
// rejects — subscribing to nothing is a silent trading outage, not a default.
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
