package registry_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"

	"github.com/eighred/kanz/internal/prediction/registry"
	"github.com/eighred/kanz/pkg/bus"
)

// fakeLog is a LogSubscriber that hands a fixed sequence of payloads to the
// follower's handler and then fires ready — the shape SubscribeReplay has
// against a real broker, without one.
type fakeLog struct {
	payloads [][]byte
	subject  string
	errs     []error // what the handler returned, in order
}

func (f *fakeLog) SubscribeReplay(_ context.Context, subject string, h bus.EventHandler, ready func()) error {
	f.subject = subject
	for _, p := range f.payloads {
		f.errs = append(f.errs, h(context.Background(), &envelopepb.Envelope{}, p))
	}
	if ready != nil {
		ready()
	}
	return nil
}

func mustPayload(t *testing.T, pb *inferencepb.ModelRegistryEvent) []byte {
	t.Helper()
	b, err := proto.Marshal(pb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func registerPB(modelID, contract string, role inferencepb.ModelRole) *inferencepb.ModelRegistryEvent {
	return &inferencepb.ModelRegistryEvent{
		Op:            inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_REGISTER,
		Origin:        "inference-1",
		ModelId:       modelID,
		FeatureSetRef: contract,
		Role:          role,
	}
}

func validatePB(modelID string) *inferencepb.ModelRegistryEvent {
	return &inferencepb.ModelRegistryEvent{
		Op:      inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_RECORD_VALIDATION,
		Origin:  "inference-1",
		ModelId: modelID,
		Passed:  true,
	}
}

func TestBusFollower_PublishRefusesRatherThanSucceedingLocally(t *testing.T) {
	f, err := registry.NewBusFollower(&fakeLog{}, nil, func(string, string, error) {})
	if err != nil {
		t.Fatalf("NewBusFollower: %v", err)
	}
	if err := f.Publish(context.Background(), registry.Event{}); !errors.Is(err, registry.ErrNotRegistrar) {
		t.Fatalf("want ErrNotRegistrar, got %v — a Publish that succeeds in-process and is denied "+
			"by the broker at runtime reads as a broken feature rather than a missing grant", err)
	}
}

func TestBusFollower_FoldsTheLogIntoTheRegistryAndArms(t *testing.T) {
	log := &fakeLog{payloads: [][]byte{
		mustPayload(t, registerPB("gbm-3", "portfolio-risk:1", inferencepb.ModelRole_MODEL_ROLE_CANDIDATE)),
		mustPayload(t, validatePB("gbm-3")),
		mustPayload(t, registerPB("gbm-3", "portfolio-risk:1", inferencepb.ModelRole_MODEL_ROLE_PRIMARY)),
	}}
	armed := false
	f, err := registry.NewBusFollower(log, func() { armed = true }, func(r, id string, e error) {
		t.Fatalf("unexpected fold error: reason=%s model=%s err=%v", r, id, e)
	})
	if err != nil {
		t.Fatalf("NewBusFollower: %v", err)
	}
	c := registry.NewCoordinated(registry.New(nil), f, "risk-engine-0")
	if err := c.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if log.subject != registry.SubjectModelRegistered {
		t.Fatalf("subscribed to %q, want %q", log.subject, registry.SubjectModelRegistered)
	}
	if !armed {
		t.Fatal("armed never fired — a caller cannot tell 'folded the log and found nothing' " +
			"from 'has not read the log', and those are opposite findings")
	}
	_, meta, ok := c.PrimaryFor("portfolio-risk:1")
	if !ok || meta.ModelID != "gbm-3" {
		t.Fatalf("PrimaryFor after replay: ok=%v meta=%+v — the validate-then-promote ordering the "+
			"log preserves did not survive the fold", ok, meta)
	}
}

func TestBusFollower_AnUnfoldableEntryIsCountedAndTheFoldContinues(t *testing.T) {
	log := &fakeLog{payloads: [][]byte{
		{0xff, 0xff, 0xff, 0xff}, // not a ModelRegistryEvent
		mustPayload(t, registerPB("gbm-3", "portfolio-risk:1", inferencepb.ModelRole_MODEL_ROLE_CANDIDATE)),
		mustPayload(t, validatePB("gbm-3")),
		mustPayload(t, registerPB("gbm-3", "portfolio-risk:1", inferencepb.ModelRole_MODEL_ROLE_PRIMARY)),
	}}
	var reasons []string
	f, err := registry.NewBusFollower(log, nil, func(r, _ string, _ error) { reasons = append(reasons, r) })
	if err != nil {
		t.Fatalf("NewBusFollower: %v", err)
	}
	c := registry.NewCoordinated(registry.New(nil), f, "risk-engine-0")
	if err := c.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if len(reasons) != 1 || reasons[0] != registry.FoldReasonDecode {
		t.Fatalf("fold reasons = %v, want exactly [%s] — a dropped entry with no reason is a "+
			"registry that is wrong and says nothing", reasons, registry.FoldReasonDecode)
	}
	// THE POINT OF THE TEST. The three entries BEHIND the bad one must still be
	// folded: the replay class has an unbounded MaxDeliver, so returning an error
	// would redeliver entry one forever and the tail would never be read.
	for i, e := range log.errs {
		if e != nil {
			t.Fatalf("handler returned %v for entry %d — nacking an append-log entry this build "+
				"cannot fold stalls every entry behind it, forever", e, i)
		}
	}
	if _, meta, ok := c.PrimaryFor("portfolio-risk:1"); !ok || meta.ModelID != "gbm-3" {
		t.Fatalf("the fold stopped at the unreadable entry: ok=%v meta=%+v", ok, meta)
	}
}

func TestBusFollower_AnUnregisterRetiresTheModel(t *testing.T) {
	log := &fakeLog{payloads: [][]byte{
		mustPayload(t, registerPB("gbm-3", "portfolio-risk:1", inferencepb.ModelRole_MODEL_ROLE_CANDIDATE)),
		mustPayload(t, validatePB("gbm-3")),
		mustPayload(t, registerPB("gbm-3", "portfolio-risk:1", inferencepb.ModelRole_MODEL_ROLE_PRIMARY)),
		mustPayload(t, &inferencepb.ModelRegistryEvent{
			Op:      inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_UNREGISTER,
			Origin:  "inference-1",
			ModelId: "gbm-3",
		}),
	}}
	f, err := registry.NewBusFollower(log, nil, func(r, id string, e error) {
		t.Fatalf("unexpected fold error: reason=%s model=%s err=%v", r, id, e)
	})
	if err != nil {
		t.Fatalf("NewBusFollower: %v", err)
	}
	c := registry.NewCoordinated(registry.New(nil), f, "risk-engine-0")
	if err := c.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if _, _, ok := c.PrimaryFor("portfolio-risk:1"); ok {
		t.Fatal("a RETIRED model is still reported as primary for its contract — the registry is " +
			"not stale here, it is wrong, and wrong in the direction that keeps a withdrawn model " +
			"in production")
	}
}

func TestBusFollower_RefusesToBeBuiltWithoutAFoldReporter(t *testing.T) {
	if _, err := registry.NewBusFollower(&fakeLog{}, nil, nil); err == nil {
		t.Fatal("NewBusFollower accepted a nil onFold — a follower that drops entries silently " +
			"rebuilds a registry that is confidently wrong")
	}
}
