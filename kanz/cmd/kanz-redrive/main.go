// Command kanz-redrive is the DLQ's drain: it reads messages parked on a
// `dlq.<subject>` stream and republishes them, byte for byte, onto the subject
// they were originally addressed to.
//
// WHY IT EXISTS (#220). A bus consumer parks a failed delivery on
// `dlq.<original-subject>` and ACKS it, which is correct — in-handler retry is
// deliberately banned estate-wide (test/arch/bus_dlq_test.go), because
// re-entering a handler on the same delivery is what once left an OMS order at
// ROUTED forever. But nothing in the estate subscribed to `dlq.>`, kanz-replay
// refuses a `dlq.` subject by design, and the DLQ stream is 720h of write-only
// retention. So a 200ms Postgres blip parked a SubmitOrder the client already
// held a 202 for, emitted no ORDER_REJECTED FACT, and left recovery to a human
// hand-writing a republisher. Parking-and-acking is right; parking into
// somewhere nothing can read from is not.
//
// Drain everything that has been parked on the order-submit path:
//
//	kanz-redrive --subject dlq.order.order.submit
//
// Send exactly one first and watch what it does — recommended, always, on a
// capital-path subject:
//
//	kanz-redrive --subject dlq.order.order.submit --limit 1
//
// THE RUN STOPS AT THE FIRST MESSAGE IT WILL NOT SEND, and says why. A refusal
// is the drain working: a message that has already been redriven to its limit,
// or one whose failure was in its own bytes rather than in the world. It is
// NAK'd, so it stays parked and is the first thing the next run sees. There is
// deliberately no --skip: acking past a parked capital-path order would lose
// the one recoverable copy, which is the defect this tool exists to remove. To
// move past one, resolve it — raise --max-redrives once you expect a different
// outcome, or pass --include-terminal once the handler defect is fixed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "kanz-redrive: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	natsURL         string
	spiffeSocket    string
	subject         string
	group           string
	maxRedrives     int
	minAge          time.Duration
	idleTimeout     time.Duration
	limit           int
	includeTerminal bool
	timeout         time.Duration
}

func run(args []string, out io.Writer) error {
	opt, err := parseFlags(args)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM cancels the run rather than killing it: Subscribe drains on
	// cancellation, so an interrupted drain settles what it already published
	// instead of leaving a message that was sent but never acked (which the next
	// run would send again).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opt.timeout)
	defer cancel()

	// The operator plane's identity, same as kanz-halt: the production broker
	// requires a client SVID (infra/nats/nats.yaml `tls { verify_and_map: true }`),
	// and a drain tool that cannot connect during an incident is not a drain. nil
	// when no socket is configured — a plaintext dial, correct against a local dev
	// broker and refused by production.
	mesh, err := transport.NewMesh(ctx, opt.spiffeSocket)
	if err != nil {
		return fmt.Errorf("spiffe identity (%s): %w", opt.spiffeSocket, err)
	}
	defer func() { _ = mesh.Close() }()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL:            opt.natsURL,
		Name:           "kanz-redrive",
		TLSConfig:      mesh.Client,
		MaxReconnects:  1,
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("connect %s: %w", opt.natsURL, err)
	}
	defer func() { _ = client.Close() }()

	// Source and Dest are the SAME client: a redrive is a move within one spine,
	// off the dlq.> stream and back onto the domain stream the subject belongs to.
	//
	// Publishing the raw bus.Message, NOT through bus.Producer. The parked bytes
	// are already a complete, stamped EventFrame — event_id, correlation_id,
	// idempotency_key and all. Re-stamping would mint a NEW event_id for an event
	// that already happened, breaking the causation chain the audit trail is built
	// from and defeating every idempotency check downstream that keys on the
	// original.
	r := &bus.Redriver{
		Source: client,
		Dest:   client,
		Options: bus.RedriveOptions{
			MaxRedrives:     opt.maxRedrives,
			MinAge:          opt.minAge,
			IncludeTerminal: opt.includeTerminal,
		},
		IdleTimeout: opt.idleTimeout,
		Limit:       opt.limit,
	}

	stats, runErr := r.Run(ctx, opt.subject, opt.group)

	// The summary prints on BOTH paths. A run that redrove 12 orders and then
	// refused the 13th has done 12 real things, and an operator who sees only the
	// error does not know that.
	fmt.Fprintf(out, "%s: inspected %d, redriven %d\n", opt.subject, stats.Inspected, stats.Redriven)

	if runErr != nil {
		var refusal *bus.RedriveRefusal
		if errors.As(runErr, &refusal) {
			fmt.Fprintf(out, "\nSTOPPED — this message was not sent, and is still parked:\n  %s\n", refusal.Reason)
			fmt.Fprintf(out, "\nIt stays at the head of %s until it is resolved. Nothing behind it "+
				"has been drained.\n", opt.subject)
			return errors.New("stopped on a refused message (see above)")
		}
		return runErr
	}
	if stats.Inspected == 0 {
		fmt.Fprintf(out, "nothing parked on %s\n", opt.subject)
	}
	return nil
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("kanz-redrive", flag.ContinueOnError)
	var opt options
	fs.StringVar(&opt.natsURL, "nats", envOr("KANZ_NATS_URL", "nats://localhost:4222"), "NATS URL of the spine")
	fs.StringVar(&opt.spiffeSocket, "spiffe-socket", envOr("SPIFFE_ENDPOINT_SOCKET", ""),
		"SPIFFE Workload API socket for the operator SVID the production broker requires.\n"+
			"\tEmpty ⇒ a PLAINTEXT dial: fine against a local dev broker, refused by production.")
	fs.StringVar(&opt.subject, "subject", "", "the DLQ subject to drain, e.g. dlq.order.order.submit — REQUIRED.\n"+
		"\tWildcards work (dlq.order.>), but prefer draining one subject at a time: a\n"+
		"\trefusal stops the whole run, so a wide pattern lets one stuck message block\n"+
		"\tthe drain of every other subject under it.")
	fs.StringVar(&opt.group, "group", "kanz-redrive", "durable consumer group.\n"+
		"\tSTABLE ON PURPOSE: the durable's cursor is what stops a message that was\n"+
		"\talready redriven from being redriven again by the next run. Change it only if\n"+
		"\tyou intend to re-read the whole 720h of DLQ retention from the start, which on\n"+
		"\ta capital-path subject means re-submitting every order it ever parked.")
	fs.IntVar(&opt.maxRedrives, "max-redrives", bus.DefaultMaxRedrives,
		"refuse a message that has already been redriven this many times.\n"+
			"\tThe loop bound: a redriven message that fails again is parked again carrying\n"+
			"\tits count, so this is what stops DLQ→live→DLQ from cycling forever.")
	fs.DurationVar(&opt.minAge, "min-age", bus.DefaultMinAge,
		"refuse a message parked more recently than this.\n"+
			"\tA redrive inside the consumer's dedup window is silently skipped and acked —\n"+
			"\tit reports success having done nothing. 0 disables the check, and is also the\n"+
			"\tway to drain messages parked before Kanz-DLQ-Parked-At existed.")
	fs.DurationVar(&opt.idleTimeout, "idle-timeout", bus.DefaultIdleTimeout,
		"end the run after this long with no further message")
	fs.IntVar(&opt.limit, "limit", 0, "stop cleanly after redriving this many messages (0 = no limit).\n"+
		"\tUse --limit 1 first on a capital-path subject.")
	fs.BoolVar(&opt.includeTerminal, "include-terminal", false,
		"also redrive messages classified terminal (unframeable bytes, a failed envelope\n"+
			"\tvalidation, a handler panic). Default false: those failed because of what they\n"+
			"\tARE, so the same bytes through the same handler park again. Set it once the\n"+
			"\tdefect is deployed-fixed.")
	fs.DurationVar(&opt.timeout, "timeout", 10*time.Minute, "overall deadline for the run")
	if err := fs.Parse(args); err != nil {
		return opt, err
	}

	if opt.subject == "" {
		return opt, errors.New("--subject is required, e.g. --subject dlq.order.order.submit")
	}
	// Checked here as well as in Redriver.Run so the operator gets it before the
	// dial, not after: the most likely typo is naming the LIVE subject, and that
	// mistake republishes live traffic onto itself.
	if !bus.IsDLQSubject(opt.subject) {
		return opt, fmt.Errorf("--subject %q is not a DLQ subject: this drains the dead-letter "+
			"namespace, so it must start with \"dlq.\" — did you mean %q?", opt.subject, "dlq."+opt.subject)
	}
	if opt.group == "" {
		return opt, errors.New("--group cannot be empty: without a durable cursor a second run " +
			"re-reads and re-sends everything the first one already redrove")
	}
	if opt.limit < 0 {
		return opt, fmt.Errorf("--limit %d is negative; 0 means no limit", opt.limit)
	}
	return opt, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
