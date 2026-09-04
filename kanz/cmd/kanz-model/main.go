// kanz-model is the operator's tool for publishing a model portfolio — the
// target allocation a household of a given risk profile is supposed to hold
// (WEALTH-01d). It is the ONLY thing on this platform that publishes one.
//
// # Why this exists
//
// internal/wealth has carried the drift half of WEALTH-01d since it was written:
// SelectModel picks the model for a household's risk profile, ComputeDrift diffs
// the household's actual weights against it, and Drift.Breached decides whether a
// rebalance is due. None of it ever ran, because NOTHING STORED A TARGET
// ALLOCATION — wealth.v1.ModelPortfolio was fully specified in proto and never
// constructed in any Go code, so there was no catalogue, no publisher and no
// subject (#1010). The platform could execute a rebalance somebody asked for and
// could not notice that one was due.
//
// A target allocation is an investment-committee decision made OUTSIDE this
// platform; nothing in kanz derives one. That is the same situation cmd/kanz-mandate
// and cmd/kanz-household were written for, and this is the same answer.
//
// # Why this publishes COMPACTED STATE, like kanz-mandate and kanz-household
//
// A ModelPortfolio is the target IN FORCE, not an event that happened. It rides
// wealth.model.published.<tenant>.<model_id> on the WEALTH stream, which carries
// --max-msgs-per-subject=1 and no max-age (infra/nats/bootstrap-job.yaml), so the
// stream keeps exactly one message per model forever and a wealth pod booting
// tomorrow arms its catalogue from whatever this tool last published. There is one
// message type on one subject shape, so — as with kanz-household and unlike
// kanz-altevent — there is no -kind flag: nothing here selects among candidates.
//
// # Why validation happens before ANY network I/O
//
// Because the subject is compacted, a bad publish is not recoverable by leaving it
// alone: it stays the answer for that model id until somebody publishes another.
// And unlike a bad valuation, which is wrong about one household, a bad model is
// wrong about EVERY household on that risk profile at once — weights that sum to
// 0.9 report every one of them as ~10 points drifted, and a zero tolerance reports
// every one of them as permanently breached.
//
// The rule is wealth.ModelPortfolio.Validate, the SAME function the wealth
// service's catalogue applies on delivery. Sharing it is the point: a model this
// tool accepts is a model the catalogue can actually admit, rather than one that
// DLQs at delivery and leaves the profile it was meant to serve with no target at
// all.
//
//	kanz-model -tenant acme -file growth.json -by operator:akif -reason "IC review 2026-09"
//
// The file is the protojson form of wealth.v1.ModelPortfolio. -by/-reason are
// REQUIRED and are stamped onto the published FACT itself (recorded_by/reason),
// not merely printed: this subject is compacted, so a published model discards the
// one it replaced and nothing else is left to attribute the change to.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-model: "+err.Error())
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
	mp, err := loadModel(opt.file, opt.by, opt.reason)
	if err != nil {
		return err
	}

	subject := wealth.SubjectModelFor(opt.tenant, mp.GetModelId())
	fmt.Fprintf(out, "model %s — profile %s, %d target(s), drift band %.4f\n",
		mp.GetModelId(), mp.GetRiskProfile(), len(mp.GetTargetWeights()), mp.GetDriftTolerance())
	fmt.Fprintf(out, "subject: %s\n", subject)
	for _, id := range sortedKeys(mp.GetTargetWeights()) {
		fmt.Fprintf(out, "  %-24s %8.4f\n", id, mp.GetTargetWeights()[id])
	}
	if opt.dryRun {
		// Marshalled, not just summarised. This subject is COMPACTED to the LAST
		// message per model, so a bad publish does not sit alongside the good
		// history for comparison — it REPLACES the allocation every household of
		// this risk profile is measured against, and there is nothing left to diff
		// against afterwards. A human must be able to read exactly what is about to
		// overwrite it, recorded_by/reason included, protojson-rendered the way the
		// catalogue's decoder sees it.
		body, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(mp)
		if err != nil {
			return fmt.Errorf("marshal ModelPortfolio for dry-run: %w", err)
		}
		fmt.Fprintln(out, "payload:")
		fmt.Fprintln(out, string(body))
		fmt.Fprintln(out, "--dry-run: nothing published.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// SEC-M3c: the operator plane's identity, exactly as kanz-mandate and
	// kanz-household dial it. The production broker requires a client SVID —
	// without one this dial is refused and no model portfolio can be published at
	// all.
	mesh, err := transport.NewMesh(ctx, opt.spiffeSocket)
	if err != nil {
		return fmt.Errorf("spiffe identity (%s): %w", opt.spiffeSocket, err)
	}
	defer func() { _ = mesh.Close() }()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: opt.natsURL, Name: "kanz-model", MaxReconnects: 1, TLSConfig: mesh.Client,
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", opt.natsURL, err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "kanz-model",
		ProducerVersion: "1",
		Tenant:          opt.tenant,
	})
	if err != nil {
		return err
	}

	if err := producer.Publish(ctx, modelEvent(opt, mp)); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	fmt.Fprintf(out, "PUBLISHED — model %s for profile %s, by %s: %s\n",
		mp.GetModelId(), mp.GetRiskProfile(), opt.by, opt.reason)
	fmt.Fprintln(out, "Every wealth replica arms with it — including one that boots tomorrow — and every "+
		"household on this risk profile is measured against it from the next valuation onward.")
	fmt.Fprintln(out, "RETIRING A MODEL IS A SEPARATE ACT: this subject is compacted, not deleted, so a "+
		"superseded model published under a DIFFERENT model_id keeps being delivered until its own "+
		"subject is purged — and two models claiming one risk profile make that profile UNRESOLVABLE "+
		"rather than picking one.")
	return nil
}

// modelEvent builds the envelope run publishes, EXTRACTED FROM run SO A TEST CAN
// REACH IT — the same seam kanz-household's householdEvent is, and for the same
// reason (#245): run dials a broker before it constructs anything, so every field
// below would otherwise never have been through bus.Validate.
//
// THE STAKES ARE THE SAME AS kanz-household'S AND THE BLAST RADIUS IS WIDER. The
// subject is compacted to the last message per model, so a wrong PartitionKey or a
// wrong subject token does not add a bad record beside the good ones — it replaces
// some model's target allocation, and every household on that profile is measured
// against the replacement.
func modelEvent(opt options, mp *wealthpb.ModelPortfolio) bus.Event {
	return bus.Event{
		// The subject is TENANT-PREFIXED and the event type is not: the subject is
		// where the message lands, the type is what it is. Same split as
		// kanz-household.
		Subject:       wealth.SubjectModelFor(opt.tenant, mp.GetModelId()),
		EventType:     wealth.EventTypeModelPublished,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        wealth.Domain,
		// A ModelPortfolio carries no time of its own — it is the target in force
		// from the moment it is published, and there is no as_of to honour — so
		// unlike kanz-household this legitimately stamps publish time.
		EventTime:        time.Now().UTC(),
		PartitionKey:     mp.GetModelId(),
		TenantID:         opt.tenant,
		PayloadSchemaRef: "wealth.v1.ModelPortfolio:1",
		Payload:          mp,
	}
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("kanz-model", flag.ContinueOnError)
	var opt options
	fs.StringVar(&opt.natsURL, "nats", env.Or("KANZ_NATS_URL", "nats://localhost:4222"), "NATS URL of the spine")
	fs.StringVar(&opt.spiffeSocket, "spiffe-socket", env.Or("SPIFFE_ENDPOINT_SOCKET", ""),
		"SPIFFE Workload API socket for the operator SVID the production broker requires.\n"+
			"Empty ⇒ a PLAINTEXT dial: fine against a local dev broker, refused by production.")
	fs.StringVar(&opt.tenant, "tenant", env.Or("KANZ_TENANT", ""), "envelope tenant_id — REQUIRED (the bus rejects an untenanted envelope)")
	fs.StringVar(&opt.file, "file", "", "path to the model, as protojson wealth.v1.ModelPortfolio — REQUIRED")
	fs.StringVar(&opt.by, "by", "", `operator principal, "{type}:{id}" (e.g. operator:akif) — REQUIRED`)
	fs.StringVar(&opt.reason, "reason", "", "why this model is being published, e.g. the IC decision it records — REQUIRED")
	fs.BoolVar(&opt.dryRun, "dry-run", false, "validate and print, publish nothing")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	switch {
	case opt.tenant == "":
		return options{}, errors.New("--tenant is required: the bus rejects an envelope with no tenant_id")
	case opt.by == "":
		return options{}, errors.New("--by is required: the allocation every household of a risk profile is measured against is attributed to a named human, or it is not published")
	case opt.reason == "":
		return options{}, errors.New("--reason is required: an unexplained overwrite of a firm's target allocation is not auditable")
	case opt.file == "":
		return options{}, errors.New("--file is required: the model to publish (protojson wealth.v1.ModelPortfolio)")
	}
	return opt, nil
}

// loadModel reads the model, stamps the operator provenance onto the payload, and
// validates it HERE — before the network is touched — against
// wealth.ModelPortfolio.Validate, the SAME rule the wealth service's catalogue
// applies on delivery.
//
// It converts to the domain shape to validate rather than re-implementing the
// checks: a second copy of "weights must sum to 1" in this file is exactly the
// one-implementation-per-concept failure that lets a publisher accept what a
// consumer refuses, and the consequence here is a DLQ'd model and a risk profile
// left with no target allocation while this tool printed PUBLISHED.
func loadModel(path, by, reason string) (*wealthpb.ModelPortfolio, error) {
	b, err := os.ReadFile(path) //#nosec G304 -- an operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var mp wealthpb.ModelPortfolio
	if err := protojson.Unmarshal(b, &mp); err != nil {
		return nil, fmt.Errorf("%s is not a wealth.v1.ModelPortfolio: %w", path, err)
	}
	// The provenance required by parseFlags, carried onto the payload itself
	// rather than only printed — see the package doc.
	mp.RecordedBy, mp.Reason = by, reason

	domain, err := toDomain(&mp)
	if err != nil {
		return nil, err
	}
	if err := domain.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &mp, nil
}

// toDomain maps the wire model onto the domain one so Validate can be applied.
// The risk profile is an EXPLICIT SWITCH rather than a numeric cast, for the same
// reason the wealth service's decoder uses one: a value added to
// wealth.v1.RiskProfile and not to internal/wealth would be published as a profile
// no household can be matched to, and the model would sit in the catalogue serving
// nobody while looking correct.
func toDomain(mp *wealthpb.ModelPortfolio) (wealth.ModelPortfolio, error) {
	var profile wealth.RiskProfile
	switch mp.GetRiskProfile() {
	case wealthpb.RiskProfile_RISK_PROFILE_UNSPECIFIED:
		profile = wealth.ProfileUnspecified
	case wealthpb.RiskProfile_RISK_PROFILE_CONSERVATIVE:
		profile = wealth.ProfileConservative
	case wealthpb.RiskProfile_RISK_PROFILE_MODERATE:
		profile = wealth.ProfileModerate
	case wealthpb.RiskProfile_RISK_PROFILE_BALANCED:
		profile = wealth.ProfileBalanced
	case wealthpb.RiskProfile_RISK_PROFILE_GROWTH:
		profile = wealth.ProfileGrowth
	case wealthpb.RiskProfile_RISK_PROFILE_AGGRESSIVE:
		profile = wealth.ProfileAggressive
	default:
		return wealth.ModelPortfolio{}, fmt.Errorf(
			"risk_profile %v is not one this build knows; publishing it would put a model in the catalogue "+
				"that no household can ever be matched to", mp.GetRiskProfile())
	}
	targets := make(map[string]float64, len(mp.GetTargetWeights()))
	for id, w := range mp.GetTargetWeights() {
		targets[id] = w
	}
	return wealth.ModelPortfolio{
		ModelID:    mp.GetModelId(),
		Profile:    profile,
		Targets:    targets,
		Tolerance:  mp.GetDriftTolerance(),
		RecordedBy: mp.GetRecordedBy(),
		Reason:     mp.GetReason(),
	}, nil
}

// sortedKeys orders the target instruments so the printed preview — the thing an
// operator reads before overwriting a firm's target allocation — is the same on
// every run rather than in Go's map order.
func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
