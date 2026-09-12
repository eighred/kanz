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
// # It now takes two people, in two invocations (#410)
//
// A mandate is THE CONTROL every order is checked against, and #410's title names
// the gap exactly: "one person can place an order, override compliance, or change a
// mandate". Until now that was one flag on one command.
//
//	kanz-mandate propose --tenant acme --file mandate.json --by operator:akif
//	                     --reason "Q3 mandate, approved by the IC" --out proposal.json
//
//	kanz-mandate approve --tenant acme --file mandate.json
//	                     --proposal proposal.json --by operator:dana
//
// propose PUBLISHES NOTHING. It emits a proposal carrying a digest of the exact
// mandate and reason. approve re-derives that digest from the mandate file it is
// handed, refuses if the approver is the proposer, and publishes — recording both
// names on the FACT.
//
// # WHAT THIS ENFORCES AND WHAT IT DOES NOT, precisely
//
// It enforces: internal/compliance's publisher takes a dualcontrol.Approval and
// there is no argument for a lone actor, so NO code path here can publish a
// unilateral mandate; the approver differs from the proposer after case and space
// folding; the approval covers the EXACT mandate and reason, so the file cannot be
// swapped between the two steps; and the proposal expires.
//
// IT DOES NOT AUTHENTICATE THE TWO HUMANS, AND IT STILL DOES NOT. Both invocations
// run on one operator's machine under one SVID, and the proposal file is a CARRIER,
// not a signature — one person can run both steps. What the two-step buys is that
// the FACT records four eyes and the payload cannot change between them, so a
// unilateral change made THROUGH THIS TOOL is DETECTABLE in the trail and not
// prevented.
//
// # THE PREVENTABLE PATH NOW EXISTS, AND IT IS NOT THIS ONE (#562)
//
// This paragraph used to end "that is the follow-up on #410, not pretended at
// here". The follow-up landed. Two separately authenticated requests through the
// api-gateway are the real control:
//
//	POST /v1/portfolios/{id}/mandate                       propose  (authz.Mandate)
//	POST /v1/portfolios/{id}/mandate/approve               sign     (authz.Mandate)
//	GET  /v1/mandates/pending-changes                      the queue an approver acts from
//	GET  /v1/mandates/pending-changes/{proposal_id}        the proposed mandate itself (#606)
//
// The gateway authenticates each caller and injects the principal; the compliance
// service holds the proposal between the two requests and refuses a second
// signature from the first signatory, across two REQUESTS and after folding case
// and space. THAT is where a mandate change should be made.
//
// # So why does this tool still exist
//
// It is the BREAK-GLASS path, and it is deliberately kept rather than removed:
// the gateway route needs a running gateway, a running compliance service with
// COMPLIANCE_NATS_URL set, and a deployment that has named an
// API_GATEWAY_MANDATE_ROLE. An estate that has none of those — a fresh install, a
// DR rebuild, a local rig — still has to be able to put a portfolio under mandate,
// and until it can, EVERY portfolio is governed by nothing and the engine's "no
// mandate governs this portfolio" branch returns nil silently.
//
// WHAT THAT MEANS FOR AN AUDITOR: a ConfigChanged whose two names came from this
// tool proves one operator held both credentials at once, and one that came from
// the gateway proves two authenticated principals signed. The FACT does not
// distinguish them — its source field does (source "kanz-mandate" versus
// "compliance"), and that is the honest limit of what the trail can say.
//
// The file is the protojson form of compliance.v1.Mandate.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"io"
	"os"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/encoding/protojson"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-mandate: "+err.Error())
		os.Exit(1)
	}
}

// usage is returned for a missing or unknown subcommand.
//
// THE OLD SINGLE-SHOT FORM IS NOT ACCEPTED. Keeping it working "for
// compatibility" would leave the unilateral path in place beside the guarded one,
// and the unilateral path is shorter — so it is the one that would get used. A
// caller of the old form gets this error, which names the replacement.
var errUsage = errors.New(`a mandate change takes two people (#410), so this tool takes two invocations:

  kanz-mandate validate --tenant T --file mandate.json
                                                    (records no decision)

  kanz-mandate propose --tenant T --file mandate.json --by operator:alice \
               --reason "why" --out proposal.json      (publishes nothing)

  kanz-mandate approve --tenant T --file mandate.json \
               --proposal proposal.json --by operator:bob

The single-command form was removed rather than deprecated: it published a mandate
on one person's say-so, and a shorter unguarded path beside a guarded one is the
path that gets used.`)

type options struct {
	natsURL      string
	spiffeSocket string
	tenant       string
	file         string
	by           string
	reason       string
	proposal     string
	out          string
	ttl          time.Duration
	dryRun       bool
}

// proposalFile is the on-disk artifact propose emits and approve consumes.
//
// THE DIGEST INSIDE Proposal IS THE ONLY SECURITY-BEARING FIELD. Everything else
// is there so the human approving can read what they are approving without
// running a decoder — and being display-only is exactly why they must not be
// trusted: approve re-derives the digest from the MANDATE FILE it is handed and
// compares, so editing MandateID here changes nothing except what the file claims.
type proposalFile struct {
	Proposal dualcontrol.Proposal `json:"proposal"`
	Reason   string               `json:"reason"`

	// Display only. See above.
	Tenant      string `json:"tenant_id"`
	PortfolioID string `json:"portfolio_id"`
	MandateID   string `json:"mandate_id"`
	Version     uint64 `json:"version"`
	RuleCount   int    `json:"rule_count"`
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], out)
	case "propose":
		return runPropose(args[1:], out)
	case "approve":
		return runApprove(args[1:], out)
	default:
		return errUsage
	}
}

// runValidate applies the same strict protojson and domain validation used by
// both publishing paths. It requires no actor because it records no decision,
// opens no network connection, and creates no proposal or approval artifact.
func runValidate(args []string, out io.Writer) error {
	opt, err := parseFlags("validate", args)
	if err != nil {
		return err
	}
	m, err := loadMandate(opt.file, opt.tenant)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "VALID — mandate %s v%d for tenant %s portfolio %s, %d rule(s), effective %s\n",
		m.GetMandateId(), m.GetVersion(), m.GetTenantId(), m.GetPortfolioId(), len(m.GetRules()),
		m.GetEffectiveAt().AsTime().UTC().Format(time.RFC3339))
	fmt.Fprintln(out, "NOTHING WAS PROPOSED, APPROVED, OR PUBLISHED.")
	return nil
}

// runPropose validates the mandate, digests it, and writes a proposal. It opens
// no connection to the broker at all — the strongest possible statement that
// proposing is not publishing.
func runPropose(args []string, out io.Writer) error {
	opt, err := parseFlags("propose", args)
	if err != nil {
		return err
	}
	m, err := loadMandate(opt.file, opt.tenant)
	if err != nil {
		return err
	}
	digest, err := comp.MandateDigest(m, opt.reason)
	if err != nil {
		return err
	}
	// The proposal id is the config key plus the mandate version: an approval is
	// then self-evidently for one portfolio's one version, and two concurrent
	// proposals for the same version collide by name rather than both looking
	// approvable.
	id := comp.MandateConfigKey(m.GetTenantId(), m.GetPortfolioId()) +
		"@v" + fmt.Sprint(m.GetVersion())
	prop, err := dualcontrol.Propose(id, dualcontrol.ActMandateChange,
		comp.MandateConfigKey(m.GetTenantId(), m.GetPortfolioId()),
		opt.by, digest, time.Now().UTC(), opt.ttl)
	if err != nil {
		return err
	}

	pf := proposalFile{
		Proposal: prop, Reason: opt.reason,
		Tenant: m.GetTenantId(), PortfolioID: m.GetPortfolioId(),
		MandateID: m.GetMandateId(), Version: uint64(m.GetVersion()),
		RuleCount: len(m.GetRules()),
	}
	blob, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	if opt.out == "" || opt.out == "-" {
		fmt.Fprintln(out, string(blob))
	} else if err := os.WriteFile(opt.out, append(blob, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", opt.out, err)
	}

	fmt.Fprintf(out, "PROPOSED — mandate %s v%d for portfolio %s, %d rule(s), by %s\n",
		m.GetMandateId(), m.GetVersion(), m.GetPortfolioId(), len(m.GetRules()), opt.by)
	fmt.Fprintf(out, "digest %s, expires %s\n", digest[:12], prop.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintln(out, "NOTHING WAS PUBLISHED. A different person must now run `kanz-mandate approve`.")
	return nil
}

// runApprove re-derives the digest from the mandate it is handed, obtains the
// Approval, and publishes.
func runApprove(args []string, out io.Writer) error {
	opt, err := parseFlags("approve", args)
	if err != nil {
		return err
	}
	pf, err := loadProposal(opt.proposal)
	if err != nil {
		return err
	}
	m, err := loadMandate(opt.file, opt.tenant)
	if err != nil {
		return err
	}
	// RE-DERIVED FROM THE MANDATE FILE, never read from the proposal. This is the
	// check that makes swapping the file between the two steps impossible: the
	// approver signs the value, not the request.
	digest, err := comp.MandateDigest(m, pf.Reason)
	if err != nil {
		return err
	}
	approval, err := pf.Proposal.Approve(opt.by, digest, time.Now().UTC())
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "mandate %s v%d — tenant %s, portfolio %s, %d rule(s), effective %s\n",
		m.GetMandateId(), m.GetVersion(), m.GetTenantId(), m.GetPortfolioId(),
		len(m.GetRules()), m.GetEffectiveAt().AsTime().UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "proposed by %s, approved by %s\n", approval.Proposer(), approval.Approver())
	fmt.Fprintf(out, "reason: %s\n", pf.Reason)
	fmt.Fprintf(out, "subject: %s\n", comp.SubjectMandateFor(m.GetTenantId(), m.GetPortfolioId()))
	if opt.dryRun {
		fmt.Fprintln(out, "--dry-run: the approval is valid; nothing published.")
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
	// THE CLIENT IS PASSED AS WELL, and that is the #916 change. A mandate publish
	// is a read-modify-write of the portfolio's compacted subject: the value that
	// goes on the wire carries the mandate in force PLUS every mandate already
	// scheduled, so this tool has to read the subject before writing it. Publishing
	// only the mandate in --file deleted the rest, which is how scheduling a change
	// left a portfolio UNGOVERNED at the next restart.
	//
	// It also makes previous_value honest: it is now the value actually replaced,
	// read from the stream, rather than the nil this passed because it had nothing
	// to read.
	if err := comp.NewPublisher(producer, client).Publish(ctx, m, approval, pf.Reason); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	fmt.Fprintf(out, "PUBLISHED — portfolio %s is now governed by mandate %s v%d, proposed by %s and approved by %s\n",
		m.GetPortfolioId(), m.GetMandateId(), m.GetVersion(), approval.Proposer(), approval.Approver())
	fmt.Fprintln(out, "Every OMS and compliance replica arms with it — including one that boots tomorrow.")
	return nil
}

func parseFlags(sub string, args []string) (options, error) {
	fs := flag.NewFlagSet("kanz-mandate "+sub, flag.ContinueOnError)
	var opt options
	fs.StringVar(&opt.tenant, "tenant", env.Or("KANZ_TENANT", ""), "envelope tenant_id — REQUIRED (the bus rejects an untenanted envelope)")
	fs.StringVar(&opt.file, "file", "", "path to the mandate, as protojson compliance.v1.Mandate — REQUIRED")
	switch sub {
	case "propose":
		fs.StringVar(&opt.by, "by", "", `operator principal, "{type}:{id}" (e.g. operator:akif) — REQUIRED`)
		fs.StringVar(&opt.reason, "reason", "", "why this mandate is being set; recorded in the FACT and covered by the approval — REQUIRED")
		fs.StringVar(&opt.out, "out", "", "write the proposal here ('-' or empty ⇒ stdout)")
		fs.DurationVar(&opt.ttl, "ttl", dualcontrol.DefaultTTL, "how long the proposal stays approvable")
	case "approve":
		fs.StringVar(&opt.by, "by", "", `operator principal, "{type}:{id}" (e.g. operator:akif) — REQUIRED`)
		fs.StringVar(&opt.proposal, "proposal", "", "path to the proposal emitted by `kanz-mandate propose` — REQUIRED")
		fs.StringVar(&opt.natsURL, "nats", env.Or("KANZ_NATS_URL", "nats://localhost:4222"), "NATS URL of the spine")
		fs.StringVar(&opt.spiffeSocket, "spiffe-socket", env.Or("SPIFFE_ENDPOINT_SOCKET", ""),
			"SPIFFE Workload API socket for the operator SVID the production broker requires.\n"+
				"Empty ⇒ a PLAINTEXT dial: fine against a local dev broker, refused by production.")
		fs.BoolVar(&opt.dryRun, "dry-run", false, "validate the approval and print, publish nothing")
	}
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	// A mandate is a governing decision. It is attributed, or it does not happen.
	switch {
	case opt.tenant == "":
		return options{}, errors.New("--tenant is required: the bus rejects an envelope with no tenant_id")
	case opt.file == "":
		return options{}, errors.New("--file is required: the mandate (protojson compliance.v1.Mandate). approve needs it too — the approval covers the VALUE, so it is re-derived from the file rather than taken from the proposal")
	case sub != "validate" && opt.by == "":
		return options{}, errors.New("--by is required: a mandate change is attributed to a named human, or it is not made")
	}
	if sub == "propose" && opt.reason == "" {
		return options{}, errors.New("--reason is required: an unexplained change to what governs a portfolio is not auditable")
	}
	if sub == "approve" && opt.proposal == "" {
		return options{}, errors.New("--proposal is required: approve applies a proposal made by someone else, and there is no way to publish without one")
	}
	return opt, nil
}

// loadProposal reads the artifact propose emitted.
func loadProposal(path string) (proposalFile, error) {
	b, err := os.ReadFile(path) //#nosec G304 -- an operator-supplied path, by design
	if err != nil {
		return proposalFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	var pf proposalFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return proposalFile{}, fmt.Errorf("%s is not a kanz-mandate proposal: %w", path, err)
	}
	// A proposal whose act is anything else is an approval for a DIFFERENT act
	// being replayed here. Approval.Covers refuses it again at publish time; this
	// says so earlier, where the operator can still read why.
	if pf.Proposal.Act != dualcontrol.ActMandateChange {
		return proposalFile{}, fmt.Errorf("%s is a proposal for %s, not a mandate change",
			path, pf.Proposal.Act)
	}
	return pf, nil
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
	// ONE IMPLEMENTATION, SHARED WITH THE GATEWAY PATH (#562). These four checks
	// used to be written out here, and the compliance service's propose route needs
	// exactly the same four — a second copy is how the two publishers come to accept
	// different mandates, on a COMPACTED stream where the bad one is the last message
	// on the portfolio's subject and every consumer that boots arms itself with it.
	if err := comp.ValidateMandate(&m); err != nil {
		return nil, err
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
