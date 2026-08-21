package config

import (
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/pkg/secret"
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
	// openStores REFUSES TO START unless AllowEphemeralMaster says the deployment
	// accepts losing every operator override on restart (#261).
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

	// AllowEphemeralMaster (DATAMASTER_ALLOW_EPHEMERAL_MASTER=true) opts in to the
	// in-memory golden store + exception queue when no DSN is set. openStores
	// REFUSES TO START without it, on the same principle as AllowSim directly
	// above and for a sharper reason: an operator override is a named human's
	// signed decision, and in a map it is deleted by the next rolling update
	// (#261). No shipped manifest sets it.
	AllowEphemeralMaster bool

	// RequireDualControl (DATAMASTER_REQUIRE_DUAL_CONTROL=true) arms maker-checker
	// on the pricing override (#410): the override is recorded as PENDING and
	// takes effect only when a DIFFERENT authenticated person approves it.
	//
	// DEFAULT FALSE, DELIBERATELY. Arming it on a deploy would turn every existing
	// override caller's 200 into a 202 that completes only when a second person
	// acts — a valuation outage delivered by a security improvement. The control
	// ships built and counted (kanz_datamaster_overrides_total{signatures}), and
	// is armed with the list of override callers in hand. Same stance as
	// OMS_REQUIRE_MANDATE and OMS_REQUIRE_VERIFIED_ACCOUNT.
	//
	// UNARMED IS NOT UNRECORDED: every override still says whether a second
	// person signed it, so turning this on later does not retroactively make the
	// old rows ambiguous.
	RequireDualControl bool

	// Source is this service's identity on the bus: it becomes the envelope's
	// producer, which is how a consumer knows which service asserted a FACT.
	Source string

	// SPIFFESocket is the workload-identity socket the mTLS dial uses. Empty
	// means plaintext, which is the dev path — the mesh itself refuses to be
	// half-configured.
	SPIFFESocket string

	// NATSURL is the spine the override FACT is published onto (#410).
	//
	// EMPTY IS NOT A DISABLE — it is a BACKLOG. The FACT is committed to the
	// outbox in the same transaction as the override whether or not this is set,
	// so with no URL the overrides keep working and their announcements pile up
	// in Postgres until a relay can drain them. That is the correct failure mode
	// (nothing is lost, nothing is blocked) and the dangerous one to leave
	// unattended, which is why the composition root WARNS and
	// kanz_datamaster_outbox_oldest_pending_seconds climbs.
	NATSURL string

	// OutboxInterval is how often the relay drains. Zero uses the relay default.
	OutboxInterval time.Duration

	// DualControlTTL is how long a pending override proposal stays approvable.
	// Long enough for an approver in another timezone; short enough that a
	// signature cannot be collected against a stale view of the book.
	DualControlTTL time.Duration

	// LapsedProposalRetention is how long a proposal that expired UNSIGNED stays
	// listed, and therefore how long its row lives (#563).
	//
	// ONE KNOB FOR BOTH ON PURPOSE. A separate visibility window and purge cutoff
	// would create a span where the row exists and the surface hides it, and
	// "absent from the list" would stop meaning one thing. Here a lapsed proposal
	// is visible for exactly as long as it exists.
	//
	// It bounds the table. A proposal nobody signs is never claimed, so nothing
	// else ever removes it.
	LapsedProposalRetention time.Duration

	// ProposalPurgeInterval is how often lapsed proposals past the retention are
	// removed. ZERO DISABLES THE PURGE, which the composition root logs loudly:
	// the list keeps growing in plain sight rather than the table growing
	// silently, but it does keep growing.
	ProposalPurgeInterval time.Duration
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
		Listen:          env.Or("DATAMASTER_LISTEN", ":8080"),
		LogLevel:        env.ParseLevelOr(os.Getenv("DATAMASTER_LOG_LEVEL"), slog.LevelInfo),
		OTLPEndpoint:    os.Getenv("DATAMASTER_OTLP_ENDPOINT"),
		DatabaseURL:     databaseURL,
		Tenant:          env.Or("DATAMASTER_TENANT", "__system__"),
		RefreshInterval: durationOr("DATAMASTER_REFRESH_INTERVAL", 5*time.Minute),
		AllowSim:        boolOr("DATAMASTER_ALLOW_SIM", false),

		AllowEphemeralMaster: boolOr("DATAMASTER_ALLOW_EPHEMERAL_MASTER", false),

		Source:             env.Or("DATAMASTER_SOURCE", "datamaster"),
		SPIFFESocket:       os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:            os.Getenv("DATAMASTER_NATS_URL"),
		OutboxInterval:     durationOr("DATAMASTER_OUTBOX_INTERVAL", 0),
		RequireDualControl: boolOr("DATAMASTER_REQUIRE_DUAL_CONTROL", false),
		DualControlTTL:     durationOr("DATAMASTER_DUAL_CONTROL_TTL", dualcontrol.DefaultTTL),
		// SEVEN DAYS, and the number is argued rather than round. A proposer who
		// proposed an override on Friday and returns on Monday must still be able
		// to see that it lapsed; anything shorter makes the answer depend on how
		// long they were away, which is the ambiguity this whole change removes.
		LapsedProposalRetention: durationOr("DATAMASTER_LAPSED_PROPOSAL_RETENTION", 7*24*time.Hour),
		// Hourly. The purge is a bounded DELETE on an indexed column and the
		// retention is measured in days, so nothing is gained by running it often.
		ProposalPurgeInterval: durationOr("DATAMASTER_PROPOSAL_PURGE_INTERVAL", time.Hour),

		RefFiles:   parseVendorMap(os.Getenv("DATAMASTER_REF_FILES")),
		PriceFiles: parseVendorMap(os.Getenv("DATAMASTER_PRICE_FILES")),
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
