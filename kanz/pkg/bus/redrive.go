package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// THE DRAIN SIDE OF THE DLQ (#220).
//
// Parking a failed capital-path command and ACKING it is the correct behaviour
// and is arch-guarded as such: MaxAttempts is deliberately 1 estate-wide
// (test/arch/bus_dlq_test.go's retryCertifiedConsumers) because re-entering a
// handler in-process on the same delivery is what once left an OMS order at
// ROUTED forever. None of that is in question here.
//
// What WAS missing is the other half. `dlq.>` was a one-way door: nothing in
// the estate subscribed to it, tools/replay refuses a `dlq.` subject outright,
// and the DLQ stream is 720h of write-only retention. A 200ms Postgres blip
// therefore parked a SubmitOrder the client already held a 202 for, emitted no
// ORDER_REJECTED FACT, and left recovery to a human hand-writing a
// republisher. Parking-and-acking is right; parking into somewhere nothing can
// read from is not.
//
// WHY THIS IS NOT tools/replay. Replay is structurally the wrong vehicle and
// its refusal of `dlq.` subjects is correct, not an oversight to relax:
//
//   - It republishes into the ISOLATED `replay.{runID}.` namespace, whose whole
//     purpose (EVT-20b) is that live consumers never see it. A redriven order
//     has to land back on `order.order.submit`, where the OMS is actually
//     listening. Routing it through replay would produce
//     `replay.<id>.dlq.order.order.submit` — a subject with no stream, no
//     consumer, and a hard publish failure at the end of it.
//   - It stamps QUALITY_FLAG_REPLAYED, which the live validator hard-rejects
//     (EVT-20c). Every redriven message would be bounced straight back into the
//     DLQ by the validate step, one level deeper.
//   - Its source is the Kafka archive, addressed by topic/partition/offset. The
//     parked messages are on the NATS `dlq.>` stream.
//
// Replay answers "re-run history somewhere safe". Redrive answers "this one
// event never happened; make it happen now". They are different operations and
// the second one needed its own tool.

// Redrive defaults. Each is a bound on a way the drain can go wrong, and the
// numbers are argued rather than picked.
const (
	// DefaultMaxRedrives bounds how many times one message may make the
	// round trip DLQ → live subject → DLQ before the drain refuses to send it
	// again. It is the loop bound the whole design turns on: without it, a
	// message whose failure is permanent is redriven, fails, parks, is redriven…
	// for as long as an operator keeps running the tool, and each pass costs a
	// real dispatch attempt on a capital-path subject.
	//
	// THREE, not one and not fifty. One would make the drain single-shot, and
	// the case that motivates redrive — an infrastructure outage — routinely
	// outlasts a single operator attempt. Fifty would be indistinguishable from
	// unbounded to the human watching it. Three attempts that all fail is no
	// longer a blip: it is a message that needs a person to look at it, and the
	// refusal is how it gets one.
	DefaultMaxRedrives = 3

	// redriveDedupMargin is the headroom DefaultMinAge keeps ABOVE the consumer
	// dedup TTL. It exists so the suppressing entry is long GONE when the redrive
	// lands, not so it is "about" to expire — the same reasoning, and the same
	// shape, as claimLeaseMargin in dedup.go.
	redriveDedupMargin = 3 * time.Minute

	// DefaultMinAge is how old a parked message must be before it may be
	// redriven, and it exists because a redrive that is TOO EARLY does not fail
	// — it silently does nothing. Two independent dedup windows can swallow it,
	// and only one of them is addressable from this side:
	//
	//  1. The destination stream's `--dupe-window=2m` (every stream in
	//     infra/nats/bootstrap-job.yaml). JetStream answers a duplicate
	//     Nats-Msg-Id with a successful PubAck carrying Duplicate=true, so the
	//     publish reports success and the message is discarded. redriveMsgID
	//     below removes this one outright, by re-stamping the id — MEASURED
	//     against a real broker: the redriven copy lands on the stream.
	//  2. The CONSUMER's dedup window, which redriveMsgID CANNOT touch, because
	//     it is keyed on the envelope's idempotency_key rather than on any
	//     header. Parking calls dedup.Commit(env.IdempotencyKey), holding the key
	//     for defaultDedupTTL. A redrive inside that window is refused by Claim,
	//     skipped, and ACKED: the handler never runs, the drain reports
	//     "redriven 1", and its durable cursor has moved past the parked copy.
	//     That is the precise shape CLAUDE.md forbids — nothing configured and
	//     checked-and-fine must not look the same.
	//
	// SO IT IS DERIVED FROM defaultDedupTTL, NOT CHOSEN. It was 5 minutes against
	// a 2-minute TTL, which was correct and was also a COINCIDENCE: two unrelated
	// literals in two files, with nothing tying them and no test that would fail
	// if someone raised the TTL to ten minutes. The drain would then silently
	// stop working — every run reporting success, every message skipped. This is
	// the #237 treatment applied to the same class of bug: one number is derived
	// from the other, and the ordering is asserted BY THE COMPILER below.
	DefaultMinAge = defaultDedupTTL + redriveDedupMargin

	// DefaultIdleTimeout is how long Run waits for another message before
	// deciding the DLQ subject is drained. A redrive is a bounded operator
	// action with a summary at the end, not a daemon.
	DefaultIdleTimeout = 5 * time.Second
)

// Compile-time assertion: the default minimum age must OUTLAST the consumer
// dedup TTL, or the default drain redrives messages straight into a suppression
// window that skips and acks them — a drain that reports success and recovers
// nothing.
//
// WHAT THIS DOES AND DOES NOT CATCH, because the two are easy to conflate and a
// guard believed to cover more than it does is worse than none. The DERIVATION
// above is what makes raising defaultDedupTTL safe: DefaultMinAge moves with it,
// so the ordering cannot be broken from that direction and this expression stays
// exactly redriveDedupMargin no matter what the TTL becomes. Verified by
// mutation — setting the TTL to 10m compiles and behaves correctly.
//
// What it catches is the OTHER edit, and the likelier one: someone replacing the
// derivation with a literal ("5m is what it has always been") while the TTL is
// larger, or making the margin negative. Verified by mutation the same way —
// DefaultMinAge = 1 * time.Minute fails the build with "constant overflows uint".
const _ = uint(DefaultMinAge - defaultDedupTTL - 1)

// RedriveOptions bounds one redrive run.
type RedriveOptions struct {
	// MaxRedrives is the loop bound; see DefaultMaxRedrives. Zero means
	// DefaultMaxRedrives. Negative is refused by Run rather than treated as
	// unbounded — there is no legitimate unbounded redrive.
	MaxRedrives int
	// MinAge is the minimum age of a parked message; see DefaultMinAge. Exactly
	// zero disables the age check entirely, which is the documented operator
	// override for a message whose HeaderDLQParkedAt is missing.
	MinAge time.Duration
	// IncludeTerminal redrives messages classified ClassTerminal too. Default
	// false: a terminal failure is one the same bytes will reproduce, so
	// redriving it burns the loop budget to arrive back where it started. An
	// operator who has FIXED the defect (deployed a handler that now understands
	// the payload) is exactly who should be able to say otherwise.
	IncludeTerminal bool
}

func (o RedriveOptions) maxRedrives() int {
	if o.MaxRedrives == 0 {
		return DefaultMaxRedrives
	}
	return o.MaxRedrives
}

// RedriveRefusal is a message the drain declined to send, and why. It is a
// distinct type because a refusal is the drain WORKING — the loop bound holding,
// the age check holding — and it must never be summarised as an error count
// alongside transport failures.
type RedriveRefusal struct {
	// Subject is the DLQ subject the message was read from.
	Subject string
	// Reason is the operator-facing explanation, including what to do about it.
	Reason string
}

func (r *RedriveRefusal) Error() string { return r.Subject + ": " + r.Reason }

// ErrRedriveRefused matches any RedriveRefusal via errors.Is.
var ErrRedriveRefused = errors.New("redrive refused")

// errRedriveLimit NAKs a message the run is no longer allowed to touch because
// Redriver.Limit was reached. Internal and never returned from Run: hitting the
// limit is the operator getting what they asked for, not a failure.
var errRedriveLimit = errors.New("redrive: limit reached")

// errRedrivenThisRun NAKs a message this run already sent once, which then
// failed and came straight back. Internal, and not a failure.
//
// ONE ATTEMPT PER MESSAGE PER RUN, AND THE BUDGET DEPENDS ON IT. The drain stays
// subscribed to dlq.> while it works, so a redriven message whose handler fails
// immediately is re-parked and re-offered to the SAME run within milliseconds.
// Left alone, one run walks a message through its entire MaxRedrives budget
// against a dependency that is still down — measured against a real broker, a
// single run took a poison message from 0 to 2 redrives inside 200ms.
//
// That makes the loop bound meaningless as the human checkpoint it is documented
// to be: "three attempts that all fail is a message that needs a person" is only
// true if the three attempts are three DECISIONS. It is the same pathology #237
// found one layer down, where a bare Nak() exhausted MaxDeliver in under a
// millisecond and turned a delivery budget into no budget at all — there the fix
// was backoff, here it is that a run gets one attempt and the operator decides
// whether there is another.
var errRedrivenThisRun = errors.New("redrive: already attempted in this run")

func (r *RedriveRefusal) Is(target error) bool { return target == ErrRedriveRefused }

// PlanRedrive turns a parked message into the message that should be published
// back onto the live subject, or refuses with a reason.
//
// Pure and exported so the whole decision — routing, the loop bound, the age
// gate, the class filter — is testable without a broker. Everything that can
// send a real order back onto a real subject is decided here.
func PlanRedrive(parked Message, opt RedriveOptions, now time.Time) (Message, error) {
	refuse := func(format string, args ...any) (Message, error) {
		return Message{}, &RedriveRefusal{Subject: parked.Subject, Reason: fmt.Sprintf(format, args...)}
	}

	// The message must have come FROM the DLQ namespace. Guards against a
	// mistyped --subject pointed at a live subject, which would otherwise
	// re-publish live traffic in a loop.
	if !IsDLQSubject(parked.Subject) {
		return refuse("not a DLQ subject — redrive reads from %s*, and a live subject "+
			"republished onto itself is an infinite loop, not a drain", dlqSubjectPrefix)
	}

	dest := parked.Headers[HeaderDLQOriginalSubject]
	if dest == "" {
		return refuse("no %s header, so there is no address to send it back to. This package "+
			"stamps that header on every message it parks, so a message without one was parked by "+
			"something that does not share this contract at all. Redrive REFUSES rather than "+
			"guessing a destination for a capital-path command", HeaderDLQOriginalSubject)
	}
	if IsDLQSubject(dest) {
		return refuse("%s is %q, which is itself a DLQ subject — publishing there would bury the "+
			"message one level deeper (dlq.dlq.…) instead of draining it", HeaderDLQOriginalSubject, dest)
	}
	// The header and the subject must agree. They always do for a message this
	// package parked (publishDLQ derives one from the other), so a mismatch means
	// the header was rewritten somewhere. Refusing is what stops a corrupt or
	// forged header on a harmless `dlq.market.…` message from injecting a payload
	// onto `order.order.submit`.
	//
	// IT IS ALSO WHAT NOW REFUSES AN ARCHIVER-PARKED MESSAGE. Since #285 the
	// archiver stamps this package's header names, so the "no header" arm above no
	// longer catches it — but it parks everything onto ONE Kafka topic,
	// `dlq.archiver`, rather than `dlq.<original>`, so the derived subject cannot
	// agree and this arm refuses. That is still the right answer: those messages
	// are all ClassTerminal, they are on Kafka rather than NATS, and a drain for
	// them is a separate decision (#285 item 3) — not something to infer here.
	if want := dlqSubject(dest); want != parked.Subject {
		return refuse("%s is %q, which implies it was parked on %q — not the %q it was read from. "+
			"A rewritten destination header is the one way a redrive could put a payload on a "+
			"subject it never came from, so this refuses instead of trusting it",
			HeaderDLQOriginalSubject, dest, want, parked.Subject)
	}

	if class := parked.Headers[HeaderDLQClass]; class == ClassTerminal && !opt.IncludeTerminal {
		return refuse("classified %s (%s=%q): the failure was in the message, not the world, so "+
			"the same bytes through the same handler will park again. Redrive it only once the "+
			"defect is fixed, with IncludeTerminal / --include-terminal. Reason it parked: %s",
			ClassTerminal, HeaderDLQClass, class, parked.Headers[HeaderDLQError])
	}

	redrives, err := redriveCount(parked.Headers)
	if err != nil {
		return refuse("%v", err)
	}
	if max := opt.maxRedrives(); redrives >= max {
		return refuse("already redriven %d time(s), the limit is %d. It has failed every time, so "+
			"it is not a transient blip and another attempt will not change that. Investigate the "+
			"cause (%s), then raise --max-redrives once you expect a different outcome",
			redrives, max, parked.Headers[HeaderDLQError])
	}

	if opt.MinAge > 0 {
		parkedAt, perr := parkedAtOf(parked.Headers)
		if perr != nil {
			return refuse("%v. Redriving inside the dedup windows is silently discarded rather than "+
				"failing, so an unverifiable age is refused; pass --min-age=0 to skip the check "+
				"deliberately", perr)
		}
		if age := now.Sub(parkedAt); age < opt.MinAge {
			return refuse("parked %s ago, less than the %s minimum. A redrive this soon is swallowed "+
				"by the consumer's dedup window (the key is held from the moment it parked) and "+
				"reports success having done nothing — wait, or pass --min-age=0",
				age.Round(time.Millisecond), opt.MinAge)
		}
	}

	return Message{
		Subject: dest,
		Key:     parked.Key,
		Body:    parked.Body,
		Headers: redriveHeaders(parked.Headers, redrives+1),
	}, nil
}

// redriveHeaders builds the headers for the republished message.
//
// The Kanz-DLQ-* failure metadata is DROPPED except for the redrive count: it
// describes a delivery that is now over, and carrying HeaderDLQError forward
// onto a live subject would make a successfully-redriven message look parked to
// anything reading its headers. The count is the one field that must survive,
// because it is the loop bound and its whole mechanism is riding the round trip
// — dlqHeaders copies inbound headers verbatim, so if this message fails again
// the count comes back with it.
func redriveHeaders(parked map[string]string, redrives int) map[string]string {
	h := make(map[string]string, len(parked)+1)
	for k, v := range parked {
		switch k {
		case HeaderDLQOriginalSubject, HeaderDLQAttempts, HeaderDLQError, HeaderDLQParkedAt, HeaderDLQClass:
			continue
		}
		h[k] = v
	}
	h[HeaderDLQRedrives] = strconv.Itoa(redrives)
	if id := parked[headerNatsMsgID]; id != "" {
		h[headerNatsMsgID] = redriveMsgID(id, redrives)
	}
	return h
}

// redriveMsgID derives the broker dedup key for a redriven message.
//
// It must change, or the destination stream's 2m dupe window collapses the
// republish into a successful-looking no-op (see DefaultMinAge). It must be
// DETERMINISTIC in the redrive count rather than random, because the drain
// publishes and then acks: a crash between the two re-reads the same parked
// message at the same count on the next run, and a stable id lets the broker
// collapse that genuine duplicate. Random ids would turn every interrupted run
// into a duplicate order.
func redriveMsgID(original string, redrives int) string {
	return original + "-redrive-" + strconv.Itoa(redrives)
}

// MayBeSuppressed reports whether a parked message is young enough that the
// receiving consumer's dedup window may skip the redriven copy as a duplicate.
//
// It is a WARNING, not a verdict. The drain runs in a different process from
// the consumer and cannot observe whether a dispatch happened; all it can do is
// compare the message's age against the dedup TTL every consumer in this estate
// runs on. A false alarm is possible (a consumer with dedup disabled, or one
// that restarted since the park, has no entry to collide with) and is the right
// direction to be wrong in.
//
// An age that cannot be established returns false: PlanRedrive already refuses
// that case unless MinAge is 0, and under MinAge 0 the operator has explicitly
// taken the age question off the table.
func MayBeSuppressed(parked Message, now time.Time) bool {
	parkedAt, err := parkedAtOf(parked.Headers)
	if err != nil {
		return false
	}
	return now.Sub(parkedAt) < defaultDedupTTL
}

func redriveCount(h map[string]string) (int, error) {
	raw, ok := h[HeaderDLQRedrives]
	if !ok || raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is %q, which is not a number — the loop bound cannot be evaluated "+
			"and redriving without one is how a poison message runs forever", HeaderDLQRedrives, raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s is %q — a negative redrive count would extend the loop bound "+
			"instead of consuming it", HeaderDLQRedrives, raw)
	}
	return n, nil
}

func parkedAtOf(h map[string]string) (time.Time, error) {
	raw, ok := h[HeaderDLQParkedAt]
	if !ok || raw == "" {
		return time.Time{}, fmt.Errorf("no %s header, so the message's age cannot be established", HeaderDLQParkedAt)
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s is %q, which is not an RFC3339 timestamp", HeaderDLQParkedAt, raw)
	}
	return t, nil
}

// Redriver drains one DLQ subject back onto the live subjects its messages came
// from.
//
// It deliberately does NOT go through Consumer. Consumer unframes, validates,
// dedups and — per the arch guard — must wire a DLQ of its own, which for a
// consumer OF the DLQ would mean parking failures on `dlq.dlq.…`. The redrive
// moves opaque bytes back to where they were addressed; it is not a handler of
// the events and must not decode them. Reading them through Consumer would also
// mean a message whose envelope no longer validates could never be drained,
// when the redrive is exactly the mechanism for getting it back out.
type Redriver struct {
	// Source reads the DLQ subject. Its Subscribe must be a durable, group-based
	// subscription: the cursor is what stops a message that was successfully
	// redriven from being redriven again on the next run.
	Source Subscriber
	// Dest publishes onto the live subject.
	Dest    Publisher
	Options RedriveOptions
	Logger  *slog.Logger
	Metrics *BusMetrics
	// IdleTimeout ends the run once the subject has gone quiet. Zero means
	// DefaultIdleTimeout.
	IdleTimeout time.Duration
	// Limit stops the run cleanly after this many messages have been redriven.
	// Zero means no limit. It is the "send one and look at it" control: the
	// first real use of this tool will be during an incident, against orders
	// whose client already holds a 202, and an operator should be able to prove
	// the drain works on one message before releasing forty.
	Limit int
}

// RedriveStats summarises a run.
type RedriveStats struct {
	// Inspected counts messages read from the DLQ subject.
	Inspected int
	// Redriven counts messages republished AND acked.
	Redriven int
	// ReturnedThisRun counts messages this run redrove that FAILED AGAIN and came
	// back to the DLQ before the run ended. They are left unacked for a later
	// run — see errRedrivenThisRun. A non-zero value is the useful early signal
	// that whatever broke is still broken.
	ReturnedThisRun int
	// SuppressionRisk counts redriven messages that were younger than
	// defaultDedupTTL when they were sent — i.e. ones the receiving consumer may
	// silently skip as duplicates. It is only ever non-zero when MinAge was
	// lowered below the default, and it exists so that case cannot pass for a
	// clean run. See MayBeSuppressed.
	//
	// A COUNT AND NOT A REFUSAL. Lowering MinAge is a deliberate operator act
	// with a legitimate use (draining messages parked before Kanz-DLQ-Parked-At
	// existed, which carry no age at all), so the drain does it. What it must not
	// do is let "redriven 1" mean "and nothing happened" — the drain cannot see
	// the consumer's dedup window from another process, so it reports the risk
	// instead of claiming an outcome it did not observe.
	SuppressionRisk int
}

// Run drains dlqSubj until it goes idle, ctx is cancelled, or a message is
// refused.
//
// HALT-AND-SURFACE ON REFUSAL, NOT SKIP. A refused message is NAK'd, so it stays
// unacked and is the first thing the next run sees. The run stops there and
// returns the refusal. This is the same decision #219 made for the Kafka
// consumer — stop and be visible rather than step over something you could not
// handle — and here the reasoning is sharper: the refusals are a loop bound and
// an age gate, i.e. the drain telling an operator that this specific message
// needs a decision. Acking past it would move the cursor beyond a parked
// capital-path order and lose the one copy that was recoverable, which is the
// defect this whole tool exists to remove. There is deliberately no --skip.
//
// AT-LEAST-ONCE, PUBLISH BEFORE ACK. A crash between the two re-reads the same
// message next run; redriveMsgID keeps that from becoming a duplicate order
// within the destination's dedup window. The alternative ordering — ack then
// publish — would drop the message on the same crash, which is the failure this
// tool exists to end.
func (r *Redriver) Run(ctx context.Context, dlqSubj, group string) (RedriveStats, error) {
	var stats RedriveStats
	if r.Source == nil {
		return stats, errors.New("redrive: Source is nil")
	}
	if r.Dest == nil {
		return stats, errors.New("redrive: Dest is nil")
	}
	if group == "" {
		return stats, errors.New("redrive: group is empty — the durable cursor is what stops a " +
			"redriven message from being redriven again on the next run")
	}
	if !IsDLQSubject(dlqSubj) {
		return stats, fmt.Errorf("redrive: %q is not a DLQ subject (want %s…) — this drains the "+
			"dead-letter namespace, and pointing it at a live subject would republish live traffic "+
			"onto itself", dlqSubj, dlqSubjectPrefix)
	}
	if r.Options.MaxRedrives < 0 {
		return stats, fmt.Errorf("redrive: MaxRedrives is %d — there is no unbounded redrive; a "+
			"message that fails every attempt needs a person, not more attempts", r.Options.MaxRedrives)
	}
	if r.Options.MinAge < 0 {
		return stats, fmt.Errorf("redrive: MinAge is %s — use exactly 0 to disable the age check", r.Options.MinAge)
	}
	log := r.Logger
	if log == nil {
		log = slog.Default()
	}
	idle := r.IdleTimeout
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}

	runCtx, cancel := context.WithCancel(ctx)

	// THE WATCHDOG IS JOINED (#813). It is a Run(ctx) loop's own goroutine, and a
	// Run loop that returns while one is still running hands its caller a
	// completed shutdown that is not one — the defect the ingest engine's
	// snapshot loop had, where the escaped goroutine was still publishing.
	//
	// A deferred Wait rather than an inline one because Run has several return
	// paths below this point, and a join reached on only some of them is the
	// defect this repository keeps finding rather than a fix for it.
	//
	// DEFER ORDER IS LATENCY, NOT TERMINATION — stated precisely because the
	// obvious claim ("the other order deadlocks") is false, and was disproved by
	// mutation: the watchdog's own `time.After(idle)` arm always fires, so it
	// stops on its own within IdleTimeout regardless. Defers unwind LIFO, so
	// cancel() runs first and the already-parked watchdog leaves at once. The
	// reverse order costs the whole IdleTimeout on any path that reaches this
	// return WITHOUT the handler having called cancel() — a Subscribe that fails
	// outright is the one that does, and an operator drain reporting a transport
	// error would sit here for five seconds saying nothing. Pinned by
	// TestRedriverReturnsAsSoonAsTheSubscriptionEnds.
	var watchdog sync.WaitGroup
	defer watchdog.Wait()
	defer cancel()

	// The idle watchdog owns its own timer in a single goroutine, so there is no
	// Stop/Reset race with the dispatch goroutine. time.After per iteration is a
	// timer per message, which at operator-drain volumes is free.
	activity := make(chan struct{}, 1)
	watchdog.Add(1)
	go func() {
		defer watchdog.Done()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-activity:
			case <-time.After(idle):
				cancel()
				return
			}
		}
	}()

	// Written and read only on the subscription's single dispatch goroutine,
	// then read after Subscribe returns — which happens-after the dispatch
	// goroutine is drained and stopped (NATSClient.Subscribe drains before
	// returning).
	var halt error

	// sentThisRun holds the Nats-Msg-Id of every copy this run published. A
	// re-parked message carries that id back verbatim (redriveHeaders stamps it,
	// dlqHeaders copies inbound headers forward), so it is a stable identity for
	// "I already tried this one".
	sentThisRun := map[string]bool{}
	returnedThisRun := map[string]bool{}

	subErr := r.Source.Subscribe(runCtx, dlqSubj, group, func(ctx context.Context, msg Message) error {
		// BEFORE the activity signal, deliberately. A message this run already
		// attempted is NAK'd, and NAK means redelivery — so counting it as
		// activity would keep resetting the idle watchdog and the run would never
		// end while a fast-failing message bounced against it.
		if id := msg.Headers[headerNatsMsgID]; id != "" && sentThisRun[id] {
			// Counted and logged ONCE per message, not once per delivery. It is
			// NAK'd, so the broker keeps re-offering it on nakDelay's backoff until
			// the run ends — without this the stat would report one returned
			// message as three or four, and the log would repeat itself.
			if !returnedThisRun[id] {
				returnedThisRun[id] = true
				stats.ReturnedThisRun++
				log.Warn("a message this run redrove has already failed again and returned",
					"subject", msg.Subject, "redrives", msg.Headers[HeaderDLQRedrives],
					"parked_error", msg.Headers[HeaderDLQError],
					"detail", "left parked for a later run: the loop budget is a sequence of "+
						"operator decisions, not attempts a single run may spend on its own")
			}
			return errRedrivenThisRun // NAK — stays parked, first in line next run
		}
		select {
		case activity <- struct{}{}:
		default:
		}
		// Already halted: the drain window may still hand over buffered messages
		// after cancel(). NAK them unread rather than continuing to publish
		// orders past the point an operator has been told to look at something.
		if halt != nil {
			return halt
		}
		// Same for the limit. cancel() does not stop delivery instantly —
		// Subscribe DRAINS, deliberately, handing every already-fetched message to
		// the callback — so without this the buffered remainder would be redriven
		// past a limit an operator set precisely to stop that. NAK'd, so they are
		// still there for the next run.
		if r.Limit > 0 && stats.Redriven >= r.Limit {
			return errRedriveLimit
		}
		stats.Inspected++

		out, err := PlanRedrive(msg, r.Options, time.Now().UTC())
		if err != nil {
			r.Metrics.observeRedrive(msg.Subject, "refused")
			halt = err
			cancel()
			return err
		}
		if err := r.Dest.Publish(ctx, out); err != nil {
			r.Metrics.observeRedrive(out.Subject, "error")
			halt = fmt.Errorf("redrive: publish %s → %s: %w", msg.Subject, out.Subject, err)
			cancel()
			return halt
		}
		if id := out.Headers[headerNatsMsgID]; id != "" {
			sentThisRun[id] = true
		}
		stats.Redriven++
		r.Metrics.observeRedrive(out.Subject, "ok")
		log.Info("redrove parked message",
			"from", msg.Subject, "to", out.Subject,
			"redrives", out.Headers[HeaderDLQRedrives],
			"parked_error", msg.Headers[HeaderDLQError])
		if MayBeSuppressed(msg, time.Now().UTC()) {
			stats.SuppressionRisk++
			r.Metrics.observeRedrive(out.Subject, "suppression_risk")
			log.Warn("redriven inside the consumer dedup window — the handler may never see it",
				"to", out.Subject, "dedup_ttl", defaultDedupTTL,
				"detail", "the parking consumer committed this event's idempotency_key for "+
					"the full dedup TTL, so a copy arriving inside that window is skipped and "+
					"acked without dispatching. This run cannot tell which happened. Confirm the "+
					"handler actually ran, or re-drive after the window with the default --min-age")
		}
		// Limit reached: cancel AFTER the ack return below, so this message is
		// settled. Cancelling before returning nil would still ack (the return
		// value is what settles it), but the ordering here is explicit because
		// getting it backwards would re-redrive the last message every run.
		if r.Limit > 0 && stats.Redriven >= r.Limit {
			cancel()
		}
		return nil
	})

	if halt != nil {
		return stats, halt
	}
	if subErr != nil {
		return stats, fmt.Errorf("redrive: subscribe %s: %w", dlqSubj, subErr)
	}
	// ctx cancelled by the CALLER (not the idle watchdog) is not a clean drain:
	// the operator interrupted it, and the summary must not read as "done".
	if err := ctx.Err(); err != nil {
		return stats, fmt.Errorf("redrive: interrupted after %d redriven: %w", stats.Redriven, err)
	}
	return stats, nil
}
