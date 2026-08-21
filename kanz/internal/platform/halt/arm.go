package halt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// Subscriber is the broadcast-subscribe surface Arm needs, satisfied by
// *bus.Consumer. It is an interface so a test can arm a Gate without a broker,
// and it is deliberately the *Ready* variant: the plain SubscribeBroadcast gives
// no signal that the broker accepted the subscription, and "did the brake line
// connect" is the whole question Arm exists to answer.
type Subscriber interface {
	SubscribeBroadcastReady(ctx context.Context, subject string, h bus.EventHandler, ready func()) error
}

// ArmTimeout bounds the wait for the broker to confirm the halt subscription and
// deliver whatever mode was last declared.
//
// It is generous on purpose. Exceeding it does not kill the process — it leaves
// the gate CLOSED with "the brake line never connected" as the reason every
// refused order carries — so the cost of waiting too long is a slower start and
// the cost of waiting too little is a fund that will not trade until someone
// restarts it. A JetStream consumer create against a healthy broker is
// milliseconds; 30s is the pathological case, not the normal one.
const ArmTimeout = 30 * time.Second

// Arm connects THIS process to the platform brake and does not return until the
// broker has confirmed the subscription and the last declared mode has been
// folded — or until it is certain that will not happen.
//
// IT IS THE ONE WAY A SERVICE JOINS THE KILL SWITCH (#635). Every service on the
// order path calls exactly this, so the delivery policy, the failure behaviour
// and the log line are the same everywhere. The alternative — each composition
// root spelling out its own `go consumer.SubscribeBroadcast(...)` — is how
// webhook-ingest ended up the only process in the estate that could hear the
// brake at all.
//
// # What it returns, and what the caller MUST do with each
//
// On success it returns a wait func. THE CALLER MUST JOIN IT (a WaitGroup
// goroutine, like every other subscription in a composition root): the
// subscription runs until ctx ends, and an unjoined goroutine is one that is
// still reading a bus its process is closing.
//
// On failure it returns an error AND HAS ALREADY TRIPPED THE GATE CLOSED. The
// caller logs it and CARRIES ON — it must not exit.
//
// WHY NOT EXIT. Refusing to start is the right instinct and the wrong action
// here, and the difference is what a halt is for. A halted service still serves
// the exits: the OMS still cancels, the gateway still answers
// POST /v1/orders/{id}/cancel. A process that exits because it could not hear
// the brake takes those down too, and turns "cannot confirm it is safe to trade"
// into "cannot get out of the book" — strictly worse than the condition it was
// reacting to. So the failure resolves the same way an undecodable ModeChanged
// does in Gate.Handle: the gate latches CLOSED, and every order the service
// refuses from then on quotes this reason back to whoever sent it. That is loud
// and it is safe. What it must never be is silent, which is precisely the shape
// this issue found: a service that starts, reports ready, and honours no halt.
//
// The state is NOT recoverable without a restart, deliberately. There is no
// subscription left to deliver a later resume, so an operator who fixes the
// grant restarts the pod — an explicit act, on a control whose entire contract
// is that leaving HALTED requires one.
func Arm(ctx context.Context, sub Subscriber, gate *Gate, logger *slog.Logger) (func() error, error) {
	if gate == nil {
		// A nil Gate is halted, so nothing unsafe follows — but nothing is
		// listening either, and a caller that passes one has a wiring bug that
		// would otherwise present as "the resume never arrives".
		return nil, errors.New("halt: nil gate — nothing would fold the brake signal")
	}
	if sub == nil {
		gate.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
			"no bus subscriber was wired to "+SubjectModeChanged+", so this process cannot hear the platform brake")
		return nil, errors.New("halt: nil subscriber")
	}
	if logger == nil {
		logger = slog.Default()
	}

	armed := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		err := sub.SubscribeBroadcastReady(ctx, SubjectModeChanged, gate.Handle, func() {
			once.Do(func() { close(armed) })
		})
		if err != nil && ctx.Err() == nil {
			gate.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
				"the halt subscription on "+SubjectModeChanged+" failed: "+err.Error())
		}
		done <- err
	}()

	timeout := time.NewTimer(ArmTimeout)
	defer timeout.Stop()

	select {
	case <-armed:
		mode, reason, since := gate.State()
		logger.Info("halt gate armed — this process now hears the platform brake",
			"subject", SubjectModeChanged, "mode", mode.String(), "reason", reason, "since", since)
		// DENY-BY-DEFAULT SURVIVES ARMING, and this line is the only thing that
		// makes it visible. A broker with no ModeChanged ever published arms
		// instantly with nothing delivered, which leaves the gate exactly as
		// NewGate built it: closed. That is the deployment sign-off cmd/kanz-halt
		// documents — bringing the platform live is an explicit, attributable act
		// — and without this WARN it looks identical to a healthy start.
		if gate.Halted() {
			logger.Warn("halt gate is CLOSED after arming — this process will REFUSE orders until an "+
				"operator resumes (kanz-halt --resume --by operator:you --reason ...)",
				"mode", mode.String(), "reason", reason)
		}
		return func() error { return <-done }, nil

	case err := <-done:
		// The subscription ended before it ever armed. Either the broker refused
		// it (no stream, no grant) or ctx was cancelled during startup.
		if err == nil {
			err = errors.New("the subscription ended before the broker confirmed it")
			if ctx.Err() == nil {
				gate.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
					"the halt subscription on "+SubjectModeChanged+" ended before it was armed")
			}
		}
		return nil, fmt.Errorf("halt: could not subscribe %s: %w", SubjectModeChanged, err)

	case <-timeout.C:
		gate.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
			"the halt subscription on "+SubjectModeChanged+" did not arm within "+ArmTimeout.String())
		return nil, fmt.Errorf("halt: subscription to %s did not arm within %s", SubjectModeChanged, ArmTimeout)
	}
}

// Refusal renders the answer a halted service owes whoever asked it to trade:
// what stopped, why, and since when. The second return is the question every
// call site is really asking, so that "am I halted" and "what do I say about it"
// cannot come apart into two different opinions on the same gate.
//
// A nil Gate answers halted, exactly as Gate.Halted does — a caller that forgot
// to wire the brake does not get to trade, and does not get a vague message
// about it either.
func Refusal(g *Gate) (string, bool) {
	if !g.Halted() {
		return "", false
	}
	mode, reason, since := g.State()
	if since.IsZero() {
		return fmt.Sprintf("platform is %s: %s", modeWord(mode), reason), true
	}
	return fmt.Sprintf("platform is %s since %s: %s",
		modeWord(mode), since.UTC().Format(time.RFC3339), reason), true
}

// modeWord renders an OperatingMode for a human reading a refusal, not for a
// machine parsing one: OPERATING_MODE_HALTED is the wire name and "halted" is
// what belongs in the sentence an operator reads at 3am.
func modeWord(m lifecyclepb.OperatingMode) string {
	switch m {
	case lifecyclepb.OperatingMode_OPERATING_MODE_HALTED:
		return "halted"
	case lifecyclepb.OperatingMode_OPERATING_MODE_DEGRADED:
		return "degraded"
	case lifecyclepb.OperatingMode_OPERATING_MODE_MAINTENANCE:
		return "in maintenance"
	case lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL:
		return "normal"
	default:
		// UNSPECIFIED is the zero-value gate: it has not been told anything yet.
		// "unknown" is the honest word, and it is a refusal for the same reason
		// the gate starts closed — not knowing means not trading.
		return "in an unknown mode"
	}
}
