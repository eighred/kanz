package config

import (
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strings"

	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/pkg/secret"
)

// Config is the regulatory filing service runtime configuration, sourced from
// the environment. The service assembles the delivered FRTB / Form PF / AIFMD /
// TCFD / SFDR filings from request-supplied inputs, signs them, and serves them
// point-in-time + completeness-gated — the reporting layer over the PARITY-06
// analytics (WIRE-01e).
type Config struct {
	// Listen is the FILING API port, and it is not :8083 any more (#765).
	//
	// allow-observability-scrape selects every pod in kanz-services and admits a
	// list of ports with 8083 among them — audit's metrics and, until this split,
	// regulatory's whole surface. So every pod in the kanz-observability
	// namespace could POST /v1/filings/*, and a filing is not a read: it is
	// signed and, on the default chain signer, appends a link to the AUDIT-01
	// hash chain. The monitoring plane could write to the compliance record, and
	// this service reads no principal header, so there was no authentication step
	// for it to fail.
	//
	// :8103 sits beside accounting's :8101 (#447) and audit's :8102 (#627) —
	// the two services that made this same move before it, audit from this very
	// port.
	Listen string
	// MetricsListen serves /metrics and NOTHING ELSE. It stays on :8083 for the
	// reason accounting's entry gives for keeping its own: 8083 is already
	// admitted for audit's metrics, so moving regulatory's there costs nothing,
	// while admitting a NEW port to the scrape rule would widen it for no gain.
	// It is the API that had to leave.
	MetricsListen string
	LogLevel      slog.Level

	// Signer selects the filing signature backend: "chain" (default) links every
	// filing into the AUDIT-01 hash chain (signer.ChainSigner) so a signature is
	// a verifiable chain position; "hash" is a bare SHA-256 content digest
	// (tamper-evidence without ordering). Both satisfy the delivered Signer seam.
	Signer string

	// DatabaseURL is the Postgres DSN for the durable audit hash-chain link store
	// (REG-02). When set (and Signer is "chain"), each filing's chain link is
	// persisted and the chain head is recovered on startup, so the tamper-evident
	// chain survives a restart. Empty ⇒ openLinkStore REFUSES TO START unless
	// AllowEphemeralChain says the deployment accepts an in-process chain (#261).
	// Signer "hash" needs no DSN at all and never reaches that check.
	DatabaseURL string
	// AllowEphemeralChain (REGULATORY_ALLOW_EPHEMERAL_CHAIN=true) opts in to the
	// in-memory link store when the chain signer is selected and no DSN is set.
	// It exists for a laptop and a test rig; no shipped manifest sets it, and a
	// deployment that does MUST run one replica — see openLinkStore.
	AllowEphemeralChain bool

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string

	// Tenant is the tenant THIS DEPLOYMENT serves, and it is never read from a
	// caller. It scopes the reference-data cache only — datamaster is
	// tenant-per-instance, so a cache built for the wrong tenant looks exactly
	// like an empty security master rather than failing loudly.
	//
	// Nothing else here is tenant-scoped: this service reads no principal header,
	// and the ESG screen evaluates the book the CALLER supplies rather than one
	// it fetches, so there is no cross-tenant read for this value to guard.
	Tenant string

	// RefData wires the instrument classifier the ESG screen resolves SECTOR and
	// ISSUER through (#751).
	//
	// Unset ⇒ no classifier, and those dimensions are then UNRESOLVABLE — which
	// the shared COMP-01 engine REFUSES rather than passes (#640). So an
	// exclusion policy naming a sector comes back as "cannot be verified" with
	// the cause named, never as a clean PASS over a book nobody could classify.
	// That is the honest posture for a deployment with no reference data, and it
	// is why the route is still worth mounting before this is wired.
	RefData refdata.Config
}

// Load reads the configuration from the environment with production-safe defaults.
func Load() (Config, error) {
	// Resolved before the literal so a declared-but-unreadable mount stops Load
	// HERE. The local helper this replaces answered an unreadable file with the
	// plaintext env and then with "", and "" is this service's in-memory-chain
	// default: a broken CSI mount would have quietly demoted the durable audit
	// hash chain (REG-02) to one lost on the next restart, which is precisely the
	// tamper-evidence the filings are signed to carry. See pkg/secret.
	databaseURL, err := secret.Read("REGULATORY_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	refData, rerr := refdata.LoadConfig("REGULATORY")
	if rerr != nil {
		return Config{}, rerr
	}

	cfg := Config{
		Listen:        env.Or("REGULATORY_LISTEN", ":8103"),
		MetricsListen: env.Or("REGULATORY_METRICS_LISTEN", ":8083"),
		LogLevel:      env.ParseLevelOr(os.Getenv("REGULATORY_LOG_LEVEL"), slog.LevelInfo),
		Signer:        strings.ToLower(env.Or("REGULATORY_SIGNER", "chain")),
		DatabaseURL:   databaseURL,
		OTLPEndpoint:  os.Getenv("REGULATORY_OTLP_ENDPOINT"),
		Tenant:        env.Or("REGULATORY_TENANT", "__system__"),
		RefData:       refData,

		AllowEphemeralChain: os.Getenv("REGULATORY_ALLOW_EPHEMERAL_CHAIN") == "true",
	}
	// SHARING THE PORT PUTS THE FILING ROUTES BACK WHERE THEY WERE. A deployment
	// that sets both to the same value re-creates #765 exactly — the /v1 surface
	// on the port allow-observability-scrape admits namespace-wide — so it is
	// refused at startup rather than logged. Same guard mcp's config carries.
	if cfg.Listen == cfg.MetricsListen {
		return Config{}, fmt.Errorf("regulatory: REGULATORY_LISTEN and REGULATORY_METRICS_LISTEN "+
			"must differ, and both are %q — sharing a port puts the filing routes on the one "+
			"allow-observability-scrape admits, and a filing appends to the AUDIT-01 hash chain "+
			"(#765)", cfg.Listen)
	}
	return cfg, nil
}
