package config

import (
	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/internal/fillfact"
	"log/slog"
	"os"
	"time"

	"github.com/eighred/kanz/pkg/secret"
)

// Config is the accounting (IBOR) service runtime configuration, sourced from the
// environment. The service folds OMS-01 fills and cash/corporate-action events
// into the book-of-record and serves point-in-time NAV and custodian
// reconciliation. The journal store defaults to in-memory (a durable backend
// plugs in behind ledger.Store at the composition root, the PERS-01 stance); the
// bus consumer that feeds the journal is wired there too, so the default boot
// serves the read/reconcile endpoints without a broker.
type Config struct {
	// Listen is the API port — the tenant-scoped routes that MOVE MONEY
	// (cash-movements, nav, reconcile). It is deliberately NOT the scraped port
	// (#447): allow-observability-scrape must admit whatever port serves
	// /metrics, so an API sharing it is reachable from kanz-observability with a
	// self-chosen principal header, whatever else the NetworkPolicy says.
	Listen string
	// MetricsListen serves /metrics and nothing else. It is the port the scrape
	// rule admits, which is exactly why the API is not on it.
	MetricsListen string
	LogLevel      slog.Level
	BaseCurrency  string

	// NATSURL is the live spine the fill-folding consumer subscribes to
	// (WIRE-01b). Empty ⇒ no consumer (the default; the service serves the
	// read/reconcile endpoints on an in-memory journal without a broker).
	NATSURL string
	// Source is the consumer identity (logging / durable consumer name).
	Source string
	// ConsumerGroup is the durable consumer name the fill subjects subscribe under.
	ConsumerGroup string

	// CustodyStatementSubject is where custodian statements arrive (#962). Empty
	// ⇒ no statement consumer.
	CustodyStatementSubject string
	// CustodyPairs are the "portfolio:custodian" pairs the scheduled
	// reconciliation covers.
	//
	// AN EMPTY LIST DISABLES THE CONTROL ENTIRELY, and the composition root says
	// so at ERROR rather than starting a scheduler with nothing to do. The
	// pre-#962 estate reconciled only when a human posted a statement by hand; a
	// scheduler configured with no pairs reproduces exactly that, with the added
	// cost of looking configured.
	CustodyPairs []string
	// CustodyAccounts is the RAW ACCOUNTING_CUSTODY_ACCOUNTS declaration, kept
	// unsplit for custody.ParseCustodyAccounts to interpret whole:
	//
	//	ACCOUNTING_CUSTODY_ACCOUNTS=PF1:CUST-A:okx-sub-1,okx-sub-2 PF1:CUST-B:bin-main
	//
	// IT IS NOT env.SplitList, AND THAT IS THE FIX RATHER THAN AN OVERSIGHT
	// (#1029). SplitList splits on comma, which is also the separator between one
	// custodian's accounts — so a custodian holding two accounts had its entry cut
	// in half and the pod exited 2 on the bare fragment, making #1006's scoping
	// reachable only for single-account custodians. Both levels of the grammar now
	// have ONE owner; splitting here and interpreting there is how they disagreed.
	//
	// IT IS REQUIRED EXACTLY WHEN A PORTFOLIO HAS MORE THAN ONE CUSTODIAN, and
	// custody.NewBookScope refuses the start without it (#1006). The book side of
	// the comparison used to load the whole portfolio regardless of the custodian
	// on the subject, so each custodian's run reported every position held at the
	// other as MISSING_AT_CUSTODIAN. A single-custodian portfolio needs no entry:
	// the whole book against its one custodian is correct.
	CustodyAccounts string
	// CustodyInterval is how often each pair is reconciled.
	CustodyInterval time.Duration
	// CustodyLagDays is how many days back from the run instant the reconciled
	// business date sits. A custodian states holdings as of a CLOSE and transmits
	// afterwards, so 0 would conclude NO_STATEMENT permanently and train readers
	// to ignore the alert.
	CustodyLagDays int
	// CustodyTolerance is the absolute difference at or below which the book and
	// the custodian are treated as agreeing. Empty ⇒ exact match required.
	CustodyTolerance string
	// FillSubjects are the order.v1 fill FACT subjects folded into the journal.
	FillSubjects []string
	// CashSubjects are the accounting.v1 cash-movement FACT subjects folded into
	// the journal (WIRE-01f: subscription/redemption/fee). Default the
	// accounting.cash wildcard. Comma-separated.
	CashSubjects []string

	// DatabaseURL is the Postgres DSN for the durable IBOR journal (PARITY-02a).
	// Empty ⇒ openStore REFUSES TO START unless AllowEphemeralLedger says the
	// deployment accepts a book of record held in RAM (#261).
	DatabaseURL string
	// AllowEphemeralLedger (ACCOUNTING_ALLOW_EPHEMERAL_LEDGER=true) opts in to the
	// in-memory journal when no DSN is set. It exists for a laptop and a test rig,
	// and openStore refuses to boot without it — see the WHY THIS ONE REFUSES
	// paragraph on openStore. Not set by any shipped manifest, deliberately.
	AllowEphemeralLedger bool
	// Tenant is carried as the `app.tenant_id` GUC on every DB connection.
	//
	// It is REQUIRED, not decorative. 0001_ledger.sql runs FORCE ROW LEVEL
	// SECURITY with a policy on current_setting('app.tenant_id'), and Append
	// inserts VALUES (current_setting('app.tenant_id'), ...). Without the GUC set,
	// against the non-superuser role production requires, every write ERRORS and
	// every read returns ZERO ROWS — the ledger silently holds nothing.
	//
	// The Postgres tests set this GUC in their own pool (AfterConnect) and pass.
	// main.go never did. That is why this was invisible.
	Tenant string

	// SnapshotInterval is how often the ledger checkpoint job runs (#229).
	//
	// It is what keeps NAV bounded: without a checkpoint, materializing a book
	// reads the portfolio's ENTIRE lifetime journal on every request, so the
	// service degrades with AGE rather than with load. The job only runs with a
	// durable store — the in-memory journal dies with the pod, so checkpointing
	// it buys nothing.
	//
	// ACCOUNTING_SNAPSHOT_INTERVAL, a Go duration. A zero or negative value
	// DISABLES the job, which Load refuses to infer from a malformed setting: a
	// typo must not silently reinstate the unbounded read.
	SnapshotInterval time.Duration
	// SnapshotBatch caps the portfolios one checkpoint pass will fold
	// (ACCOUNTING_SNAPSHOT_BATCH). Each is a full journal read, so an unbounded
	// pass against a large estate would hold a pool connection for as long as it
	// took — the shape of problem the job exists to remove.
	SnapshotBatch int

	// Live FX for multi-currency NAV (WIRE-01d): a latest-rate cache folded from
	// the market.v1 FX spine, so the NAV endpoint values a multi-currency book
	// with no `fx` in the request. Off unless FXPairs is configured AND a broker
	// is set. A per-request `fx` still overrides the live rates (client-supplied).
	//
	// FXPairs is the raw FX-pair reference spec (ACCOUNTING_FX_PAIRS):
	// comma-separated `<instrument_id>:<foreign_currency>` entries the cache
	// records rates for, parsed by fxfeed.ParsePairs.
	FXPairs string
	// FXSubjects are the market.v1 FX subjects the cache subscribes
	// (ACCOUNTING_FX_SUBJECTS); default the MARKET FX wildcard. Comma-separated.
	FXSubjects []string
	// InstrumentCurrency is the raw instrument→reference-currency spec
	// (ACCOUNTING_INSTRUMENT_CURRENCY): comma-separated `<instrument>:<currency>`
	// entries — the security-master join the multi-currency valuation needs. A
	// per-request map overrides it. The datamaster-backed source plugs in here.
	InstrumentCurrency string

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
}

// DefaultFXSubjects is the FX-cache subscription when none is configured.
var DefaultFXSubjects = []string{"market.fx.>"}

// DefaultFillSubjects are the order.v1 fill FACT subjects the consumer folds
// (mirrors the consume package's wire subjects — the same "mirror, don't import
// the OMS internal package" stance).
var DefaultFillSubjects = []string{fillfact.SubjectFilled, fillfact.SubjectPartiallyFilled}

// DefaultCashSubjects are the accounting.v1 cash-movement FACT subjects the
// consumer folds (WIRE-01f) — the wildcard over the cashmove.Publisher subjects.
var DefaultCashSubjects = []string{"accounting.cash.>"}

// DefaultCustodyStatementSubject is where a custodian feed adapter announces a
// statement (#962). The per-custodian adapters that translate SWIFT MT535/MT940
// or a prime broker's SFTP drop into this canonical shape are deferred to
// #105/#106, which have real feeds to develop against; the subject and the
// control behind it exist now so an adapter has somewhere to publish.
const DefaultCustodyStatementSubject = "accounting.custody.statement"

// DefaultCustodyInterval is how often each (portfolio, custodian) pair is
// reconciled. Daily is the custodian's own cadence; more often simply
// re-reconciles the same close.
const DefaultCustodyInterval = 24 * time.Hour

// DefaultCustodyLagDays is the business-date lag. ONE, not zero: a custodian
// states holdings as of a CLOSE and transmits afterwards, so reconciling "today"
// during today compares a still-moving book against a statement that cannot exist
// yet — every run would conclude NO_STATEMENT and the alert would never clear.
const DefaultCustodyLagDays = 1

// DefaultSnapshotInterval is how often the ledger checkpoint job runs when
// ACCOUNTING_SNAPSHOT_INTERVAL is unset. Five minutes bounds the worst-case
// tail a NAV request folds to five minutes of fills for one portfolio, at a
// cost of one full journal fold per stale portfolio per five minutes on a
// background goroutine. It is ON by default deliberately: the read path is
// unbounded without it, and an operator who never heard of this setting should
// get the bounded behaviour.
const DefaultSnapshotInterval = 5 * time.Minute

// DefaultSnapshotBatch is the most portfolios one checkpoint pass folds.
const DefaultSnapshotBatch = 64

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	// Resolved before the literal so an ACCOUNTING_DATABASE_URL_FILE that will not
	// read stops Load HERE. The local helper this replaces answered an unreadable
	// mount with the plaintext env and then with "", and an empty DSN is not inert
	// in this service: it SELECTS the in-memory journal. A broken Vault mount would
	// therefore have booked the fund's book-of-record into a map that dies with the
	// pod, and reported a clean start doing it. See pkg/secret.
	databaseURL, err := secret.Read("ACCOUNTING_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	subjects := env.SplitList(os.Getenv("ACCOUNTING_FILL_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultFillSubjects
	}
	fxSubjects := env.SplitList(os.Getenv("ACCOUNTING_FX_SUBJECTS"))
	if len(fxSubjects) == 0 {
		fxSubjects = DefaultFXSubjects
	}
	cashSubjects := env.SplitList(os.Getenv("ACCOUNTING_CASH_SUBJECTS"))
	if len(cashSubjects) == 0 {
		cashSubjects = DefaultCashSubjects
	}

	snapshotInterval, err := env.Duration("ACCOUNTING_SNAPSHOT_INTERVAL", DefaultSnapshotInterval)
	if err != nil {
		return Config{}, err
	}
	snapshotBatch, err := env.Int("ACCOUNTING_SNAPSHOT_BATCH", DefaultSnapshotBatch)
	if err != nil {
		return Config{}, err
	}

	custodyInterval, err := env.Duration("ACCOUNTING_CUSTODY_INTERVAL", DefaultCustodyInterval)
	if err != nil {
		return Config{}, err
	}
	custodyLagDays, err := env.Int("ACCOUNTING_CUSTODY_LAG_DAYS", DefaultCustodyLagDays)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen:        env.Or("ACCOUNTING_LISTEN", ":8101"),
		MetricsListen: env.Or("ACCOUNTING_METRICS_LISTEN", ":8080"),
		LogLevel:      env.ParseLevelOr(os.Getenv("ACCOUNTING_LOG_LEVEL"), slog.LevelInfo),
		BaseCurrency:  env.Or("ACCOUNTING_BASE_CURRENCY", "USD"),
		NATSURL:       os.Getenv("ACCOUNTING_NATS_URL"),
		Source:        env.Or("ACCOUNTING_SOURCE", "accounting"),
		ConsumerGroup: env.Or("ACCOUNTING_CONSUMER_GROUP", "accounting"),
		FillSubjects:  subjects,
		CashSubjects:  cashSubjects,
		DatabaseURL:   databaseURL,
		Tenant:        env.Or("ACCOUNTING_TENANT", "__system__"),

		AllowEphemeralLedger: os.Getenv("ACCOUNTING_ALLOW_EPHEMERAL_LEDGER") == "true",

		SnapshotInterval: snapshotInterval,
		SnapshotBatch:    snapshotBatch,

		CustodyStatementSubject: env.Or("ACCOUNTING_CUSTODY_STATEMENT_SUBJECT", DefaultCustodyStatementSubject),
		CustodyPairs:            env.SplitList(os.Getenv("ACCOUNTING_CUSTODY_PAIRS")),
		CustodyAccounts:         os.Getenv("ACCOUNTING_CUSTODY_ACCOUNTS"),
		CustodyInterval:         custodyInterval,
		CustodyLagDays:          custodyLagDays,
		CustodyTolerance:        os.Getenv("ACCOUNTING_CUSTODY_TOLERANCE"),

		OTLPEndpoint: os.Getenv("ACCOUNTING_OTLP_ENDPOINT"),
		SPIFFESocket: os.Getenv("SPIFFE_ENDPOINT_SOCKET"),

		FXPairs:            os.Getenv("ACCOUNTING_FX_PAIRS"),
		FXSubjects:         fxSubjects,
		InstrumentCurrency: os.Getenv("ACCOUNTING_INSTRUMENT_CURRENCY"),
	}, nil
}
