package config

import (
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lineage/internal/graph"
)

// Config is the lineage service runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret + ConfigMap mounts. Same
// shape as the audit service (LIN-01 is its lineage-graph sibling).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine the harvester consumes. Empty ⇒ HTTP-only (serve
	// the lineage/catalog API over whatever graph already exists — e.g. a
	// read-only replica).
	NATSURL string
	// Source is the consumer identity.
	Source string
	// ConsumerGroup is the durable consumer name (one group ⇒ harvested once).
	ConsumerGroup string
	// Subjects to harvest. Default ">" — the lineage graph is comprehensive by
	// design, like the audit log.
	Subjects []string
	// EventIndexMax is the hard ceiling on indexed event ids (#244). A ">"
	// subscription means this is the one structure that grows with the estate's
	// whole event rate rather than with its schema count, so it is the one that
	// has to be capped. There is no value meaning "unbounded": 0 or a negative is
	// a config error and refuses to start.
	EventIndexMax int

	// PolicyFile is the AUTH-01b policy bundle governing PII lineage access.
	// Empty ⇒ a deny-all authorizer (no role can read PII until a bundle is
	// mounted) — deny-by-default at the config layer too.
	PolicyFile string
	// GovernanceFile is the LIN-01c PII classification config. Empty ⇒ nothing is
	// classified PII (everything public) — a deployment must mount it to govern.
	GovernanceFile string

	// OpenLineageURL is the OpenLineage backend (Marquez/DataHub) the emitter
	// POSTs to. Empty ⇒ the LogEmitter (events to stdout JSON).
	OpenLineageURL string

	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string

	// Tenant is the producer's fallback tenant_id (MT-01b) for events this
	// service RAISES rather than derives — today, the AUTH-01d authorization
	// decisions published to platform.authz.decision.
	//
	// IT CANNOT BE EMPTY, and that is not a style preference. bus.Validate
	// requires tenant_id on the live path, and the producer resolves it as
	// explicit-field > ctx > this. A decision is raised by an HTTP request to the
	// read API, so there is no inbound delivery to supply a ctx tenant — which is
	// how services/compliance publishes decisions without a fallback at all (its
	// come off a consumer). Empty here means every decision publish is rejected
	// as an invalid envelope.
	//
	// Defaults to bus.SystemTenant because the lineage graph spans the estate
	// rather than one customer — the second, legitimate meaning documented in
	// pkg/bus/validate.go, the one internal/topic maps to the un-prefixed archive
	// topics. A PER-CUSTOMER lineage deployment must set LINEAGE_TENANT, and that
	// is a deployment review: leaving a per-customer service on __system__
	// attributes one customer's access decisions to the platform, and nothing
	// downstream can tell.
	Tenant string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
}

// DefaultSubjects harvests everything — lineage completeness over economy.
var DefaultSubjects = []string{">"}

func Load() (Config, error) {
	subjects := env.SplitList(os.Getenv("LINEAGE_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}
	indexMax := graph.DefaultEventIndexCapacity
	if v := os.Getenv("LINEAGE_EVENT_INDEX_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("config: LINEAGE_EVENT_INDEX_MAX=%q must be a positive "+
				"integer (there is no unbounded setting — see #244)", v)
		}
		indexMax = n
	}
	return Config{
		Listen:         env.Or("LINEAGE_LISTEN", ":8086"),
		LogLevel:       env.ParseLevelOr(env.Or("LINEAGE_LOG_LEVEL", "info"), slog.LevelInfo),
		NATSURL:        os.Getenv("LINEAGE_NATS_URL"),
		Source:         env.Or("LINEAGE_SOURCE", "lineage"),
		ConsumerGroup:  env.Or("LINEAGE_CONSUMER_GROUP", "lineage"),
		Subjects:       subjects,
		EventIndexMax:  indexMax,
		PolicyFile:     os.Getenv("LINEAGE_POLICY_FILE"),
		GovernanceFile: os.Getenv("LINEAGE_GOVERNANCE_FILE"),
		OpenLineageURL: strings.TrimRight(os.Getenv("LINEAGE_OPENLINEAGE_URL"), "/"),
		OTLPEndpoint:   os.Getenv("LINEAGE_OTLP_ENDPOINT"),
		Tenant:         env.Or("LINEAGE_TENANT", bus.SystemTenant),
		SPIFFESocket:   os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}, nil
}
