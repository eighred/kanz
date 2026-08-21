// Command kanz-halt is the operator's break-glass handle on the platform
// kill-switch. It publishes a lifecycle.v1.ModeChanged FACT on
// platform.mode.changed — the single signal the shared halt.Gate folds — and
// does nothing else.
//
// It is deliberately a STANDALONE binary that shares no code with the trading
// pipeline: it imports pkg/bus, the generated schemas, and a constants-only
// package for the subject. The one tool that must work while the system is on
// fire cannot be coupled to the system that is on fire.
//
// WHAT IT ACTUALLY STOPS, as of #635. Every process that can put an order in
// front of an exchange folds this FACT through the one internal/platform/halt
// Gate: api-gateway (POST /v1/orders and the approval that releases a held
// order), the OMS (admission of any NEW order, from any publisher), both venue
// adapters (the placement itself) and webhook-ingest (the TradingView
// perimeter). Until #635 this doc said "every edge process (webhook-ingest, the
// alpha Runner)", which was an accurate description of the code and an
// inaccurate description of the line three above it — the OMS and the venue
// adapters could not reach the gate at any depth.
//
// WHAT IT DOES NOT STOP, deliberately: cancels, at every layer, and orders
// already resting at an exchange. A halt refuses NEW exposure; it is not a
// cancel and not a flatten. An operator who needs the book closed has to close
// it — this command stops the platform adding to it.
//
// The gate is DENY-BY-DEFAULT: every one of those processes boots CLOSED and
// refuses to trade until it hears an operator resume.
// That is not a bug to work around — it is the deployment sign-off. Bringing the
// platform live is an explicit, attributable act, recorded in the append-only
// log:
//
//	kanz-halt --resume --by operator:akif --reason "initial deployment validation"
//
// And stopping it is the same act in reverse:
//
//	kanz-halt --by operator:akif --reason "risk breach on fund-alpha"
//
// A halt is FAIL-CLOSED and LATCHING: once tripped, reconnecting the bus does not
// clear it, and no automated actor can. Only an operator resume reopens the gate.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"

	"github.com/eighred/kanz/internal/platform/mode"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

// schemaRefModeChanged identifies the payload on the wire, "{package}.{Message}:{version}".
// bus.Validate REQUIRES it — an envelope without one is rejected at Publish, so a
// kill-switch that omitted it would fail in the operator's hands during an incident
// and nowhere before.
const schemaRefModeChanged = "lifecycle.v1.ModeChanged:1"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "kanz-halt: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	natsURL      string
	spiffeSocket string
	by           string
	reason       string
	tenant       string
	previous     string
	resume       bool
	timeout      time.Duration
}

func run(args []string, out *os.File) error {
	opt, err := parseFlags(args)
	if err != nil {
		return err
	}

	mc, err := buildFact(opt)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), opt.timeout)
	defer cancel()

	// THE OPERATOR PLANE'S IDENTITY (SEC-M3c).
	//
	// The production broker requires a client SVID (infra/nats/nats.yaml: `tls {
	// verify: true, verify_and_map: true }`). This tool presented none — so the
	// one command that must work while the system is on fire could not connect to
	// the spine at all, and because the halt gate is DENY-BY-DEFAULT that also
	// meant nothing could RESUME it: the platform could not be brought live.
	//
	// It authenticates by SPIFFE, not OIDC, and that is the decision, not a
	// convenience. THE TWO IDENTITY PLANES DO NOT MEET: an SSO round-trip to
	// stop a system whose auth may be part of the incident is a dependency on the
	// thing you are escaping. An SVID is issued to this Job by SPIRE, and it grants
	// no `Trade`.
	//
	// The rule predates this file; the design document that first stated it was
	// deleted on 2026-07-29, so it is restated here rather than cited — what
	// keeps it true is in this repository: the dial below takes an SVID from the
	// workload API and never a human credential, and the operator plane's
	// capabilities are the api-gateway's (authz.Trade among them), which this Job
	// is not issued and cannot obtain.
	//
	// Bounded by the SAME deadline as the dial, deliberately: NewSource respects an
	// earlier parent deadline, so --timeout still means what it says. A break-glass
	// tool that blocks 30s waiting for an agent is a broken brake.
	mesh, err := transport.NewMesh(ctx, opt.spiffeSocket)
	if err != nil {
		return fmt.Errorf("spiffe identity (%s): %w", opt.spiffeSocket, err)
	}
	defer func() { _ = mesh.Close() }()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL:  opt.natsURL,
		Name: "kanz-halt",
		// nil when no socket is configured — a plaintext dial, correct against a
		// local dev broker and refused by the production one.
		TLSConfig: mesh.Client,
		// Fail fast. A break-glass tool that silently retries a dead spine forever
		// is worse than one that tells the operator it could not stop the system.
		MaxReconnects:  1,
		ConnectTimeout: opt.timeout,
	})
	if err != nil {
		return fmt.Errorf("connect %s: %w", opt.natsURL, err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "kanz-halt",
		ProducerVersion: version.String(),
	})
	if err != nil {
		return err
	}

	err = producer.Publish(ctx, bus.Event{
		Subject:          mode.Subject,
		EventType:        mode.Subject,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "platform",
		PayloadSchemaRef: schemaRefModeChanged,
		EventTime:        time.Now().UTC(),
		// component is the partition key, so a component's transitions are totally
		// ordered — two operators racing to halt cannot interleave into an
		// ambiguous final state.
		PartitionKey: mc.GetComponent(),
		TenantID:     opt.tenant,
		Payload:      mc,
	})
	if err != nil {
		return fmt.Errorf("publish halt FACT: %w", err)
	}

	if opt.resume {
		fmt.Fprintf(out, "RESUMED — system mode NORMAL, by %s: %s\n", mc.GetChangedBy(), mc.GetReason())
		fmt.Fprintln(out, "The order path reopens on each service's next delivery.")
		return nil
	}
	// THE SCOPE IS PRINTED, not left to the operator's memory (#635). This is the
	// sentence someone reads at 3am immediately after stopping the platform, and
	// the two things it must not leave them guessing about are what is still
	// running and what they still have to do themselves.
	fmt.Fprintf(out, "HALTED — system mode HALTED, by %s: %s\n", mc.GetChangedBy(), mc.GetReason())
	fmt.Fprintln(out, "STOPPED: no NEW order is admitted or placed — POST /v1/orders (423),")
	fmt.Fprintln(out, "         the OMS (ORDER_REJECTED/PLATFORM_HALTED), both venue adapters,")
	fmt.Fprintln(out, "         TradingView signals and the alpha runner.")
	fmt.Fprintln(out, "NOT STOPPED: cancels, at every layer — deliberately, so you can still exit.")
	fmt.Fprintln(out, "STILL LIVE: orders already resting at an exchange, and their fills. A halt is")
	fmt.Fprintln(out, "         not a cancel — flattening the book is a separate act you must take.")
	fmt.Fprintln(out, "This LATCHES: only `kanz-halt --resume` reopens the gate.")
	return nil
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("kanz-halt", flag.ContinueOnError)
	var opt options
	fs.StringVar(&opt.natsURL, "nats", envOr("KANZ_NATS_URL", "nats://localhost:4222"), "NATS URL of the spine")
	fs.StringVar(&opt.spiffeSocket, "spiffe-socket", envOr("SPIFFE_ENDPOINT_SOCKET", ""),
		"SPIFFE Workload API socket for the operator SVID the production broker requires\n"+
			"(e.g. unix:///run/spiffe/spire-agent.sock). Empty ⇒ a PLAINTEXT dial: fine against\n"+
			"a local dev broker, refused by production. See infra/nats/halt-job.yaml.")
	fs.StringVar(&opt.by, "by", "", `operator principal, "{type}:{id}" (e.g. operator:akif) — REQUIRED`)
	fs.StringVar(&opt.reason, "reason", "", "why the mode is changing; recorded in the FACT — REQUIRED")
	fs.StringVar(&opt.tenant, "tenant", envOr("KANZ_TENANT", ""), "envelope tenant_id — REQUIRED.\n"+
		"\tAUDIT/ROUTING ONLY. The halt is PLATFORM-WIDE: the gate ignores this field\n"+
		"\tand stops every tenant's execution. There is no per-tenant halt today.")
	fs.StringVar(&opt.previous, "previous", "", "the mode in effect before this transition\n"+
		"\t(normal|degraded|maintenance|halted). Optional: left unset the FACT carries\n"+
		"\tUNSPECIFIED rather than a guess — this tool cannot observe prior state, and\n"+
		"\tinventing it would be fabricating an audit record.")
	fs.BoolVar(&opt.resume, "resume", false, "resume trading (mode NORMAL) instead of halting")
	fs.DurationVar(&opt.timeout, "timeout", 10*time.Second, "connect + publish timeout")
	if err := fs.Parse(args); err != nil {
		return opt, err
	}

	if opt.by == "" {
		return opt, errors.New("--by is required: a mode change must be attributable to a principal")
	}
	if opt.reason == "" {
		return opt, errors.New("--reason is required: lifecycle.v1 forbids an unexplained transition")
	}
	if opt.tenant == "" {
		return opt, errors.New("--tenant is required: the bus rejects an envelope with no tenant_id on the live path")
	}
	if !strings.Contains(opt.by, ":") {
		return opt, fmt.Errorf(`--by %q is not a principal: use "{type}:{id}", e.g. operator:akif`, opt.by)
	}
	// The gate deliberately ignores a NORMAL transition issued by "system:" — an
	// automated detector must never be able to clear a safety trip. A resume sent
	// under a system: principal would publish cleanly and change nothing, which is
	// the worst possible outcome for a tool an operator reaches for in an incident.
	// Reject it here, loudly, rather than let it no-op on the far side.
	if opt.resume && strings.HasPrefix(opt.by, "system:") {
		return opt, fmt.Errorf(`--by %q cannot resume: the gate ignores a "system:" resume so that automated `+
			`recovery cannot clear a safety trip. Resume under a human principal (e.g. operator:akif)`, opt.by)
	}
	return opt, nil
}

func buildFact(opt options) (*lifecyclepb.ModeChanged, error) {
	newMode := lifecyclepb.OperatingMode_OPERATING_MODE_HALTED
	if opt.resume {
		newMode = lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL
	}
	prev, err := parseMode(opt.previous)
	if err != nil {
		return nil, err
	}
	if prev == newMode && prev != lifecyclepb.OperatingMode_OPERATING_MODE_UNSPECIFIED {
		return nil, fmt.Errorf("--previous %s equals the new mode: a ModeChanged is always a real transition", opt.previous)
	}
	return &lifecyclepb.ModeChanged{
		Component:    mode.ComponentSystem,
		PreviousMode: prev,
		NewMode:      newMode,
		Reason:       opt.reason,
		ChangedBy:    opt.by,
	}, nil
}

func parseMode(s string) (lifecyclepb.OperatingMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return lifecyclepb.OperatingMode_OPERATING_MODE_UNSPECIFIED, nil
	case "normal":
		return lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL, nil
	case "degraded":
		return lifecyclepb.OperatingMode_OPERATING_MODE_DEGRADED, nil
	case "maintenance":
		return lifecyclepb.OperatingMode_OPERATING_MODE_MAINTENANCE, nil
	case "halted":
		return lifecyclepb.OperatingMode_OPERATING_MODE_HALTED, nil
	default:
		return lifecyclepb.OperatingMode_OPERATING_MODE_UNSPECIFIED,
			fmt.Errorf("--previous %q: want normal|degraded|maintenance|halted", s)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
