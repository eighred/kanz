package config

import (
	"log/slog"
	"os"
	"strings"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/platform/subject"
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
	return Config{
		Listen:        envOr("COMPLIANCE_LISTEN", ":8091"),
		APIListen:     envOr("COMPLIANCE_API_LISTEN", ":8095"),
		LogLevel:      parseLevel(envOr("COMPLIANCE_LOG_LEVEL", "info")),
		Source:        envOr("COMPLIANCE_SOURCE", "compliance"),
		OTLPEndpoint:  os.Getenv("COMPLIANCE_OTLP_ENDPOINT"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:       os.Getenv("COMPLIANCE_NATS_URL"),
		ConsumerGroup: envOr("COMPLIANCE_CONSUMER_GROUP", "compliance"),
	}, nil
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
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
