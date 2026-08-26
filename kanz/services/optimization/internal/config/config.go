package config

import (
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/internal/refdata"
	"log/slog"
	"os"
)

// Config is the optimization service runtime configuration, sourced from the
// environment (the same stateless reporting/compute shape as the performance
// service — OPT-01 is a construction/proposal plane). The service optimizes
// target weights and materializes a proposal's trades into OMS-01 commands; the
// concrete bus publisher + pre-trade gate are wired at the composition root
// behind the bridge seams (DEBT-02), so the default boot serves the
// optimize/propose + dry-run-materialize endpoints without a broker.
type Config struct {
	// Listen is the API port, and it is 8100 rather than the platform's usual
	// 8080 for a reason that is not stylistic (#409).
	//
	// allow-observability-scrape selects EVERY pod in kanz-services
	// (podSelector: {}) and admits a list of ports, 8080 among them — so any
	// service listening on 8080 is reachable from the whole kanz-observability
	// namespace whatever it serves there. Moving /metrics to its own port
	// therefore buys nothing while the API stays on 8080: the API would still be
	// open to the monitoring plane, and these routes decide whose name goes on an
	// order from a header the api-gateway is supposed to be the only source of.
	//
	// 8100 is outside that list, and test/arch asserts it stays outside.
	Listen string

	// MetricsListen is a SEPARATE PORT for /metrics, and the separation is a
	// security boundary rather than tidiness (#409).
	//
	// This service decides WHOSE NAME goes on an order from the principal header
	// the api-gateway injects, so its API must be reachable by the gateway and
	// nothing else. Every other header-trusting service on this platform serves
	// its tenant-scoped routes on the same port as /metrics, and
	// allow-observability-scrape must admit that port — so a pod in
	// kanz-observability can choose a principal and use those routes. That is the
	// exposure #232 could not close, because closing it needs a second listener in
	// each service rather than a manifest edit.
	//
	// This is that second listener. The trading surface is the one place the
	// platform cannot afford to inherit the gap, so it is the first service that
	// does not. The precedent is inference, whose gRPC prediction API is
	// deliberately kept off the scrape list for the same reason.
	MetricsListen string

	// NATSURL is the event spine. Empty means this deployment PUBLISHES NOTHING:
	// the materialize route still returns the commands it built, and none reach
	// the bus.
	NATSURL string

	// Source names this producer on every envelope it emits.
	Source string

	// SPIFFESocket is the workload SVID used for mTLS to the broker. The
	// production broker refuses a plaintext client at the handshake (SEC-M3).
	SPIFFESocket string

	// AutoPublish sends the order commands a materialized proposal produces to
	// the bus, instead of building them and stopping (#409).
	//
	// THIS REVERSES A GUARDED DECISION, DELIBERATELY AND BY NAME. The bridge's
	// own header states the default stance — "the optimizer proposes, a human
	// approves, and ONLY THEN does this bridge emit commands" — and the
	// composition root had never wired a publisher. Turning this on moves the
	// platform from proposing to trading on its own recommendation. The owner
	// asked for it; the switch is explicit, OFF by default, stated at startup in
	// as many words, and counted — so no deployment acquires the behaviour by
	// omission, which is the one way this must never happen.
	//
	// WHAT IT DOES NOT CHANGE: who the order is attributed to. The issuer is
	// still the gateway-authenticated principal that called the route, and the
	// producer re-checks it at the bus (AUTH-01c). Auto-publish removes the
	// approval STEP, not the human.
	//
	// It has no effect without NATSURL, and a deployment that sets one without
	// the other is REFUSED at startup rather than left looking armed.
	AutoPublish bool
	LogLevel    slog.Level

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string
	// Tenant is the tenant THIS DEPLOYMENT serves, and it is never read from a
	// caller. It scopes the reference-data cache; the tenant a MANDATE is
	// resolved for comes from the authenticated principal on each request, which
	// is what stops one caller's portfolio being evaluated against another
	// tenant's limits (#243).
	Tenant string

	// RefData wires the instrument classifier the mandate check resolves SECTOR,
	// ISSUER and ASSET_CLASS through (#751). Unset ⇒ no classifier, and those
	// dimensions are then UNRESOLVABLE — which compliance REFUSES rather than
	// passes (#640), so a sector cap makes a proposal infeasible with a named
	// reason instead of silently feasible.
	RefData refdata.Config
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	refData, rerr := refdata.LoadConfig("OPTIMIZATION")
	if rerr != nil {
		return Config{}, rerr
	}
	cfg := Config{
		Listen:        env.Or("OPTIMIZATION_LISTEN", ":8100"),
		MetricsListen: env.Or("OPTIMIZATION_METRICS_LISTEN", ":8094"),
		Tenant:        env.Or("OPTIMIZATION_TENANT", "__system__"),
		RefData:       refData,
		NATSURL:       os.Getenv("OPTIMIZATION_NATS_URL"),
		Source:        env.Or("OPTIMIZATION_SOURCE", "optimization"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		AutoPublish:   os.Getenv("OPTIMIZATION_AUTO_PUBLISH") == "true",
		LogLevel:      env.ParseLevelOr(os.Getenv("OPTIMIZATION_LOG_LEVEL"), slog.LevelInfo),
		OTLPEndpoint:  os.Getenv("OPTIMIZATION_OTLP_ENDPOINT"),
	}

	// AUTO-PUBLISH WITH NO BROKER IS REFUSED, NOT IGNORED (#409).
	//
	// The two settings together are the entire difference between a service that
	// proposes and one that trades. Accepting the flag alone would produce a
	// deployment whose configuration says it trades on its own recommendation and
	// whose behaviour is a dry run — and the direction of that mistake is the
	// dangerous one: an operator reads the flag, believes the orders are going
	// out, and finds out otherwise from a fill that never arrives.
	//
	// A misconfiguration must surface on the first event, never as a default that
	// looks healthy.
	if cfg.AutoPublish && cfg.NATSURL == "" {
		return Config{}, fmt.Errorf("OPTIMIZATION_AUTO_PUBLISH=true but OPTIMIZATION_NATS_URL is unset: " +
			"this deployment claims to publish materialized orders and has no broker to publish them to. " +
			"Set the broker, or unset the flag — a service that silently dry-runs while its configuration " +
			"says otherwise is worse than one that does neither")
	}
	return cfg, nil
}
