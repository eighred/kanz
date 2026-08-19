package registry

// THE Log BINDING (#112 step 3). Until this file the seam had an in-memory
// default and nothing else, which is why the package doc's own grep for an
// importer came back empty.
//
// # This side FOLLOWS, it does not register
//
// The registrar is kanz-py: it holds the weights, it runs the MLOPS-01a
// validation, and BusRegistryPublisher is what puts a mutation on
// platform.model.registered. A Go replica reads that log to answer ONE question
// — which model, if any, serves a given feature contract — and answering it does
// not require the authority to change the answer.
//
// So Publish REFUSES rather than silently succeeding. A Go caller that tries to
// register a model through a follower gets ErrNotRegistrar with the reason,
// in-process, at the call site. The alternative was a Publish that worked in
// tests and was denied by the broker at runtime, which is the shape this
// estate has been bitten by repeatedly: authenticated fine, denied per-subject,
// reads as a broken feature rather than a missing grant.

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// ErrNotRegistrar is returned by a follower's Publish.
var ErrNotRegistrar = errors.New(
	"registry: this replica FOLLOWS the model log and cannot write to it — kanz-py's " +
		"BusRegistryPublisher is the registrar (it holds the weights and runs the MLOPS-01a " +
		"validation), and this account holds no publish grant on " + SubjectModelRegistered)

// LogSubscriber is the narrow slice of pkg/bus.Consumer a follower needs.
//
// DECLARED HERE, at the consumer, so this package does not import the whole bus
// surface for one method — and so a test can drive the fold with a fake without
// standing up a broker.
type LogSubscriber interface {
	SubscribeReplay(ctx context.Context, subject string, h bus.EventHandler, ready func()) error
}

// FoldReason labels an entry the follower could not fold. Each one is a
// DIFFERENT finding, and collapsing them would lose the only thing that says
// whether the gap is a bad producer, a build that is behind the schema, or a
// retention window that swallowed the evidence a promotion needed.
const (
	FoldReasonDecode   = "decode"     // the payload is not a ModelRegistryEvent
	FoldReasonUnknown  = "unknown_op" // an op this build has no fold for
	FoldReasonRejected = "rejected"   // the registry refused it (MLOPS-01a gate, unknown model)
)

// BusFollower is a read-only Log over the bus.
type BusFollower struct {
	sub     LogSubscriber
	onArmed func()
	onFold  func(reason string, modelID string, err error)
}

// NewBusFollower binds the model-registry log to sub.
//
// onArmed fires once the backlog that existed at subscribe time has been folded
// — the moment "this replica has read the log" becomes true, and the moment
// before which an empty registry means NOTHING RATHER THAN NO MODEL. A caller
// that reports registry state must not report it before this fires.
//
// onFold is called for every entry that could not be applied, with the reason.
// It is not optional in spirit: a follower that drops entries silently rebuilds
// a registry that is confidently wrong.
func NewBusFollower(sub LogSubscriber, onArmed func(), onFold func(reason, modelID string, err error)) (*BusFollower, error) {
	if sub == nil {
		return nil, errors.New("registry: log subscriber is nil")
	}
	if onFold == nil {
		return nil, errors.New("registry: onFold is required — an entry dropped without a reason is a registry that is wrong and says nothing")
	}
	return &BusFollower{sub: sub, onArmed: onArmed, onFold: onFold}, nil
}

// Publish refuses. See ErrNotRegistrar.
func (f *BusFollower) Publish(context.Context, Event) error { return ErrNotRegistrar }

// Replay folds platform.model.registered from the oldest entry the stream still
// holds, then keeps folding as new entries arrive. It returns when ctx ends.
//
// IT BLOCKS FOR THE LIFE OF THE PROCESS, which is what makes a follower converge
// rather than merely start correct. CoordinatedRegistry.Rebuild calls this, so
// Rebuild is a long-running call here — run it in its own goroutine and read
// onArmed for the "I have folded the log" signal, rather than waiting on the
// return.
//
// # An unfoldable entry is DROPPED, LOUDLY, and never nacked
//
// The handler returns nil even for a failure, and that is deliberate. The
// broadcast/replay class has an UNBOUNDED MaxDeliver (pkg/bus controlTuning),
// and an append-log entry this build cannot fold will not become foldable by
// being redelivered — so returning an error would redeliver one bad entry
// forever while every entry BEHIND it went unread. A registry missing its whole
// tail, stuck behind entry three, reports "no model serves this contract" with
// the same confidence as one that folded all five hundred. Dropping it and
// counting it is the honest failure: the fold continues and the gap has a
// number on it.
func (f *BusFollower) Replay(ctx context.Context, apply func(Event) error) error {
	return f.sub.SubscribeReplay(ctx, SubjectModelRegistered,
		func(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
			var pb inferencepb.ModelRegistryEvent
			if err := proto.Unmarshal(payload, &pb); err != nil {
				f.onFold(FoldReasonDecode, "", fmt.Errorf("unmarshal ModelRegistryEvent: %w", err))
				return nil
			}
			e, err := FromProto(&pb)
			if err != nil {
				reason := FoldReasonDecode
				if errors.Is(err, ErrUnknownOp) {
					reason = FoldReasonUnknown
				}
				f.onFold(reason, pb.GetModelId(), err)
				return nil
			}
			if err := apply(e); err != nil {
				f.onFold(FoldReasonRejected, e.Metadata.ModelID, err)
				return nil
			}
			return nil
		},
		f.onArmed)
}
