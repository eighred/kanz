package config

import (
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"

	"github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/pkg/secret"
)

// Config is the alternatives (private-markets) service runtime configuration,
// sourced from the environment. The service folds commitment lifecycle events
// (capital calls, distributions, NAV marks) into an event-sourced fund position
// and serves position summaries + private-asset metrics (IRR/TVPI/DPI/RVPI). The
// journal store is Postgres behind the fund.Store seam, wired at the composition
// root (the PERS-01 stance) along with the bus consumer that feeds it. There is
// no in-memory DEFAULT any more: with no DSN the service refuses to start unless
// AllowEphemeralJournal opts in (#261).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string

	// DatabaseURL selects the durable commitment journal. Empty ⇒ openStore
	// REFUSES TO START unless AllowEphemeralJournal says the deployment accepts
	// losing every capital call and distribution on restart (#261).
	DatabaseURL string
	// AllowEphemeralJournal (ALTERNATIVES_ALLOW_EPHEMERAL_JOURNAL=true) opts in to
	// the in-memory journal when no DSN is set. It exists for a laptop and a test
	// rig; no shipped manifest sets it.
	AllowEphemeralJournal bool
	// Tenant is carried as the `app.tenant_id` GUC on every DB connection so
	// Postgres RLS scopes the journal (MT-01d). Defaults to __system__, the
	// risk-engine convention.
	Tenant string

	// NATSURL is the live spine the commitment-lifecycle consumer subscribes to
	// (ALT-01b). Empty ⇒ no consumer (the default; the service serves the read
	// endpoints on whatever journal openStore selected, without a broker).
	NATSURL string
	// Source is the consumer identity (logging / durable consumer name).
	Source string
	// ConsumerGroup is the durable consumer name the lifecycle subjects
	// subscribe under.
	ConsumerGroup string
	// Subjects are the commitment-lifecycle FACT subjects folded into the fund
	// journal. Defaults to alternatives.AllSubjects() — the full lifecycle set
	// (committed/called/distributed/marked). Comma-separated.
	//
	// Dropping a subject from ALTERNATIVES_SUBJECTS silently stops folding that
	// event type: the position for any commitment still emitting it will simply
	// stop advancing, with no error anywhere in this service — the gap only
	// surfaces downstream as a wrong IRR/TVPI or a position stuck at an old NAV
	// mark. Only narrow this list deliberately.
	Subjects []string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as accounting/oms do,
	// so one manifest env name serves every service.
	SPIFFESocket string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	// Resolved before the literal so a declared-but-unreadable ALTERNATIVES_DATABASE_URL_FILE
	// stops Load HERE rather than falling through to the plaintext env and then to ""
	// the way the local helper this replaces did. Empty is a real setting in this
	// service — it selects the in-memory fund journal — so a failed CSI mount would
	// have come up healthy while every capital call and distribution it folded was
	// discarded on the next restart. See pkg/secret.
	databaseURL, err := secret.Read("ALTERNATIVES_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	subjects := env.SplitList(os.Getenv("ALTERNATIVES_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = alternatives.AllSubjects()
	}
	return Config{
		Listen:       env.Or("ALTERNATIVES_LISTEN", ":8080"),
		LogLevel:     env.ParseLevelOr(os.Getenv("ALTERNATIVES_LOG_LEVEL"), slog.LevelInfo),
		OTLPEndpoint: os.Getenv("ALTERNATIVES_OTLP_ENDPOINT"),
		DatabaseURL:  databaseURL,
		Tenant:       env.Or("ALTERNATIVES_TENANT", "__system__"),

		AllowEphemeralJournal: os.Getenv("ALTERNATIVES_ALLOW_EPHEMERAL_JOURNAL") == "true",
		NATSURL:               os.Getenv("ALTERNATIVES_NATS_URL"),
		Source:                env.Or("ALTERNATIVES_SOURCE", "alternatives"),
		ConsumerGroup:         env.Or("ALTERNATIVES_CONSUMER_GROUP", "alternatives"),
		Subjects:              subjects,
		SPIFFESocket:          os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}, nil
}
