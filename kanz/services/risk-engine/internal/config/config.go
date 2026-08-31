package config

import (
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/pit"
	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/pkg/secret"
)

// Config is the risk-engine runtime configuration. Sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine (EVT-08). Empty ⇒ the service runs
	// HTTP-only (probes up, no ingestion) — useful for a bare scaffold
	// deploy or local boot without a broker.
	NATSURL string
	// Source is the producer identity stamped on emitted FACTs (EVT-17b).
	Source string
	// RefData wires the instrument classifier every named stress scenario's
	// sector shocks resolve through (#640). Unwired, EvaluateScenario REFUSES a
	// scenario carrying SectorShocks rather than returning the unshocked book.
	RefData refdata.Config
	// Tenant is the tenant_id stamped on emitted FACTs (MT-01a). The recomputer
	// publishes derived risk events from a background ctx (debounced, async), so
	// the triggering event's tenant can't propagate via ctx here — this is the
	// fallback. Defaults to the reserved "__system__"; a single-tenant
	// deployment sets RISK_ENGINE_TENANT. True per-portfolio tenant stamping
	// (the engine carrying portfolio→tenant in state) is an MT-01d follow-up.
	Tenant string

	// DatabaseURL is the Postgres DSN for durable state (PERS-01). Empty ⇒
	// state is in-memory only: no bootstrap restore, no periodic snapshot,
	// re-baselines from the live spine on restart.
	DatabaseURL string
	// SnapshotInterval tunes the periodic durable-snapshot cadence (PARITY-02f):
	// shorter shrinks restart-to-ready replay at the cost of more SQL write
	// volume, longer suits quiet books. A deploy-time knob so cadence tuning
	// needs no rebuild. Zero (unset/unparseable) ⇒ engine.DefaultSnapshotInterval.
	SnapshotInterval time.Duration
	// KafkaBrokers is the durable-log bootstrap list (EVT-09) used by the
	// PERS-01d bootstrap replay. Empty ⇒ replay is skipped; the engine
	// restores from the latest durable snapshot and relies on the live NATS
	// spine to cover everything since. Comma-separated in the environment.
	KafkaBrokers []string

	// RequireValidatedAnalytics arms the SR 11-7 model-validation gate
	// (RISK_REQUIRE_VALIDATED_ANALYTICS, #471): an analytic that holds no
	// current, passing, signed validation and is not a NAMED exemption in
	// benchmarks.ValidationExemptions() stops this engine at startup.
	//
	// IT SHIPS TRUE, WHICH IS THE OPPOSITE OF THE REST OF THE *_REQUIRE_* FAMILY,
	// and the reason the family ships false does not apply. OMS_REQUIRE_MANDATE
	// and OMS_REQUIRE_VERIFIED_ACCOUNT refuse over ESTATE DATA an operator has to
	// go and correct — unmandated portfolios, unverified accounts — so arming them
	// is a trading outage until somebody finishes a backlog. This one refuses over
	// something no operator can be behind on: the twelve analytics this build
	// carries, graded against benchmarks in the same build.
	//
	// THE EVIDENCE IS NOT BAKED IN — IT IS RECOMPUTED ON EVERY BOOT, AND THAT IS
	// THE ASSUMPTION THIS DEFAULT RESTS ON. benchmarks.Reports() calls
	// validation.Validate, which RUNS the case sets against the pricers in this
	// process. So "it passed in CI" only implies "it passes in the pod" if the two
	// compute the same floats. They do, today, for a reason that is not a property
	// of this package and can change without anyone touching it:
	//
	//   - .github/workflows/build.yml's docker/build-push-action step passes NO
	//     `platforms:` key, and no Dockerfile in this repo sets GOARCH or reads
	//     TARGETARCH. Every image is therefore single-arch, native to the runner.
	//   - Both that job and kanz-ci.yml's `go test -race -p 1 ./...` are
	//     `runs-on: ubuntu-latest`, so CI executes these same case sets on the
	//     same architecture the image is built for.
	//   - IEEE-754 float64 is deterministic given identical architecture and Go
	//     version, so a validation that passes in CI passes in the pod.
	//
	// THE DAY THIS ESTATE PUBLISHES A MULTI-ARCH IMAGE, that chain breaks and
	// boot-time validation becomes architecture-dependent. Go permits FMA
	// contraction on arm64 and not on amd64, and math.Exp/Log are assembly on some
	// arches and pure Go on others — a LAST-ULP difference in one transcendental
	// against a 1e-9 identity tolerance would then CrashLoopBackOff EVERY
	// risk-engine replica on the arch CI did not run, with a green pipeline behind
	// it. Adding `platforms:` to build.yml is therefore a change to THIS default:
	// either the case sets get arch-aware tolerances, or this ships false.
	//
	// (Verified 2026-08-19 against build.yml and kanz-ci.yml at 06e4779. It is the
	// premise, not the conclusion, that a future reader must re-check.)
	//
	// SO THE DEFAULT IS THE ARGUMENT. Defaulting false would mean shipping the
	// posture that has been in place since #494 — a complete gate nobody turned
	// on — and calling the issue resolved. The exposure the true default creates
	// is the honest one: a build whose analytics cannot vouch for themselves does
	// not serve risk numbers.
	//
	// UNSET ⇒ TRUE. Set RISK_REQUIRE_VALIDATED_ANALYTICS=false to fall back to
	// counting-and-warning; anything strconv.ParseBool cannot read is an error,
	// never a silent disarm.
	RequireValidatedAnalytics bool

	// MarketDataURL is the Postgres/Timescale DSN of the shared market-data
	// price-history store (MODEL-01b) that the market-data service writes. When
	// set, the risk-engine reads it point-in-time-correct to drive the real
	// historical-simulation VaR99 (RISK-12), overriding the RISK-07 1%×gross
	// placeholder. Read-only from here; price history is universal market fact,
	// not tenant-owned, so the connection carries no tenant GUC. Empty ⇒ no price
	// store: VaR99 stays the placeholder (the honest no-market-data fallback).
	MarketDataURL string

	// LiquidityVenue is the MIC whose candles the liquidity measures measure ADV
	// from (RISK_ENGINE_LIQUIDITY_VENUE). REQUIRED to serve the liquidity family,
	// and there is deliberately NO DEFAULT and no "any" value.
	//
	// ADV SUMMED ACROSS VENUES IS A DIFFERENT NUMBER AND IT FLATTERS.
	// liquidity.Model.DaysToLiquidate divides size by participation × ADV, so
	// adding three venues' volume together asserts the desk can work the order on
	// all three at once and reports a liquidation horizon three times shorter than
	// any single book supports — shorter is the direction that makes a limit check
	// pass. Guessing a venue here would make that assertion on an operator's
	// behalf, in a config file nobody wrote.
	//
	// Empty ⇒ LiquidationHorizon is NOT registered and the liquidity family stays
	// dark; the composition root WARNs naming the consequence and
	// kanz_risk_measure_live{family="liquidity"} reads 0. A desk trading several
	// venues runs one deployment per venue and reconciles above this layer, where
	// the routing assumption is visible.
	LiquidityVenue string

	// VenueAccounts binds portfolios to the exchange accounts they may be
	// liquidated in — the same deploy-time spec the OMS reads, in the same syntax,
	// parsed by the same execution.ParseBindings:
	//
	//	RISK_ENGINE_VENUE_ACCOUNTS="tenant/portfolio@MIC=account,..."
	//
	// THE ENGINE READS IT BECAUSE THE MEASURE IS PER PORTFOLIO AND THE EXCHANGE
	// MARGINS PER ACCOUNT. collateral.v1.VenueMarginState is published by a venue
	// adapter, which holds one API credential and therefore IS one account; it
	// cannot know which portfolio that account backs. Without this mapping
	// LiquidationProximity has no boundary to measure a distance to, and the
	// honest answer is a whole-book refusal — which is what it gives.
	//
	// IT MUST NAME THE SAME ACCOUNTS THE OMS SPENDS FROM. Two specs that disagree
	// do not fail: they measure a real account that is not the one the portfolio
	// trades in. What makes that visible rather than silent is that a bound
	// account nobody observes ages out to UNKNOWN within venuemargin.DefaultMaxAge
	// and the measure refuses with margin_unknown, per account, counted — so the
	// divergence surfaces as refusals naming the account, not as a plausible
	// number.
	//
	// Empty ⇒ the margin family stays dark: RegisterMarginRisk is called with no
	// provider, fires no_margin_provider once, and
	// kanz_risk_measure_live{family="margin"} reads 0. A registered measure that
	// refuses on every portfolio forever would read as live, which is the one
	// state an operator must not be shown.
	VenueAccounts string

	// OTLPEndpoint is the OTel collector (host:port) for span export (OBS-01).
	// Empty ⇒ spans are created and trace context propagates, but are not
	// exported — startup never blocks on a collector.
	OTLPEndpoint string

	// ShardMembers is the full risk-engine fleet member set (PARITY-05a) used
	// to build the consistent-hash ring. Every replica MUST be given the same
	// list. Comma-separated in the environment.
	//
	// THE TWO BELOW ARE SET TOGETHER OR NOT AT ALL (#110). Both empty is the
	// unsharded default — this replica owns every portfolio, which is the
	// posture the estate actually runs and which app.ShardPosture WARNs about
	// by name. Either one alone, or a ShardSelf absent from ShardMembers, is
	// REFUSED at startup by shard.NewAssignment: each of those produced a
	// replica that owned nothing (or everything) while its probes stayed green.
	//
	// Note what a correct list requires: it must name exactly the pods that are
	// running. risk-engine is a KEDA-scaled Rollout, so no static value can do
	// that — see test/arch/shard_membership_is_not_static_test.go, which forbids
	// pairing a hand-written list with an autoscaled workload.
	ShardMembers []string
	// ShardSelf is THIS replica's member id — its identity on the ring
	// (typically a StatefulSet pod name / ordinal). It MUST appear in
	// ShardMembers; anything else is refused rather than silently mis-sharded.
	ShardSelf string

	// RedisURL enables cross-pod shared-state dedup (PARITY-05c): with N
	// replicas, the consumer switches from the per-instance in-memory dedup
	// window to a Redis-backed bus.Deduper so a redelivery landing on a
	// different replica than the original is still recognized. Empty ⇒
	// per-instance dedup (the pre-05c default). Only honored in the `redis`
	// build; the default build ignores it (the concrete go-redis client is
	// behind the build tag, the PARITY-04a/04h stance).
	RedisURL string

	// Calibration (WIRE-01c): the in-process curve-calibration scheduler. The
	// risk-engine is the risk module's composition root, so it — not the
	// market-data service — wires the PARITY-03a curve.Calibrator, subscribing
	// the market quote spine into a latest-quote cache and driving Refresh on a
	// nightly + intraday cadence. Off unless CalibrationInterval is set AND
	// CalibrationRates names a rate universe.
	//
	// CalibrationInterval is the intraday refresh cadence
	// (RISK_ENGINE_CALIBRATION_INTERVAL, e.g. "5m"). Zero ⇒ scheduler disabled.
	CalibrationInterval time.Duration
	// CalibrationNightly is the nightly-close cadence
	// (RISK_ENGINE_CALIBRATION_NIGHTLY_INTERVAL); default 24h when the scheduler
	// is enabled.
	CalibrationNightly time.Duration
	// CalibrationRates is the raw rate-instrument reference spec
	// (RISK_ENGINE_CALIBRATION_RATES), parsed by livequote.ParseRateInstruments.
	CalibrationRates string
	// CalibrationHorizon is how far back the point-in-time curve store retains
	// calibrated versions (RISK_ENGINE_CALIBRATION_HORIZON), measured from the
	// newest version held for a currency. Unset ⇒ pit.DefaultHorizon.
	//
	// IT IS PAIRED WITH THE CADENCE, AND Load REFUSES THE PAIR IT CANNOT HOLD
	// (#811). Retention used to be unbounded — a version per refresh, none ever
	// removed — so the pod's memory tracked uptime rather than the book. A
	// horizon bounds it at horizon/CalibrationInterval versions per currency,
	// which means the two settings are one decision: a horizon that looks
	// reasonable beside a cadence that does not is how the bound gets set and
	// still fails. Load computes the product and refuses to start above
	// pit.MaxVersionsPerKey rather than letting the store discover it.
	CalibrationHorizon time.Duration
	// MarketSubjects are the market.v1 quote subjects the calibration cache
	// subscribes (RISK_ENGINE_MARKET_SUBJECTS); default the MARKET wildcard.
	// Comma-separated. Only used when the scheduler is enabled.
	MarketSubjects []string

	// GRPCListen is the address the risk query gRPC server (API-01b) binds.
	// Empty ⇒ the query server is not started (probes + ingestion only).
	GRPCListen string
	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the gRPC query server requires mTLS with an in-mesh peer SVID
	// (SEC-01b); empty ⇒ the server listens in plaintext (local/dev).
	SPIFFESocket string
}

// DefaultCalibrationNightly is the nightly-close cadence when the scheduler is
// enabled but RISK_ENGINE_CALIBRATION_NIGHTLY_INTERVAL is unset.
const DefaultCalibrationNightly = 24 * time.Hour

// DefaultMarketSubjects is the calibration cache subscription when none is set.
var DefaultMarketSubjects = []string{"market.>"}

func Load() (Config, error) {
	// Read HERE so a malformed refresh interval stops Load with every other
	// configuration error, rather than at the one line that builds the cache.
	refData, err := refdata.LoadConfig("RISK_ENGINE")
	if err != nil {
		return Config{}, err
	}
	nightly := parseDuration(os.Getenv("RISK_ENGINE_CALIBRATION_NIGHTLY_INTERVAL"))
	if nightly <= 0 {
		nightly = DefaultCalibrationNightly
	}
	marketSubjects := env.SplitList(os.Getenv("RISK_ENGINE_MARKET_SUBJECTS"))
	if len(marketSubjects) == 0 {
		marketSubjects = DefaultMarketSubjects
	}
	calibrationInterval := parseDuration(os.Getenv("RISK_ENGINE_CALIBRATION_INTERVAL"))
	horizon, err := calibrationHorizon(calibrationInterval)
	if err != nil {
		return Config{}, err
	}

	// All three are resolved before the literal so a DECLARED-but-unreadable
	// secret mount stops Load here. The local helper this replaces answered a
	// failed mount with the plaintext env var and then with "", and for every one
	// of these three "" is a DOCUMENTED, LEGITIMATE degraded mode — see the field
	// comments above: in-memory state with no restore, VaR99 back to the 1%×gross
	// placeholder, per-instance dedup instead of cross-pod. So a broken Vault
	// mount downgraded the engine into a posture that reads as deliberate, on a
	// clean start, with nothing to distinguish it from a deployment that meant it.
	//
	// That helper's own doc comment claimed the fall-through let "the downstream
	// required-field check surface the misconfiguration". There is no such check
	// here; none of the three is required. See pkg/secret.
	databaseURL, err := secret.Read("RISK_ENGINE_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	marketDataURL, err := secret.Read("RISK_ENGINE_MARKETDATA_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	redisURL, err := secret.Read("RISK_ENGINE_REDIS_URL")
	if err != nil {
		return Config{}, err
	}

	// NOT "RISK_ENGINE_" PREFIXED, and that is a deliberate deviation from every
	// other key in this file. The control is named RISK_REQUIRE_VALIDATED_ANALYTICS
	// in #471, in internal/execution's comparison of the deny-by-default family and
	// in services/datamaster's — it was referred to by that spelling in code before
	// it existed, and inventing a second one now would leave those references
	// pointing at nothing. parseBoolDefault refuses the near-miss spelling rather
	// than letting an operator who typed the prefixed form disarm the gate in
	// silence, which is the only way this deviation could cost anything.
	requireValidated, err := parseBoolDefault("RISK_REQUIRE_VALIDATED_ANALYTICS", true,
		"RISK_ENGINE_REQUIRE_VALIDATED_ANALYTICS")
	if err != nil {
		return Config{}, err
	}

	// THE SEGREGATION REFUSAL IS VALIDATED HERE, WHERE THE OMS VALIDATES ITS OWN.
	//
	// An account bound to two portfolios is one collateral pool the exchange will
	// liquidate as one, and a proximity measured on it attributes one portfolio's
	// distance-to-liquidation to another — a number that is not merely imprecise
	// but wrong, in the direction that says a portfolio is safer than it is.
	// ParseBindings is a pure function of a string already in hand, so this is the
	// first thing that can fail and the only place it can be proven without a
	// broker and a database.
	venueAccounts := strings.TrimSpace(os.Getenv("RISK_ENGINE_VENUE_ACCOUNTS"))
	if _, err := execution.ParseBindings(venueAccounts); err != nil {
		return Config{}, fmt.Errorf("RISK_ENGINE_VENUE_ACCOUNTS is not a safe binding set: %w", err)
	}

	return Config{
		Listen:           env.Or("RISK_ENGINE_LISTEN", ":8081"),
		LogLevel:         env.ParseLevelOr(env.Or("RISK_ENGINE_LOG_LEVEL", "info"), slog.LevelInfo),
		NATSURL:          os.Getenv("RISK_ENGINE_NATS_URL"),
		Source:           env.Or("RISK_ENGINE_SOURCE", "risk-engine"),
		Tenant:           env.Or("RISK_ENGINE_TENANT", "__system__"),
		RefData:          refData,
		DatabaseURL:      databaseURL,
		SnapshotInterval: parseDuration(os.Getenv("RISK_ENGINE_SNAPSHOT_INTERVAL")),
		KafkaBrokers:     env.SplitList(os.Getenv("RISK_ENGINE_KAFKA_BROKERS")),
		MarketDataURL:    marketDataURL,

		RequireValidatedAnalytics: requireValidated,

		LiquidityVenue: strings.TrimSpace(os.Getenv("RISK_ENGINE_LIQUIDITY_VENUE")),
		VenueAccounts:  venueAccounts,
		ShardMembers:   env.SplitList(os.Getenv("RISK_ENGINE_SHARD_MEMBERS")),
		// TRIMMED BECAUSE THE MEMBER LIST IS. splitList trims each member, so an
		// id carrying the trailing space a YAML block scalar or a shell `export`
		// leaves behind could never match one — and an id that matches nothing
		// owns nothing. That now refuses the start (shard.ErrSelfNotAMember)
		// instead of silently discarding the spine, but the operator should never
		// have reached either outcome over whitespace.
		ShardSelf:    strings.TrimSpace(os.Getenv("RISK_ENGINE_SHARD_SELF")),
		RedisURL:     redisURL,
		OTLPEndpoint: os.Getenv("RISK_ENGINE_OTLP_ENDPOINT"),
		GRPCListen:   os.Getenv("RISK_ENGINE_GRPC_LISTEN"),
		SPIFFESocket: os.Getenv("RISK_ENGINE_SPIFFE_SOCKET"),

		CalibrationInterval: calibrationInterval,
		CalibrationNightly:  nightly,
		CalibrationRates:    os.Getenv("RISK_ENGINE_CALIBRATION_RATES"),
		CalibrationHorizon:  horizon,
		MarketSubjects:      marketSubjects,
	}, nil
}

// parseBoolDefault reads a boolean env var, falling back to def when it is unset.
//
// A VALUE strconv.ParseBool CANNOT READ IS AN ERROR, never a silent fallback.
// The silent version is what makes a control useless in both directions: an
// operator who writes RISK_REQUIRE_VALIDATED_ANALYTICS=yes has stated an intent as
// clearly as one who writes true, and an operator who writes =no and gets the
// default true would be refused with a message telling them to unset a variable
// they can see they have already set. (Same stance as api-gateway's parseBool;
// this one carries a default and a near-miss list, which that one does not.)
//
// A NEAR MISS IS ALSO AN ERROR, and that is the half that matters for a control
// whose default is ON. RISK_REQUIRE_VALIDATED_ANALYTICS breaks this service's
// RISK_ENGINE_ prefix convention, so RISK_ENGINE_REQUIRE_VALIDATED_ANALYTICS is
// the spelling an operator reaches for by muscle memory. Ignored, it would read
// as a disarm that took effect — the operator sees their variable in the pod
// spec, and the gate is still armed (or, if they meant to arm it, still off).
// Refusing to start names the key they actually want.
func parseBoolDefault(key string, def bool, nearMisses ...string) (bool, error) {
	for _, nm := range nearMisses {
		if strings.TrimSpace(os.Getenv(nm)) != "" {
			return false, fmt.Errorf("risk-engine: %s is set, but this control is spelled %s — "+
				"unset %s and set %s, or the value you set has no effect", nm, key, nm, key)
		}
	}
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("risk-engine: %s=%q is not a boolean (use true or false)", key, raw)
	}
	return v, nil
}

// calibrationHorizon resolves RISK_ENGINE_CALIBRATION_HORIZON against the
// intraday cadence and refuses the pair the curve store cannot hold (#811).
//
// THREE REFUSALS, AND EACH ONE IS A CASE THAT USED TO LOOK HEALTHY.
//
//  1. A MALFORMED VALUE IS AN ERROR, not a fallback. parseDuration answers 0 for
//     anything time.ParseDuration rejects, and 0 here would silently become the
//     default — so an operator who wrote "7days" instead of "168h" would be told
//     nothing, see their variable in the pod spec, and get a horizon they did not
//     choose. The same stance parseBoolDefault takes, for the same reason.
//
//  2. A NON-POSITIVE VALUE IS AN ERROR rather than "retain everything". Unbounded
//     retention is the defect this configuration exists to close, and there is
//     deliberately no spelling that reopens it: pit.Put would treat a zero
//     horizon as "keep every version", so an operator who set 0 believing it
//     meant "no limit" would get exactly #811 back with the knob reading as
//     configured.
//
//  3. A CADENCE THE HORIZON CANNOT HOLD IS AN ERROR AT STARTUP. horizon/interval
//     is the retained-version count per currency, and the two settings are one
//     decision made in two places. A one-second cadence under the seven-day
//     default is ~605k curves per currency in a pod that reports itself healthy
//     until it is OOM-killed — and calibration latency climbs the whole way,
//     because a store this size is what every Refresh inserts into. Refusing the
//     pair names both numbers while an operator can still change either.
//
// Only the intraday cadence is checked. The nightly job publishes at the same
// as-ofs into the same list at 24h spacing, so it adds single digits to the
// count and cannot be what breaches the ceiling.
func calibrationHorizon(interval time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("RISK_ENGINE_CALIBRATION_HORIZON"))
	horizon := pit.DefaultHorizon
	if raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("risk-engine: RISK_ENGINE_CALIBRATION_HORIZON=%q is not a Go "+
				"duration (e.g. 168h for the %s default)", raw, pit.DefaultHorizon)
		}
		if d <= 0 {
			return 0, fmt.Errorf("risk-engine: RISK_ENGINE_CALIBRATION_HORIZON=%q is not positive. "+
				"There is no value meaning \"retain every calibrated curve\" — that is the "+
				"unbounded retention #811 closed. Unset the variable for the %s default",
				raw, pit.DefaultHorizon)
		}
		horizon = d
	}
	if interval <= 0 {
		return horizon, nil // the calibration scheduler is off; nothing publishes versions
	}
	if retained := int64(horizon / interval); retained > pit.MaxVersionsPerKey {
		return 0, fmt.Errorf("risk-engine: a %s calibration horizon at a %s intraday cadence "+
			"retains %d curves per currency, above the %d ceiling. Lengthen "+
			"RISK_ENGINE_CALIBRATION_INTERVAL or shorten RISK_ENGINE_CALIBRATION_HORIZON — the "+
			"two are one decision, and the pod would otherwise grow until it is OOM-killed",
			horizon, interval, retained, pit.MaxVersionsPerKey)
	}
	return horizon, nil
}

// parseDuration parses a Go duration (e.g. "30s", "2m"); an empty or malformed
// value yields 0, which the snapshotter maps to engine.DefaultSnapshotInterval.
func parseDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}
