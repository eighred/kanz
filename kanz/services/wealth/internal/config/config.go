package config

import (
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/pkg/secret"
)

// Config is the wealth (advisory) service runtime configuration, sourced from the
// environment. The service aggregates a household's accounts into a virtual
// portfolio and serves the household-level exposure view (WEALTH-01b). The
// household store is Postgres behind the book.Store seam, wired at the
// composition root (the PERS-01 stance) along with the bus consumer that feeds
// household/account/holding state. There is no in-memory DEFAULT any more: with
// no DSN the service refuses to start unless AllowEphemeralBook opts in (#261).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string

	// DatabaseURL selects the durable household book. Empty ⇒ openStore REFUSES TO
	// START unless AllowEphemeralBook says the deployment accepts losing every
	// household on restart (#261).
	DatabaseURL string
	// AllowEphemeralBook (WEALTH_ALLOW_EPHEMERAL_BOOK=true) opts in to the
	// in-memory book when no DSN is set. It exists for a laptop and a test rig;
	// no shipped manifest sets it.
	AllowEphemeralBook bool
	// Tenant is carried as the `app.tenant_id` GUC on every DB connection so
	// Postgres RLS scopes the book (MT-01d). Defaults to __system__, the
	// risk-engine convention.
	Tenant string

	// NATSURL is the live spine the household-valuation consumer subscribes to
	// (WEALTH-01b). Empty ⇒ no consumer (the default; the service serves the
	// read endpoints on whatever store openStore selected, without a broker).
	NATSURL string
	// Source is the consumer identity (logging / durable consumer name).
	Source string
	// ConsumerGroup is the durable consumer name the household subject
	// subscribes under.
	ConsumerGroup string
	// Subjects are the wealth.v1.HouseholdValued FACT subjects folded into the
	// book. Defaults to wealth.SubjectHouseholdAll — the compacted wildcard over
	// every household. Comma-separated.
	Subjects []string
	// ModelSubject is the wealth.v1.ModelPortfolio catalogue subject (WEALTH-01d).
	// It is a SEPARATE field from Subjects above, not another entry in it, because
	// the two need OPPOSITE delivery semantics and putting them in one list would
	// silently give the catalogue the wrong one:
	//
	//   · Subjects rides the DURABLE QUEUE GROUP. A valuation must be folded once
	//     across the deployment, and with replicas: 2 a queue group is what makes
	//     that true.
	//   · ModelSubject rides SubscribeBroadcastReady (DeliverLastPerSubject). The
	//     catalogue is per-process state, so EVERY replica must receive EVERY
	//     model. Under a queue group the two pods would hold DISJOINT catalogues
	//     and a household's drift would resolve or not depending on which pod the
	//     Service happened to pick — the exact split-brain openStore's doc
	//     describes for the ephemeral book.
	//
	// Empty ⇒ the catalogue never arms, every household reports
	// outcome="catalogue_unarmed", and no drift is evaluated at all. That is
	// visible on kanz_wealth_model_catalogue_armed rather than silent.
	ModelSubject string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as accounting/
	// alternatives do, so one manifest env name serves every service.
	SPIFFESocket string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	subjects := env.SplitList(os.Getenv("WEALTH_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = []string{wealth.SubjectHouseholdAll}
	}

	// The DSN carries database credentials and rides a CSI/Vault file mount
	// (SEC-01d), never a pod's env block. Resolved before the literal so an
	// unreadable mount stops Load HERE: nothing downstream would have caught it,
	// because "" is a legal value that selects the in-memory book — a failed mount
	// would have started a wealth service that serves households correctly until
	// the first restart drops every one of them. See pkg/secret.
	databaseURL, err := secret.Read("WEALTH_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen:       env.Or("WEALTH_LISTEN", ":8080"),
		LogLevel:     env.ParseLevelOr(os.Getenv("WEALTH_LOG_LEVEL"), slog.LevelInfo),
		OTLPEndpoint: os.Getenv("WEALTH_OTLP_ENDPOINT"),
		DatabaseURL:  databaseURL,
		Tenant:       env.Or("WEALTH_TENANT", "__system__"),

		AllowEphemeralBook: os.Getenv("WEALTH_ALLOW_EPHEMERAL_BOOK") == "true",
		NATSURL:            os.Getenv("WEALTH_NATS_URL"),
		Source:             env.Or("WEALTH_SOURCE", "wealth"),
		ConsumerGroup:      env.Or("WEALTH_CONSUMER_GROUP", "wealth"),
		Subjects:           subjects,
		ModelSubject:       env.Or("WEALTH_MODEL_SUBJECT", wealth.SubjectModelAll),
		SPIFFESocket:       os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}, nil
}
