package config

import (
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/refdata"
)

// Config is the compliance service runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	// Listen serves the probes and /metrics. IT IS THE SCRAPED PORT, which is why
	// the mandate-change API is not on it — see APIListen.
	Listen       string
	LogLevel     slog.Level
	Source       string
	OTLPEndpoint string
	// Tenant is the tenant THIS DEPLOYMENT serves. It is not read from a caller's
	// header: the only thing it addresses is the per-tenant datamaster instance
	// this service asks for instrument classification, and that instance refuses
	// any caller whose tenant is not its own. Defaults to __system__, the same
	// convention the OMS and the risk engine use.
	Tenant string
	// RefData wires the instrument classifier the post-trade monitor resolves
	// SECTOR, ISSUER and ASSET_CLASS mandate rules through (#640). Unwired, the
	// monitor REFUSES those rules rather than reporting a fund clean against an
	// exclusion it could not evaluate.
	RefData refdata.Config

	// APIListen serves the mandate-change routes (#562) and NOTHING ELSE.
	//
	// A SECOND LISTENER, AND THE SPLIT IS THE WHOLE SECURITY ARGUMENT.
	// allow-observability-scrape admits whatever port serves /metrics from the
	// entire kanz-observability namespace, and these routes decide who governs a
	// portfolio from the X-Kanz-Principal-* headers the api-gateway injects. On the
	// scraped port a pod in kanz-observability could send two self-chosen
	// principals and hold BOTH signatures on a mandate change — the exact failure
	// this control exists to prevent, arriving through the network layer.
	//
	// #232 named the fix as a code change per service; optimization (#409) and
	// accounting (#447) made it first, and test/arch/network_policy_coverage_test.go
	// fails if this port ever collapses back onto the scraped one.
	//
	// IT IS REACHED ONLY WHEN COMPLIANCE_NATS_URL IS SET, because approving a
	// mandate change publishes a FACT and there is nothing to publish to otherwise.
	// A probes-only deployment does not mount it, so the routes 404 rather than
	// answering 500 forever.
	APIListen string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
	// NATSURL is the spine. Empty ⇒ HTTP/probes only (no consumption), a
	// read-only/degraded posture.
	NATSURL string
	// ConsumerGroup is the durable group the monitor shares.
	ConsumerGroup string
}

// PositionSubject is what the monitor BINDS: every holding's latest state
// (EXEC-M20). It is the wildcard, not the flat subject, because a position now rides
// one subject per (tenant, portfolio, instrument) on a COMPACTED stream — which is what
// lets a booting monitor rebuild the whole book in one read instead of resuming past it.
//
// subject.PositionAll lives in internal/platform/subject, not internal/risk: the OMS
// publishes it and this service consumes it, and the RISK-02 boundary forbids outsiders
// importing risk internals.
const PositionSubject = subject.PositionAll

// MonitorSubjects are the FACTs the post-trade monitor consumes: position
// changes (re-evaluate) and mandate changes (keep the registry current so a
// tightened mandate is enforced against the existing book).
func (Config) MonitorSubjects() []string {
	return []string{PositionSubject}
}

// MandateSubject is the shared mandate ConfigChanged stream.
func (Config) MandateSubject() string { return comp.SubjectMandateChanged }

func Load() (Config, error) {
	refData, err := refdata.LoadConfig("COMPLIANCE")
	if err != nil {
		return Config{}, err
	}
	return Config{
		Tenant:        env.Or("COMPLIANCE_TENANT", "__system__"),
		RefData:       refData,
		Listen:        env.Or("COMPLIANCE_LISTEN", ":8091"),
		APIListen:     env.Or("COMPLIANCE_API_LISTEN", ":8095"),
		LogLevel:      env.ParseLevelOr(env.Or("COMPLIANCE_LOG_LEVEL", "info"), slog.LevelInfo),
		Source:        env.Or("COMPLIANCE_SOURCE", "compliance"),
		OTLPEndpoint:  os.Getenv("COMPLIANCE_OTLP_ENDPOINT"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:       os.Getenv("COMPLIANCE_NATS_URL"),
		ConsumerGroup: env.Or("COMPLIANCE_CONSUMER_GROUP", "compliance"),
	}, nil
}
