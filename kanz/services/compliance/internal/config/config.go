package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/internal/marketdata/mark"
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

	// PriceSubjects are the market-data subjects the monitor folds into its
	// reference-mark source, so a post-trade book can be valued at what it is
	// WORTH rather than what it cost (#787).
	//
	// The default is mark.DefaultSubjects — the SAME declaration the OMS defaults
	// from, beside the fold that consumes it. Two services valuing the same book
	// off different subject sets would give a pre-trade and a post-trade answer
	// that disagree for reasons no operator could see, so there is one list and
	// neither service writes its own copy.
	//
	// EMPTY IS A LEGAL POSTURE HERE AND IT IS NOT IN THE OMS, which is the one
	// place these two configs are allowed to differ. The OMS refuses to start on a
	// blank list because a pod folding no marks refuses every MARKET/STOP order —
	// a trading outage. This service degrades instead: with no marks the monitor
	// cannot establish equity, so LeverageRule refuses and says so, while every
	// other rule keeps evaluating. Refusing to start would take the WHOLE monitor
	// down over one rule type, which is the outage-dressed-as-a-control mistake
	// this estate keeps a paragraph about. The posture is announced at startup.
	PriceSubjects []string

	// PriceMaxAge is how old a mark may be and still value a book. Same safety
	// bound as the OMS's, same refusal of a non-positive value: mark.Source treats
	// maxAge <= 0 as "never expires", so a stalled feed would keep valuing the
	// book at the last price it ever saw and a leverage cap would be enforced
	// against a number that stopped moving.
	PriceMaxAge time.Duration

	// ReevaluateInterval is how often every book is re-checked against the marks
	// and cash in force NOW.
	//
	// IT IS WHAT MAKES A PASSIVE BREACH VISIBLE AT ALL (#787). Every other path
	// into the monitor is woken by a FACT — a position change, a cash
	// announcement — and the breach this service exists to catch is the one with
	// no FACT behind it: a financed book that falls 20% raises its gross leverage
	// with no order placed anywhere. Without this sweep nothing ever asks.
	//
	// It bounds WORK, not latency: the sweep costs one evaluation per book per
	// interval, which is why the monitor does not simply re-evaluate on every
	// market tick. The cost of the interval is up to that much delay in noticing a
	// passive breach — the right trade for a control that catches drift rather
	// than gating an order.
	//
	// Non-positive is refused rather than treated as "off". There is no off
	// switch: a monitor that never re-evaluates is the state this issue is about.
	ReevaluateInterval time.Duration
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
	// SET-BUT-BLANK MEANS "NO PRICE FEED", and it has to be distinguishable from
	// unset for the reason the OMS distinguishes it: env.Or treats a blank value
	// as absent and returns the default, so an operator who deliberately turned
	// the feed off would silently get the default subjects back. Here the blank is
	// honoured rather than refused — see the field's comment for why this service
	// degrades where the OMS refuses.
	priceSubjectsRaw := strings.Join(mark.DefaultSubjects, ",")
	if v, ok := env.Lookup("COMPLIANCE_PRICE_SUBJECTS"); ok {
		priceSubjectsRaw = v
	} else if _, set := os.LookupEnv("COMPLIANCE_PRICE_SUBJECTS"); set {
		priceSubjectsRaw = ""
	}

	maxAge, err := time.ParseDuration(env.Or("COMPLIANCE_PRICE_MAX_AGE", "30s"))
	if err != nil {
		return Config{}, fmt.Errorf("COMPLIANCE_PRICE_MAX_AGE: %w", err)
	}
	if maxAge <= 0 {
		return Config{}, fmt.Errorf("COMPLIANCE_PRICE_MAX_AGE: must be positive (got %v); "+
			"mark.Source treats a non-positive maxAge as NEVER EXPIRES, so a stalled feed would "+
			"keep valuing the book at the last price it ever saw and a leverage cap would be "+
			"enforced against a number that stopped moving", maxAge)
	}

	interval, err := time.ParseDuration(env.Or("COMPLIANCE_REEVALUATE_INTERVAL", "60s"))
	if err != nil {
		return Config{}, fmt.Errorf("COMPLIANCE_REEVALUATE_INTERVAL: %w", err)
	}
	if interval <= 0 {
		return Config{}, fmt.Errorf("COMPLIANCE_REEVALUATE_INTERVAL: must be positive (got %v); "+
			"a monitor that never re-evaluates cannot see a breach with no FACT behind it, which "+
			"is the passive market-move breach this monitor exists to catch", interval)
	}

	return Config{
		Tenant:             env.Or("COMPLIANCE_TENANT", "__system__"),
		RefData:            refData,
		Listen:             env.Or("COMPLIANCE_LISTEN", ":8091"),
		APIListen:          env.Or("COMPLIANCE_API_LISTEN", ":8095"),
		LogLevel:           env.ParseLevelOr(env.Or("COMPLIANCE_LOG_LEVEL", "info"), slog.LevelInfo),
		Source:             env.Or("COMPLIANCE_SOURCE", "compliance"),
		OTLPEndpoint:       os.Getenv("COMPLIANCE_OTLP_ENDPOINT"),
		SPIFFESocket:       os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		NATSURL:            os.Getenv("COMPLIANCE_NATS_URL"),
		ConsumerGroup:      env.Or("COMPLIANCE_CONSUMER_GROUP", "compliance"),
		PriceSubjects:      splitSubjects(priceSubjectsRaw),
		PriceMaxAge:        maxAge,
		ReevaluateInterval: interval,
	}, nil
}

// splitSubjects turns a comma-separated list into subjects, dropping blanks so a
// trailing comma or a stray space cannot produce a subscription to "".
func splitSubjects(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
