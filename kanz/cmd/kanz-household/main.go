// kanz-household is the operator's tool for publishing a household's valuation
// (WEALTH-01b). It is the ONLY thing on this platform that publishes one.
//
// # Why this exists
//
// The wealth service folds wealth.household.valued FACTs into the household's
// current valuation, which the advisory exposure view aggregates over. The
// consumer, the decoder (services/wealth/internal/consume/proto.go) and the
// compacted WEALTH stream all exist — and NOTHING PUBLISHED TO IT. Household
// valuations arrive from advisor onboarding and custodial feeds OUTSIDE this
// platform: nothing in kanz computes a household's holdings or their market
// value. Same situation cmd/kanz-mandate was written for; same answer.
//
// # Why this is the CLOSER analogue of the two publishers, not altevent's
//
// Like kanz-mandate, this publishes COMPACTED STATE, not a journal entry: the
// WEALTH stream keeps exactly one message per household (see
// internal/wealth/subject.go's package doc and infra/nats/bootstrap-job.yaml's
// --max-msgs-per-subject=1 for that subject). A HouseholdValued is the CURRENT
// picture — a REPLACEMENT of the household's prior valuation, not a delta —
// so a pod booting tomorrow arms itself with whatever this tool last
// published. There is also exactly one message type on exactly one subject
// shape, so unlike kanz-altevent there is no -kind flag: nothing here selects
// among candidates.
//
// # Why validation happens before ANY network I/O
//
// Because the stream is compacted, a bad publish is not recoverable by
// leaving it alone — it stays the answer for this household until somebody
// publishes another good one. That is the same reasoning kanz-mandate gives
// for validating before it dials the broker, and it is why every field the
// consumer's decoder depends on (household_id, as_of, currency_code, and
// every market_value/cash) is checked here first, using the same
// dec.FromProtoChecked the decoder itself uses — so a value this tool
// accepts is a value the consumer can actually fold, not one that DLQs at
// delivery.
//
//	kanz-household -tenant acme -file household.json -by operator:akif -reason "Q2 custodial statement"
//
// The file is the protojson form of wealth.v1.HouseholdValued. -by/-reason are
// REQUIRED and are stamped onto the published FACT itself (recorded_by/reason),
// not just printed to stdout — see wealth.proto's HouseholdValued doc for why
// that matters more here than elsewhere: this stream is compacted, so a
// household's stated worth can enter the book attributable to nobody while
// silently discarding whatever valuation it replaced.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-household: "+err.Error())
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
	hv, err := loadValuation(opt.file)
	if err != nil {
		return err
	}
	// The provenance -by/-reason already required by parseFlags, carried onto
	// the payload itself (wealth.v1.HouseholdValued.recorded_by / .reason) —
	// not just printed to stdout. Without this, the operator sees PUBLISHED
	// and reasonably believes an audit trail exists on the household's stated
	// worth; before this call it did not.
	hv.RecordedBy, hv.Reason = opt.by, opt.reason

	subject := wealth.SubjectHouseholdFor(opt.tenant, hv.GetHouseholdId())
	fmt.Fprintf(out, "household %s — %d account(s), %s, as of %s\n",
		hv.GetHouseholdId(), len(hv.GetAccounts()), hv.GetCurrencyCode(),
		hv.GetAsOf().AsTime().UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "subject: %s\n", subject)
	if opt.dryRun {
		// Marshalled, not just the resolved subject: this stream is
		// COMPACTED to the LAST message per household, so a bad publish does
		// not sit alongside the good history for comparison — it silently
		// REPLACES the household's current stated worth, and there is
		// nothing left to diff against afterwards. A human must be able to
		// read exactly what is about to overwrite it, recordedBy/reason
		// included, protojson-rendered the same way the fold's decoder sees
		// it.
		body, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(hv)
		if err != nil {
			return fmt.Errorf("marshal HouseholdValued for dry-run: %w", err)
		}
		fmt.Fprintln(out, "payload:")
		fmt.Fprintln(out, string(body))
		fmt.Fprintln(out, "--dry-run: nothing published.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// SEC-M3c: the operator plane's identity, exactly as kanz-mandate and
	// kanz-altevent dial it. The production broker requires a client SVID —
	// without one this dial is refused and no household valuation can be
	// published at all.
	mesh, err := transport.NewMesh(ctx, opt.spiffeSocket)
	if err != nil {
		return fmt.Errorf("spiffe identity (%s): %w", opt.spiffeSocket, err)
	}
	defer func() { _ = mesh.Close() }()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: opt.natsURL, Name: "kanz-household", MaxReconnects: 1, TLSConfig: mesh.Client,
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", opt.natsURL, err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "kanz-household",
		ProducerVersion: "1",
		Tenant:          opt.tenant,
	})
	if err != nil {
		return err
	}

	if err := producer.Publish(ctx, bus.Event{
		Subject:       subject,
		EventType:     wealth.EventTypeHouseholdValued,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        wealth.Domain,
		// as_of, never time.Now(): this is the valuation's own point in time,
		// and the exposure view aggregates over it. Stamping ingest time would
		// silently mislabel a stale valuation as current.
		EventTime:        hv.GetAsOf().AsTime(),
		PartitionKey:     hv.GetHouseholdId(),
		TenantID:         opt.tenant,
		PayloadSchemaRef: "wealth.v1.HouseholdValued:1",
		Payload:          hv,
	}); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	fmt.Fprintf(out, "PUBLISHED — household %s valued as of %s, by %s: %s\n",
		hv.GetHouseholdId(), hv.GetAsOf().AsTime().UTC().Format(time.RFC3339), opt.by, opt.reason)
	fmt.Fprintln(out, "Every wealth consumer arms with it — including one that boots tomorrow.")
	return nil
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("kanz-household", flag.ContinueOnError)
	var opt options
	fs.StringVar(&opt.natsURL, "nats", envOr("KANZ_NATS_URL", "nats://localhost:4222"), "NATS URL of the spine")
	fs.StringVar(&opt.spiffeSocket, "spiffe-socket", envOr("SPIFFE_ENDPOINT_SOCKET", ""),
		"SPIFFE Workload API socket for the operator SVID the production broker requires.\n"+
			"Empty ⇒ a PLAINTEXT dial: fine against a local dev broker, refused by production.")
	fs.StringVar(&opt.tenant, "tenant", envOr("KANZ_TENANT", ""), "envelope tenant_id — REQUIRED (the bus rejects an untenanted envelope)")
	fs.StringVar(&opt.file, "file", "", "path to the valuation, as protojson wealth.v1.HouseholdValued — REQUIRED")
	fs.StringVar(&opt.by, "by", "", `operator principal, "{type}:{id}" (e.g. operator:akif) — REQUIRED`)
	fs.StringVar(&opt.reason, "reason", "", "why this valuation is being published, e.g. the advisor/custodial feed it transcribes — REQUIRED")
	fs.BoolVar(&opt.dryRun, "dry-run", false, "validate and print, publish nothing")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	switch {
	case opt.tenant == "":
		return options{}, errors.New("--tenant is required: the bus rejects an envelope with no tenant_id")
	case opt.by == "":
		return options{}, errors.New("--by is required: a household valuation is attributed to a named human, or it is not published")
	case opt.reason == "":
		return options{}, errors.New("--reason is required: an unexplained overwrite of a household's stated worth is not auditable")
	case opt.file == "":
		return options{}, errors.New("--file is required: the valuation to publish (protojson wealth.v1.HouseholdValued)")
	}
	return opt, nil
}

// loadValuation reads and validates the household valuation. It validates
// HERE, before publishing, because the WEALTH stream keeps only the LAST
// message per household: a bad publish is not recoverable by leaving it
// alone, only by publishing another good one, so every field the consumer's
// decoder (services/wealth/internal/consume/proto.go) depends on is checked
// against the SAME rule that decoder applies, before the network is even
// touched.
func loadValuation(path string) (*wealthpb.HouseholdValued, error) {
	b, err := os.ReadFile(path) //#nosec G304 -- an operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var hv wealthpb.HouseholdValued
	if err := protojson.Unmarshal(b, &hv); err != nil {
		return nil, fmt.Errorf("%s is not a wealth.v1.HouseholdValued: %w", path, err)
	}

	switch {
	case hv.GetHouseholdId() == "":
		return nil, errors.New("household_id is required: it is the aggregate identity and the compaction key")
	case hv.GetAsOf() == nil || !hv.GetAsOf().IsValid():
		return nil, errors.New("as_of is required: the exposure view aggregates over it, and there is no safe default for a valuation with no point in time")
	case hv.GetCurrencyCode() == "":
		return nil, errors.New("currency_code is required: a market value with no currency is not a value")
	}

	// FromProtoChecked, not FromProto: this file transcribes an advisor's or
	// custodian's feed, not something this platform computed. An exponent
	// outside the representable range is not a small number to round away —
	// coercing it to zero would silently record an account or holding worth
	// nothing and report success, overwriting whatever valuation preceded it
	// on this compacted subject.
	for _, acct := range hv.GetAccounts() {
		if acct.GetAccountId() == "" {
			return nil, fmt.Errorf("household %s: an account has no account_id", hv.GetHouseholdId())
		}
		if _, ok := dec.FromProtoChecked(acct.GetCash()); !ok {
			return nil, fmt.Errorf(
				"household %s account %s: cash cannot be represented exactly on this platform; refusing rather than rounding it",
				hv.GetHouseholdId(), acct.GetAccountId())
		}
		for _, h := range acct.GetHoldings() {
			if _, ok := dec.FromProtoChecked(h.GetMarketValue()); !ok {
				return nil, fmt.Errorf(
					"household %s account %s holding %s: market_value cannot be represented exactly on this platform; refusing rather than rounding it",
					hv.GetHouseholdId(), acct.GetAccountId(), h.GetInstrumentId())
			}
		}
	}

	return &hv, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
