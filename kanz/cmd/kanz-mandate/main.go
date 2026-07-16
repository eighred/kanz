// kanz-mandate is the operator's tool for putting a portfolio under mandate
// (EXEC-M13). It is the ONLY thing on this platform that publishes one.
//
// # Why this exists
//
// The OMS's PRE-TRADE COMPLIANCE GATE evaluates every order against the mandate in
// force for its portfolio. The compliance service's monitor does the same
// post-trade. Both read a registry fed from compliance.mandate.changed — and
// NOTHING PUBLISHED IT. The publisher existed, wired to no binary, in a package no
// other binary could even import.
//
// So the registry was always empty. And an empty registry does not fail: the engine
// takes the "no mandate governs this portfolio" branch and returns nil. Two controls
// — one of them on the capital path — reported healthy and permitted everything, and
// nothing in the platform said so.
//
// This is the producer. It publishes a lifecycle.v1.ConfigChanged FACT carrying the
// mandate, on the portfolio's own subject, onto the compacted MANDATE stream — so
// every OMS and compliance replica, including one that boots tomorrow, arms itself
// with the mandate in force.
//
// It is a CLI and not a service on purpose, exactly like cmd/kanz-halt: a mandate
// change is a deliberate, attributed, audited act by a named human. The FACT records
// who and why, and the tool refuses to publish without both.
//
//	kanz-mandate --tenant acme --file mandate.json --by operator:akif \
//	             --reason "Q3 mandate, approved by the IC"
//
// The file is the protojson form of compliance.v1.Mandate.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/transport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-mandate: "+err.Error())
		os.Exit(1)
	}
}

type options struct {
	natsURL      string
	spiffeSocket string
	tenant       string
	file         string
	by           string
	reason       string
	dryRun       bool
}

func run(args []string, out *os.File) error {
	opt, err := parseFlags(args)
	if err != nil {
		return err
	}
	m, err := loadMandate(opt.file, opt.tenant)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "mandate %s v%d — tenant %s, portfolio %s, %d rule(s), effective %s\n",
		m.GetMandateId(), m.GetVersion(), m.GetTenantId(), m.GetPortfolioId(),
		len(m.GetRules()), m.GetEffectiveAt().AsTime().UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "subject: %s\n", comp.SubjectMandateFor(m.GetTenantId(), m.GetPortfolioId()))
	if opt.dryRun {
		fmt.Fprintln(out, "--dry-run: nothing published.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// SEC-M3c: the operator plane's identity. Arming a portfolio's mandate is an
	// operator action on the same plane as the kill-switch, and the production
	// broker requires a client SVID — without one this dial is refused and no
	// portfolio can be put under mandate at all.
	mesh, err := transport.NewMesh(ctx, opt.spiffeSocket)
	if err != nil {
		return fmt.Errorf("spiffe identity (%s): %w", opt.spiffeSocket, err)
	}
	defer func() { _ = mesh.Close() }()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: opt.natsURL, Name: "kanz-mandate", MaxReconnects: 1, TLSConfig: mesh.Client,
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", opt.natsURL, err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "kanz-mandate",
		ProducerVersion: "1",
		Tenant:          opt.tenant,
	})
	if err != nil {
		return err
	}
	// previous is nil: this tool does not read the stream back, so it cannot honestly
	// claim what it superseded. The FACT's previous_value is for audit, and an
	// invented one is worse than an absent one — the mandate's own version carries
	// the ordering.
	if err := comp.NewPublisher(producer).Publish(ctx, m, nil, opt.by, opt.reason); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	fmt.Fprintf(out, "PUBLISHED — portfolio %s is now governed by mandate %s v%d, by %s: %s\n",
		m.GetPortfolioId(), m.GetMandateId(), m.GetVersion(), opt.by, opt.reason)
	fmt.Fprintln(out, "Every OMS and compliance replica arms with it — including one that boots tomorrow.")
	return nil
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("kanz-mandate", flag.ContinueOnError)
	var opt options
	fs.StringVar(&opt.natsURL, "nats", envOr("KANZ_NATS_URL", "nats://localhost:4222"), "NATS URL of the spine")
	fs.StringVar(&opt.spiffeSocket, "spiffe-socket", envOr("SPIFFE_ENDPOINT_SOCKET", ""),
		"SPIFFE Workload API socket for the operator SVID the production broker requires.\n"+
			"Empty ⇒ a PLAINTEXT dial: fine against a local dev broker, refused by production.")
	fs.StringVar(&opt.tenant, "tenant", envOr("KANZ_TENANT", ""), "envelope tenant_id — REQUIRED (the bus rejects an untenanted envelope)")
	fs.StringVar(&opt.file, "file", "", "path to the mandate, as protojson compliance.v1.Mandate — REQUIRED")
	fs.StringVar(&opt.by, "by", "", `operator principal, "{type}:{id}" (e.g. operator:akif) — REQUIRED`)
	fs.StringVar(&opt.reason, "reason", "", "why this mandate is being set; recorded in the FACT — REQUIRED")
	fs.BoolVar(&opt.dryRun, "dry-run", false, "validate and print, publish nothing")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	// A mandate is a governing decision. It is attributed, or it does not happen.
	switch {
	case opt.tenant == "":
		return options{}, errors.New("--tenant is required: the bus rejects an envelope with no tenant_id")
	case opt.file == "":
		return options{}, errors.New("--file is required: the mandate to publish (protojson compliance.v1.Mandate)")
	case opt.by == "":
		return options{}, errors.New("--by is required: a mandate change is attributed to a named human, or it is not made")
	case opt.reason == "":
		return options{}, errors.New("--reason is required: an unexplained change to what governs a portfolio is not auditable")
	}
	return opt, nil
}

// loadMandate reads and validates the mandate. It validates HERE, before publishing,
// because a malformed mandate on the compacted stream is worse than none: it is the
// last message on that portfolio's subject, so every consumer that boots would arm
// itself with it — forever, until somebody publishes a good one.
func loadMandate(path, tenant string) (*compliancepb.Mandate, error) {
	b, err := os.ReadFile(path) //#nosec G304 -- an operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m compliancepb.Mandate
	if err := protojson.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not a compliance.v1.Mandate: %w", path, err)
	}
	if m.GetTenantId() == "" {
		m.TenantId = tenant
	}
	if m.GetTenantId() != tenant {
		return nil, fmt.Errorf("the mandate is for tenant %q but --tenant is %q — publishing it would file one tenant's mandate under another",
			m.GetTenantId(), tenant)
	}
	switch {
	case m.GetMandateId() == "":
		return nil, errors.New("mandate_id is required")
	case m.GetPortfolioId() == "":
		return nil, errors.New("portfolio_id is required: a mandate governs a portfolio")
	case m.GetVersion() == 0:
		return nil, errors.New("version is required and monotonic: it is how a consumer orders two mandates for the same portfolio")
	}
	if m.GetEffectiveAt() == nil {
		m.EffectiveAt = timestamppb.New(time.Now().UTC())
	}
	// An empty ruleset is LEGAL and it means something: this portfolio is governed by
	// a mandate that declares no constraints. That is different from having no mandate
	// at all, and the operator should have to mean it — so it is allowed, but said.
	if len(m.GetRules()) == 0 {
		fmt.Fprintln(os.Stderr, "kanz-mandate: WARNING — this mandate declares NO RULES. "+
			"The portfolio will be governed by a mandate that constrains nothing.")
	}
	return &m, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
