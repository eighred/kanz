// Command archiver-drain re-archives events parked on dlq.archiver (#285 item 3).
//
// WHY IT IS A SEPARATE BINARY FROM cmd/kanz-redrive. That tool drains the NATS
// dead-letter path and refuses these messages by design: the archiver parks onto
// ONE Kafka topic rather than dlq.<original>, so redrive's subject-agreement
// check cannot pass and correctly declines to guess a destination. This one
// speaks Kafka, and it re-routes with the archiver's OWN table — which is why it
// lives under services/archiver/ where that table is importable, rather than
// beside kanz-redrive where it would have to be copied.
//
// WHY IT IS NOT A LOOP INSIDE THE ARCHIVER. Every parked event is ClassTerminal:
// the failure is in the bytes or in this archiver's static config, so nothing in
// the world changes to make a replay succeed. An automatic retry would fail
// identically and re-park forever. A human fixing the routing table is the event
// that makes recovery possible, and this is the gesture that follows it.
//
// USAGE
//
//	archiver-drain                 # DRY RUN: report what would be re-archived
//	archiver-drain --write         # actually re-archive
//	archiver-drain --write --max 500
//
// The archiver MUST be stopped first (kubectl scale deploy/archiver --replicas=0).
// This refuses otherwise, and that refusal is the ordering guarantee — see
// archive.Drain.Run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/archiver/internal/archive"
	"github.com/eighred/kanz/services/archiver/internal/config"
)

// main delegates its exit code and holds nothing else.
//
// The whole lifecycle — the signal context, both broker connections and every
// defer that closes them — lives inside drain(), because os.Exit skips deferred
// functions. This binary writes to Kafka, so exiting above an unflushed writer
// would report an outcome it had not finished producing.
func main() { os.Exit(drain(os.Args[1:], os.Stdout, os.Stderr)) }

// drain is the composition root. It returns an exit code rather than an error so
// that main stays a single os.Exit and every defer below has already unwound by
// the time the process ends.
func drain(args []string, out, errOut io.Writer) int {
	// The recorder is what keeps a failure's REASON across a context
	// cancellation. This tool cancels for two different reasons — an operator's
	// SIGTERM and its own deadline — and without a recorder those arrive at the
	// same place indistinguishable, which is how a failed run reports success.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fatal := lifecycle.NewFatal(stop)

	fatal.Raise(run(ctx, args, out))
	if err := fatal.Err(); err != nil {
		fmt.Fprintf(errOut, "archiver-drain: %v\n", err)
	}
	return fatal.Code()
}

type options struct {
	natsURL  string
	brokers  string
	tenant   string
	group    string
	subjects string
	write    bool
	max      int
	timeout  time.Duration
}

func run(ctx context.Context, args []string, out io.Writer) error {
	opt, err := parseFlags(args)
	if err != nil {
		return err
	}
	if opt.tenant == "" {
		return errors.New("--tenant (or ARCHIVER_TENANT) is required: it is what the routing table " +
			"routes against, and a drain routing for the wrong tenant would re-archive onto another " +
			"tenant's topics")
	}
	if opt.natsURL == "" {
		return errors.New("--nats-url (or ARCHIVER_NATS_URL) is required: it is how the single-writer " +
			"gate observes whether the archiver is still consuming, and without it the drain would " +
			"produce beside a live writer")
	}
	if opt.brokers == "" {
		return errors.New("--brokers (or ARCHIVER_KAFKA_BROKERS) is required")
	}

	ctx, cancel := context.WithTimeout(ctx, opt.timeout)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))

	kafka, err := bus.DialKafka(bus.KafkaConfig{
		Brokers:  strings.Split(opt.brokers, ","),
		ClientID: "archiver-drain",
	})
	if err != nil {
		return fmt.Errorf("dial kafka: %w", err)
	}
	defer func() { _ = kafka.Close() }()

	// NATS is dialled for the GATE ALONE — this tool's data path is Kafka on both
	// ends. The coupling is deliberate: the archiver's liveness is a property of
	// its JetStream durable, so the only honest probe lives there. An
	// --archiver-is-stopped flag would have removed this dependency and replaced a
	// measurement with an assertion that can be wrong while looking right.
	nats, err := bus.DialNATS(ctx, bus.NATSConfig{URL: opt.natsURL, Name: "archiver-drain"})
	if err != nil {
		return fmt.Errorf("dial nats (needed for the single-writer gate): %w", err)
	}
	defer func() { _ = nats.Close() }()

	subjects := config.DefaultSubjects
	if opt.subjects != "" {
		subjects = strings.Split(opt.subjects, ",")
	}

	rep, err := archive.NewDrain(archive.DrainConfig{
		Tenant:   opt.tenant,
		Subjects: subjects,
		Group:    opt.group,
		Kafka:    kafka,
		Reader:   kafka,
		Gate:     nats,
		Logger:   logger,
		Write:    opt.write,
		Max:      opt.max,
	}).Run(ctx)

	// The report is printed even on failure: a run that refused after counting, or
	// that died partway, still tells the operator what it did before it stopped.
	// A refusal that printed nothing would be indistinguishable from a crash.
	writeReport(out, rep)

	if errors.Is(err, archive.ErrArchiverLive) {
		return fmt.Errorf("%w\n\n"+
			"  The archiver is a SINGLE WRITER by deployment. Two producers can invert per-key order\n"+
			"  in Kafka, and a reordered log rebuilds a DIFFERENT book downstream — a subtler failure\n"+
			"  than losing it. Stop it first, then drain:\n\n"+
			"      kubectl scale deploy/archiver --replicas=0\n"+
			"      archiver-drain --write\n"+
			"      kubectl scale deploy/archiver --replicas=1\n", err)
	}
	return err
}

func writeReport(out io.Writer, rep archive.DrainReport) {
	mode := "DRY RUN — nothing was written"
	if !rep.DryRun {
		mode = "WROTE"
	}
	fmt.Fprintf(out, "\narchiver-drain [%s]\n", mode)
	fmt.Fprintf(out, "  parked when the run started: %d\n", rep.Parked)
	fmt.Fprintf(out, "  read this run:               %d\n", rep.Read)
	fmt.Fprintf(out, "  routed:                      %d\n", rep.Routed)
	fmt.Fprintf(out, "  re-archived:                 %d\n", rep.Produced)
	fmt.Fprintf(out, "  re-parked (still unroutable):%d\n", rep.Reparked)
	if len(rep.Refusals) > 0 {
		fmt.Fprintf(out, "\n  still unroutable:\n")
		for _, r := range rep.Refusals {
			fmt.Fprintf(out, "    - %s\n", r)
		}
	}
	if rep.DryRun && rep.Routed > 0 {
		fmt.Fprintf(out, "\n  %d event(s) would be re-archived. Re-run with --write to do it.\n", rep.Routed)
	}
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("archiver-drain", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.natsURL, "nats-url", os.Getenv("ARCHIVER_NATS_URL"), "NATS URL — used ONLY by the single-writer gate")
	fs.StringVar(&o.brokers, "brokers", os.Getenv("ARCHIVER_KAFKA_BROKERS"), "comma-separated Kafka brokers")
	fs.StringVar(&o.tenant, "tenant", os.Getenv("ARCHIVER_TENANT"), "tenant the routing table routes against")
	fs.StringVar(&o.group, "group", envOr("ARCHIVER_CONSUMER_GROUP", "archiver"), "the ARCHIVER's durable consumer group — what the gate probes")
	fs.StringVar(&o.subjects, "subjects", os.Getenv("ARCHIVER_SUBJECTS"), "comma-separated archived subjects (default: the archiver's own list)")
	// --write, not --include-terminal. EVERY message here is ClassTerminal, so a
	// flag required on 100% of runs stops being a decision and becomes muscle
	// memory. Requiring the operator to ask to WRITE keeps the gesture meaningful.
	fs.BoolVar(&o.write, "write", false, "actually re-archive (default: dry run)")
	fs.IntVar(&o.max, "max", 0, "bound one run (0 = everything parked when it started)")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Minute, "overall deadline for the run")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	return o, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
