package config

import (
	"log/slog"
	"os"
	"time"

	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/audit/internal/verify"
)

// Config is the audit service runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
// Mirrors the market-data / risk-engine config shape.
type Config struct {
	// Listen serves /metrics and the probes, and NOTHING that reads a tenant
	// header. It is the port allow-observability-scrape admits, which is exactly
	// why the read API is not on it (#627).
	Listen string
	// APIListen serves the /v1 compliance reads — the tenant's audit history and
	// the estate-wide chain attestation. Every route on it takes its tenant from
	// X-Kanz-Principal-Tenant, so the api-gateway must be its only reachable
	// caller; sharing the scraped port meant a pod in kanz-observability could
	// name any tenant and be served it, against a table deliberately not RLS'd.
	APIListen string
	LogLevel  slog.Level

	// NATSURL is the live spine the projection consumes. Empty ⇒ HTTP-only
	// (the query/report API serves whatever is already in the store; no new
	// projection). Useful for a read-only reporting replica.
	NATSURL string
	// Source is the consumer identity.
	Source string
	// ConsumerGroup is the durable consumer name. A single group means the
	// projection is processed once (the audit log must not double-count).
	ConsumerGroup string
	// Subjects are the subjects to materialize — ONE PER PROVISIONED STREAM, not
	// a single ">". The audit log is comprehensive by design (every
	// decision/command/outcome/quality event); narrow per deployment via
	// AUDIT_SUBJECTS only with a clear reason.
	//
	// IT WAS ">" AND THAT MATERIALIZED ONE STREAM (#698). A subscription binds to
	// exactly one stream, and the JetStream client resolved ">" by taking the
	// first of the sixteen it matched, in the server's order, with no error. The
	// other fifteen were never consumed — while this service reported Ready and
	// its consume counter climbed from the one stream that did deliver.
	Subjects []string

	// DatabaseURL is the Postgres DSN for the durable, WORM audit log. Empty ⇒
	// the in-memory store (local/dev; the log is lost on restart, so NOT for
	// production — an audit log that doesn't survive a restart isn't one), and
	// the composition root REFUSES to start on it unless AllowEphemeralLog says
	// out loud that the deployment accepts that.
	DatabaseURL string

	// AllowEphemeralLog is the EXPLICIT admission that the tamper-evidence log is
	// held in RAM and dies with the process — AUDIT_ALLOW_EPHEMERAL_LOG=true.
	//
	// It exists because an empty DSN and a configured one produced the SAME clean
	// start, and this is the compliance service: the deployment that FORGOT the
	// DSN is indistinguishable from the dev box that meant it, right up to the
	// restart that discards the whole hash chain and the regulator's request that
	// cannot be answered. Unlike the OMS's or a venue adapter's in-memory
	// fallback, there is no replica count at which this one is correct in
	// production — so it is opt-in, not warn-and-carry-on.
	AllowEphemeralLog bool

	// VerifyRoles are the roles permitted to call GET /v1/audit/verify
	// (AUDIT_VERIFY_ROLES, comma-separated) — the #118 ruling.
	//
	// WHY THIS ENDPOINT AND NO OTHER. Every other read here is tenant-scoped by
	// auth.RequireCallerTenant. /v1/audit/verify deliberately is NOT, and cannot
	// be: the hash chain is ONE sequence across every tenant, so verifying a
	// per-tenant subset proves nothing about it. That makes the attestation the
	// single cross-tenant answer this service gives — and its record count tells
	// any authenticated caller how much OTHER tenants' activity the platform is
	// carrying. A tenant may see its own vault; the estate-wide count belongs to
	// operators and monitoring.
	//
	// It restricts WHO MAY ASK, not what is returned. Scoping the attestation
	// itself would destroy the property it exists to prove.
	VerifyRoles []string

	// AllowUnrestrictedVerify is the EXPLICIT admission that /v1/audit/verify is
	// open to any authenticated principal — AUDIT_ALLOW_UNRESTRICTED_VERIFY=true.
	//
	// It exists for the same reason AllowEphemeralLog does, and the reasoning is
	// worth repeating rather than cross-referencing: an unset AUDIT_VERIFY_ROLES
	// and a deliberately-open deployment would otherwise produce the SAME clean
	// start. The failure mode is the worse direction here — a deployment that
	// FORGOT to grant the capability would silently serve estate-wide counts to
	// every tenant, and nothing would look wrong.
	//
	// So unset is a REFUSAL TO START, not a default. The escape hatch is loud,
	// named, and greppable.
	//
	// THE OPPOSITE FAILURE IS ALSO REAL AND IS WHY THE HATCH EXISTS AT ALL:
	// over-restricting a tamper check means nobody verifies. A deployment that
	// cannot yet issue roles should say so out loud and keep verification
	// working, rather than have the endpoint deny everyone with no way to tell
	// that from a chain that is fine.
	AllowUnrestrictedVerify bool

	// VerifyInterval is how often the hash chain is verified in-process (#665).
	//
	// AUDIT_VERIFY_INTERVAL overrides it; an unset or non-positive value keeps
	// verify.DefaultInterval. THERE IS NO "OFF": lengthening it past the staleness
	// threshold makes the staleness alert fire, which is the correct outcome for a
	// chain nobody is verifying — it should be visible as such rather than
	// configurable into silence.
	VerifyInterval time.Duration

	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
}

// DefaultSubjects materializes everything — audit completeness over economy —
// by naming EVERY provisioned stream's subject space, one entry per stream.
//
// # Why an enumeration rather than ">"
//
// ">" reads as "everything" and delivered one sixteenth of it (#698):
// pkg/bus.Subscribe binds one stream per subscription, and the client silently
// returned the first match. pkg/bus now REFUSES a multi-stream subject, so this
// list is not merely better — ">" no longer starts.
//
// # Why the list is safe to enumerate
//
// A list duplicating infra/nats/bootstrap-job.yaml is exactly the kind that goes
// stale the day somebody provisions a seventeenth stream, and a stale list here
// is a silent audit gap again. test/arch's TestAuditMaterializesEveryProvisionedStream
// compares the two in BOTH directions on every build: a stream nothing here
// covers fails, and an entry matching no stream fails. The enumeration is
// checked, not remembered.
//
// # The entries are the streams' OWN subject sets
//
// Not the domain prefixes. `risk.>` would match both RISK and POSITION, which is
// the multi-stream subject this whole change exists to refuse; the split between
// them (POSITION is compacted, max-msgs-per-subject=1) is why they are separate
// streams at all.
var DefaultSubjects = []string{
	"market.>",             // MARKET
	"risk.portfolio.>",     // RISK
	"risk.exposure.>",      // RISK
	"risk.signal.>",        // RISK
	"risk.command.>",       // RISK
	"execution.>",          // EXECUTION
	"strategy.>",           // EXECUTION
	"order.>",              // EXECUTION
	"optimization.>",       // EXECUTION
	"tenant.*.order.>",     // TENANT_ORDER — the MT-02 command bridge
	"inference.>",          // INFERENCE
	"platform.>",           // PLATFORM
	"data.>",               // DATA
	"observability.>",      // OBSERVABILITY
	"accounting.>",         // ACCOUNTING
	"compliance.breach.>",  // COMPLIANCE
	"settlement.>",         // SETTLEMENT
	"alternatives.>",       // ALTERNATIVES
	"compliance.mandate.>", // MANDATE
	"wealth.>",             // WEALTH
	"risk.position.>",      // POSITION
	"dlq.>",                // DLQ — a dropped event is an audit fact of its own
	// TENANT_FACT — every provisioned tenant's FACTs, arriving under a
	// tfact.<tenant>. prefix through the #668 return path. THIS ENTRY IS THE
	// TENANT'S COMPLIANCE TRAIL: NATS accounts are isolated by construction, so
	// without it a tenant's orders, fills and ledger postings are recorded
	// nowhere at all, while every service stays Ready — "this tenant produced no
	// events" and "this tenant's events cannot reach me" being the same
	// observable state.
	//
	// The projector needs no change: it takes TenantID off the ENVELOPE, which
	// crosses the bridge unmodified, so a FACT arriving as
	// tfact.acme.order.order.filled is folded under tenant acme by the same code
	// path as a platform event. The prefix is transport, not identity.
	"tfact.>",
}

func Load() (Config, error) {
	subjects := env.SplitList(os.Getenv("AUDIT_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}
	// Resolved before the literal so a declared-but-unreadable mount stops Load
	// here. An empty DSN is a LEGAL value for this service — it selects the
	// in-memory store — so a broken Vault mount used to produce an audit
	// service that started clean, served queries, and lost the entire tamper-
	// evidence log on restart. Nothing downstream would have reported it.
	verifyInterval, err := env.Duration("AUDIT_VERIFY_INTERVAL", verify.DefaultInterval)
	if err != nil {
		return Config{}, err
	}
	databaseURL, err := secret.Read("AUDIT_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	return Config{
		VerifyInterval:    verifyInterval,
		Listen:            env.Or("AUDIT_LISTEN", ":8083"),
		APIListen:         env.Or("AUDIT_API_LISTEN", ":8102"),
		LogLevel:          env.ParseLevelOr(env.Or("AUDIT_LOG_LEVEL", "info"), slog.LevelInfo),
		NATSURL:           os.Getenv("AUDIT_NATS_URL"),
		Source:            env.Or("AUDIT_SOURCE", "audit"),
		ConsumerGroup:     env.Or("AUDIT_CONSUMER_GROUP", "audit"),
		Subjects:          subjects,
		DatabaseURL:       databaseURL,
		AllowEphemeralLog: os.Getenv("AUDIT_ALLOW_EPHEMERAL_LOG") == "true",

		VerifyRoles:             env.SplitList(os.Getenv("AUDIT_VERIFY_ROLES")),
		AllowUnrestrictedVerify: os.Getenv("AUDIT_ALLOW_UNRESTRICTED_VERIFY") == "true",
		OTLPEndpoint:            os.Getenv("AUDIT_OTLP_ENDPOINT"),
		SPIFFESocket:            os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}, nil
}
