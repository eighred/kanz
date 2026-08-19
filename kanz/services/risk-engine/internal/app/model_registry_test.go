package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"

	"github.com/eighred/kanz/internal/prediction/registry"
	"github.com/eighred/kanz/pkg/bus"
)

// THE FOUR STATES ARE THE POINT OF THIS FILE. "I have not read the log" and "I
// read the log and no model claims this contract" are opposite findings that an
// empty registry cannot tell apart, and reporting the second when the first is
// true manufactures an AI outage out of a startup race.

type stubLog struct {
	payloads [][]byte
	// block, when set, makes SubscribeReplay wait for ctx instead of arming — the
	// pod whose subscription never comes up.
	block bool
}

func (s *stubLog) SubscribeReplay(ctx context.Context, _ string, h bus.EventHandler, ready func()) error {
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	for _, p := range s.payloads {
		if err := h(ctx, &envelopepb.Envelope{}, p); err != nil {
			return err
		}
	}
	if ready != nil {
		ready()
	}
	return nil
}

func pbBytes(t *testing.T, e *inferencepb.ModelRegistryEvent) []byte {
	t.Helper()
	b, err := proto.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// promoted is the three-entry sequence the registrar publishes to put a model
// into production: register as candidate, file the MLOPS-01a evidence, promote.
func promoted(t *testing.T, modelID, contract string, expires time.Time) [][]byte {
	t.Helper()
	v := &inferencepb.ModelRegistryEvent{
		Op:      inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_RECORD_VALIDATION,
		Origin:  "inference-1",
		ModelId: modelID,
		Passed:  true,
	}
	if !expires.IsZero() {
		v.ExpiresAt = timestamppb.New(expires)
	}
	return [][]byte{
		pbBytes(t, &inferencepb.ModelRegistryEvent{
			Op: inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_REGISTER, Origin: "inference-1",
			ModelId: modelID, FeatureSetRef: contract,
			Role: inferencepb.ModelRole_MODEL_ROLE_CANDIDATE,
		}),
		pbBytes(t, v),
		pbBytes(t, &inferencepb.ModelRegistryEvent{
			Op: inferencepb.ModelRegistryOp_MODEL_REGISTRY_OP_REGISTER, Origin: "inference-1",
			ModelId: modelID, FeatureSetRef: contract,
			Role: inferencepb.ModelRole_MODEL_ROLE_PRIMARY,
		}),
	}
}

func newTestModelRegistry(t *testing.T, sub registry.LogSubscriber) (*ModelRegistry, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := NewModelRegistry(reg, slog.New(slog.NewTextHandler(io.Discard, nil)), sub,
		"risk-engine-test", FeatureSetPortfolioRisk)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	return m, reg
}

func TestModelRegistry_UnknownUntilTheLogHasBeenFolded(t *testing.T) {
	m, reg := newTestModelRegistry(t, &stubLog{block: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	if _, state := m.PrimaryFor(FeatureSetPortfolioRisk); state != ModelStateUnknown {
		t.Fatalf("state before the fold = %q, want %q — an unread registry is empty for the same "+
			"reason an empty one is, and calling that 'none' invents an outage", state, ModelStateUnknown)
	}
	// THE GAUGE MUST EXIST BEFORE THE TRANSPORT WORKS. A posture that only appears
	// once the subscription arms cannot report the subscription being broken.
	if got := modelPrimaryValue(t, reg, ModelStateUnknown); got != 1 {
		t.Fatalf("kanz_prediction_model_primary{state=unknown} = %v, want 1", got)
	}
}

func TestModelRegistry_NoneWhenTheLogIsFoldedAndEmpty(t *testing.T) {
	m, reg := newTestModelRegistry(t, &stubLog{})
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, state := m.PrimaryFor(FeatureSetPortfolioRisk); state != ModelStateNone {
		t.Fatalf("state = %q, want %q — every FeatureVector on this contract is being published "+
			"for nobody and nothing else in the estate says so", state, ModelStateNone)
	}
	if got := modelPrimaryValue(t, reg, ModelStateNone); got != 1 {
		t.Fatalf("kanz_prediction_model_primary{state=none} = %v, want 1", got)
	}
	if got := modelPrimaryValue(t, reg, ModelStateUnknown); got != 0 {
		t.Fatalf("unknown still reads %v after the fold armed", got)
	}
}

func TestModelRegistry_ServingNamesTheModelResolvedByContract(t *testing.T) {
	m, reg := newTestModelRegistry(t, &stubLog{
		payloads: promoted(t, "gbm-3", string(FeatureSetPortfolioRisk), time.Now().Add(24*time.Hour)),
	})
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	meta, state := m.PrimaryFor(FeatureSetPortfolioRisk)
	if state != ModelStateServing || meta.ModelID != "gbm-3" {
		t.Fatalf("state=%q model=%q, want serving/gbm-3", state, meta.ModelID)
	}
	if got := modelPrimaryValue(t, reg, ModelStateServing); got != 1 {
		t.Fatalf("kanz_prediction_model_primary{state=serving} = %v, want 1", got)
	}
}

func TestModelRegistry_AModelWhoseValidationLapsedIsNotReportedAsServing(t *testing.T) {
	m, _ := newTestModelRegistry(t, &stubLog{
		payloads: promoted(t, "gbm-3", string(FeatureSetPortfolioRisk), time.Now().Add(-time.Hour)),
	})
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	meta, state := m.PrimaryFor(FeatureSetPortfolioRisk)
	if state != ModelStateExpired {
		t.Fatalf("state = %q, want %q — the model IS still serving and SR 11-7 is about CURRENT "+
			"validation, so an expired one reported as 'serving' is the finding going missing",
			state, ModelStateExpired)
	}
	if meta.ModelID != "gbm-3" {
		t.Fatalf("expired state lost the model id: %q", meta.ModelID)
	}
}

func TestModelRegistry_AModelForAnotherContractDoesNotServeThisOne(t *testing.T) {
	m, _ := newTestModelRegistry(t, &stubLog{
		payloads: promoted(t, "credit-1", "counterparty-credit:1", time.Time{}),
	})
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, state := m.PrimaryFor(FeatureSetPortfolioRisk); state != ModelStateNone {
		t.Fatalf("state = %q, want none — resolution is BY CONTRACT and never by name, so a model "+
			"trained on another feature set must not be handed this one's vectors", state)
	}
}

func TestModelRegistry_AnUnfoldableEntryIsCounted(t *testing.T) {
	payloads := append([][]byte{{0x00, 0x01, 0x02, 0xff}},
		promoted(t, "gbm-3", string(FeatureSetPortfolioRisk), time.Time{})...)
	m, reg := newTestModelRegistry(t, &stubLog{payloads: payloads})
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	const want = `
# HELP kanz_prediction_registry_fold_errors_total Entries of the platform.model.registered log this replica could not apply, by reason. Each one leaves the local registry missing a mutation the rest of the fleet has, so the posture beside it may name the wrong model or no model at all. decode = the payload is not a ModelRegistryEvent; unknown_op = this build has no fold for an op the registrar published (it is BEHIND the schema); rejected = the registry refused it, which for a promotion means the MLOPS-01a validation it depends on was not in the replayed window (#112).
# TYPE kanz_prediction_registry_fold_errors_total counter
kanz_prediction_registry_fold_errors_total{reason="decode"} 1
kanz_prediction_registry_fold_errors_total{reason="rejected"} 0
kanz_prediction_registry_fold_errors_total{reason="unknown_op"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"kanz_prediction_registry_fold_errors_total"); err != nil {
		t.Fatalf("fold-error counter: %v", err)
	}
	// The tail was still folded — the whole reason the handler does not nack.
	if _, state := m.PrimaryFor(FeatureSetPortfolioRisk); state != ModelStateServing {
		t.Fatalf("state = %q after one unreadable entry, want serving", state)
	}
}

func TestModelRegistry_RefusesToReportOnNothing(t *testing.T) {
	if _, err := NewModelRegistry(prometheus.NewRegistry(), slog.Default(), &stubLog{}, "o"); err == nil {
		t.Fatal("NewModelRegistry accepted zero contracts — a posture with no subject is a gauge " +
			"that always reads healthy")
	}
	if _, err := NewModelRegistry(prometheus.NewRegistry(), slog.Default(), nil, "o", FeatureSetPortfolioRisk); err == nil {
		t.Fatal("NewModelRegistry accepted a nil subscriber")
	}
}

// modelPrimaryValue reads kanz_prediction_model_primary for one state label. The
// collector emits a series for EVERY state on purpose, so testutil.ToFloat64 —
// which insists on exactly one metric — cannot be used, and that is the design
// working rather than an inconvenience: a gauge that emitted only the current
// state would leave the previous one as a stale series nothing ever zeroes.
func modelPrimaryValue(t *testing.T, reg *prometheus.Registry, state string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "kanz_prediction_model_primary" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var gotState, gotRef string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "state":
					gotState = l.GetValue()
				case "feature_set_ref":
					gotRef = l.GetValue()
				}
			}
			if gotState == state && gotRef == string(FeatureSetPortfolioRisk) {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("no kanz_prediction_model_primary series for state=%q — every state must carry a "+
		"series including the zeroes, or an alert cannot fire on the transition into one", state)
	return 0
}
