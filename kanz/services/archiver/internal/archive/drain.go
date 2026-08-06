package archive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/eighred/kanz/internal/topic"
	"github.com/eighred/kanz/pkg/bus"
)

// DrainGroup is the Kafka consumer group archiver-drain reads dlq.archiver with.
//
// It is NOT the archiver's group. The archiver never consumes this topic — it
// only produces to it — so the two share no offsets, and a drain that fell behind
// could never affect the live path.
const DrainGroup = "archiver-drain"

// ErrArchiverLive is returned when the single-writer gate finds the archiver
// still working its durable. It is a named error so the CLI can render the
// operator instruction once, rather than every call site inventing wording for
// the platform's most consequential refusal.
var ErrArchiverLive = errors.New("archiver is still consuming")

// ConsumerGate answers whether the archiver is currently consuming. Satisfied by
// *bus.NATSClient (ConsumerActive).
//
// It is an interface here for the same reason Publisher is: the gate is the one
// thing a drain test MUST be able to drive both ways, and a test that can only
// ever see "not live" would assert the refusal never fires.
type ConsumerGate interface {
	ConsumerActive(ctx context.Context, subject, group string) (bool, error)
}

// DLQReader is the Kafka side of the drain: enough to count what is parked and
// then read exactly that much. Satisfied by *bus.KafkaClient.
type DLQReader interface {
	Backlog(ctx context.Context, topic, group string) ([]bus.PartitionBacklog, error)
	Subscribe(ctx context.Context, topic, group string, h bus.Handler) error
}

// DrainConfig configures one drain run.
type DrainConfig struct {
	// Tenant and Subjects mirror the archiver's own config: Tenant is what
	// topic.For routes against, and Subjects are the archived subjects whose
	// durables the gate probes. Both MUST match the running archiver's, or the
	// gate probes a consumer nobody is using and the routing table is not the one
	// that parked these events.
	Tenant   string
	Subjects []string
	// Group is the ARCHIVER's durable consumer group — the one the gate probes.
	Group string

	Kafka  Publisher
	Reader DLQReader
	Gate   ConsumerGate
	Logger *slog.Logger

	// Write turns produces on. Zero value is a DRY RUN, and that is deliberate:
	// every message on dlq.archiver is ClassTerminal, so a flag required on 100%
	// of runs becomes muscle memory and stops being a decision. Requiring the
	// operator to ASK to write keeps the gesture meaningful.
	Write bool

	// Max bounds one run. Zero means "everything parked at the moment the run
	// started" — see Run for why that bound is what stops a re-park loop.
	Max int
}

// DrainReport is what one run did. Every field is reported even when zero:
// "nothing was parked" and "nothing could be routed" are different outcomes and
// must not render identically.
type DrainReport struct {
	Parked   int // what the topic held when the run started
	Read     int // how many this run actually read
	Routed   int // unframed and routed to a destination topic
	Produced int // actually written (0 in a dry run, even when Routed is high)
	Reparked int // still unroutable, returned to the DLQ with a higher redrive count
	DryRun   bool
	Refusals []string
}

// Drain re-archives events parked on dlq.archiver.
type Drain struct{ cfg DrainConfig }

// NewDrain builds a Drain.
func NewDrain(cfg DrainConfig) *Drain { return &Drain{cfg: cfg} }

// Run drains, and REFUSES FIRST.
//
// THE GATE IS THE WHOLE ORDERING GUARANTEE. services/archiver is a single writer
// by deployment because two producers can invert per-key order in Kafka, and a
// reordered log rebuilds a different book downstream. This re-produces to those
// same topics, so it must not run beside the archiver. An error from the probe is
// treated as LIVE, not as idle: "the broker did not answer" and "nobody is
// consuming" are different facts, and only one of them is safe to act on.
//
// THE READ IS BOUNDED BY WHAT WAS PARKED WHEN THE RUN STARTED, and that bound is
// load-bearing rather than a courtesy. An event that still cannot be routed is
// re-parked onto the same topic this loop is reading, so an unbounded drain would
// read its own re-parks and spin forever, incrementing a redrive count until it
// hit the limit. Counting first and reading exactly that many puts every re-park
// beyond this run's horizon.
func (d *Drain) Run(ctx context.Context) (DrainReport, error) {
	rep := DrainReport{DryRun: !d.cfg.Write}

	if err := d.assertArchiverStopped(ctx); err != nil {
		return rep, err
	}

	parked, err := d.parkedCount(ctx)
	if err != nil {
		return rep, err
	}
	rep.Parked = parked
	if parked == 0 {
		return rep, nil
	}

	budget := parked
	if d.cfg.Max > 0 && d.cfg.Max < budget {
		budget = d.cfg.Max
	}

	// Subscribe blocks until ctx is done; cancelling once the budget is spent is
	// how a one-shot run ends. A drain that waited for "no more messages" could
	// not tell an empty topic from a slow broker.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	handleErr := d.cfg.Reader.Subscribe(runCtx, DLQSubject, DrainGroup, func(hctx context.Context, msg bus.Message) error {
		if rep.Read >= budget {
			cancel()
			return nil
		}
		rep.Read++
		if err := d.one(hctx, msg, &rep); err != nil {
			return err
		}
		if rep.Read >= budget {
			cancel()
		}
		return nil
	})
	if handleErr != nil && !errors.Is(handleErr, context.Canceled) {
		return rep, fmt.Errorf("archiver-drain: read %s: %w", DLQSubject, handleErr)
	}
	return rep, nil
}

// one re-archives a single parked record.
//
// It re-runs the SAME routing the archiver runs — bus.Unframe then topic.For —
// rather than trusting the destination the event was headed for when it parked.
// That is the point of the drain: the routing table has CHANGED since, which is
// the only reason any of this can succeed now, and a cached destination would
// route by the table that already failed.
func (d *Drain) one(ctx context.Context, msg bus.Message, rep *DrainReport) error {
	origin := msg.Headers[bus.HeaderDLQOriginalSubject]

	env, _, err := bus.Unframe(msg.Body)
	if err != nil {
		// Still undecodable. No table fixes bytes, so this can only be re-parked.
		rep.Refusals = append(rep.Refusals,
			fmt.Sprintf("%s: undecodable envelope (%v)", origin, err))
		return d.repark(ctx, msg, rep)
	}
	name, err := topic.For(env, d.cfg.Tenant)
	if err != nil {
		rep.Refusals = append(rep.Refusals,
			fmt.Sprintf("%s (event %s): still unroutable (%v)", origin, env.GetEventId(), err))
		return d.repark(ctx, msg, rep)
	}
	rep.Routed++

	if !d.cfg.Write {
		// A dry run reports what it WOULD do and writes nothing. Routed rises,
		// Produced does not — the two fields exist so the difference is visible.
		d.cfg.Logger.Info("archiver-drain: would re-archive",
			"origin", origin, "topic", name, "event_id", env.GetEventId())
		return nil
	}

	// Body VERBATIM and Key = partition_key, exactly as Handle writes it. The
	// drain is a transport too: an event rewritten on its way into the log of
	// record cannot be trusted as a record of what happened.
	out := bus.Message{
		Subject: name,
		Key:     []byte(env.GetPartitionKey()),
		Body:    msg.Body,
		Headers: map[string]string{HeaderEventID: env.GetEventId()},
	}
	if err := d.cfg.Kafka.Publish(ctx, out); err != nil {
		// Transient. Returning the error stops the run rather than acking an
		// event that reached neither its topic nor the DLQ.
		return fmt.Errorf("archiver-drain: publish to %q: %w", name, err)
	}
	rep.Produced++
	d.cfg.Logger.Info("archiver-drain: re-archived",
		"origin", origin, "topic", name, "event_id", env.GetEventId())
	return nil
}

// repark returns a still-unroutable event to dlq.archiver with its redrive count
// incremented.
//
// IT IS NOT A SKIP. Kafka offsets are sequential, so a run cannot commit only its
// successes; once the offset moves past a record, a record left alone is gone
// when the 30-day retention fires. Re-parking is what keeps it alive, and the
// higher count is what stops it being drained forever — pkg/bus's existing loop
// bound reads this header, so the limit is not re-derived here.
//
// A dry run re-parks NOTHING: it has not moved anything, so there is nothing to
// preserve, and writing a redrive count would make a read-only run mutate the
// very thing it was asked only to inspect.
func (d *Drain) repark(ctx context.Context, msg bus.Message, rep *DrainReport) error {
	if !d.cfg.Write {
		return nil
	}
	headers := make(map[string]string, len(msg.Headers)+1)
	for k, v := range msg.Headers {
		headers[k] = v
	}
	headers[bus.HeaderDLQRedrives] = strconv.Itoa(redrivesOf(msg.Headers) + 1)

	out := bus.Message{Subject: DLQSubject, Body: msg.Body, Headers: headers}
	if err := d.cfg.Kafka.Publish(ctx, out); err != nil {
		return fmt.Errorf("archiver-drain: re-park: %w", err)
	}
	rep.Reparked++
	return nil
}

// redrivesOf reads the redrive count, treating an absent or malformed header as
// zero. Malformed counts as zero rather than failing the run: the count bounds a
// loop, and refusing to drain a recoverable event because a diagnostic header was
// mangled would trade a real recovery for a cosmetic defect.
func redrivesOf(h map[string]string) int {
	n, err := strconv.Atoi(h[bus.HeaderDLQRedrives])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// assertArchiverStopped probes every archived subject's durable and refuses if
// ANY of them is being worked.
//
// Every subject, not the first: the archiver subscribes one durable per subject
// in its own goroutine, so a probe of one subject says nothing about the other
// fourteen, and the events parked on dlq.archiver came from all of them.
func (d *Drain) assertArchiverStopped(ctx context.Context) error {
	if len(d.cfg.Subjects) == 0 {
		return errors.New("archiver-drain: no subjects configured, so the single-writer gate has nothing " +
			"to probe. It would pass vacuously and the drain would produce beside a live archiver — " +
			"refusing rather than running an unguarded drain")
	}
	for _, subject := range d.cfg.Subjects {
		active, err := d.cfg.Gate.ConsumerActive(ctx, subject, d.cfg.Group)
		if err != nil {
			// UNKNOWN IS NOT IDLE. The broker did not answer; that is not evidence
			// the archiver is stopped, and acting as though it were is how the
			// gate would fail exactly when the estate is already unwell.
			return fmt.Errorf("%w: could not probe %q, and an unanswered probe is not an idle one: %w",
				ErrArchiverLive, subject, err)
		}
		if active {
			return fmt.Errorf("%w on %q (group %q)", ErrArchiverLive, subject, d.cfg.Group)
		}
	}
	return nil
}

// parkedCount totals what dlq.archiver holds for the drain's group across every
// partition.
func (d *Drain) parkedCount(ctx context.Context) (int, error) {
	parts, err := d.cfg.Reader.Backlog(ctx, DLQSubject, DrainGroup)
	if err != nil {
		return 0, fmt.Errorf("archiver-drain: count %s: %w", DLQSubject, err)
	}
	total := 0
	for _, p := range parts {
		total += int(p.Messages)
	}
	return total, nil
}
