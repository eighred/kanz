package integrity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
	"github.com/kanz-eng/kanz/pkg/bus"
)

var pubClock = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

// captureClient records every Publish, mirroring the EVT-17 / RISK-10 tests.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func newQualityPublisher(t *testing.T) (*integrity.Publisher, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "data-integrity/test",
		ProducerVersion: "data-1.0.0",
		Tenant:          "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := integrity.NewPublisherWithClock(prod, func() time.Time { return pubClock })
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	return pub, cc
}

// unframe decodes the single captured message into its envelope + DQE.
func unframe(t *testing.T, cc *captureClient) (*envelopepb.Envelope, *observationpb.DataQualityEvent) {
	t.Helper()
	if len(cc.sent) != 1 {
		t.Fatalf("captured %d messages want 1", len(cc.sent))
	}
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(cc.sent[0].Body, &frame); err != nil {
		t.Fatalf("frame unmarshal: %v", err)
	}
	if err := bus.Validate(frame.Envelope); err != nil {
		t.Fatalf("envelope failed Validate: %v", err)
	}
	var dqe observationpb.DataQualityEvent
	if err := proto.Unmarshal(frame.Payload, &dqe); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	return frame.Envelope, &dqe
}

func TestNewPublisher_NilProducerRejected(t *testing.T) {
	if _, err := integrity.NewPublisher(nil); err == nil {
		t.Fatal("expected error for nil producer")
	}
}

func TestEmitGap_PublishesValidEvent(t *testing.T) {
	pub, cc := newQualityPublisher(t)
	res := integrity.GapResult{
		Key:         integrity.StreamKey{Source: "market-ingest/pod-7", EventType: "market.equity.trade", PartitionKey: "AAPL"},
		Status:      integrity.StatusGap,
		Sequence:    10,
		Previous:    6,
		MissingFrom: 7,
		MissingTo:   9,
	}
	if err := pub.EmitGap(context.Background(), "market_stream", res, observationpb.Severity_SEVERITY_CRITICAL); err != nil {
		t.Fatalf("EmitGap: %v", err)
	}

	env, dqe := unframe(t, cc)
	if env.EventType != "data.market_stream.gap_detected" {
		t.Errorf("EventType=%q", env.EventType)
	}
	if env.Domain != "data" || env.EventClass != envelopepb.EventClass_EVENT_CLASS_OBSERVATION {
		t.Errorf("Domain=%q EventClass=%v", env.Domain, env.EventClass)
	}
	if env.PartitionKey != "AAPL" {
		t.Errorf("envelope PartitionKey=%q want AAPL", env.PartitionKey)
	}
	if !env.EventTime.AsTime().Equal(pubClock) {
		t.Errorf("EventTime=%v want %v (detection clock)", env.EventTime.AsTime(), pubClock)
	}
	if dqe.Subject != "market.equity.trade" || dqe.PartitionKey != "AAPL" {
		t.Errorf("DQE subject=%q pk=%q", dqe.Subject, dqe.PartitionKey)
	}
	if dqe.Severity != observationpb.Severity_SEVERITY_CRITICAL {
		t.Errorf("Severity=%v", dqe.Severity)
	}
	gap := dqe.GetGap()
	if gap == nil {
		t.Fatal("detail is not GapDetail")
	}
	if gap.ProducerSource != "market-ingest/pod-7" || gap.ExpectedSequence != 7 ||
		gap.ReceivedSequence != 10 || gap.MissingCount != 3 {
		t.Errorf("GapDetail=%+v", gap)
	}
}

func TestEmitGap_RejectsNonGapStatus(t *testing.T) {
	pub, cc := newQualityPublisher(t)
	res := integrity.GapResult{Status: integrity.StatusOK}
	if err := pub.EmitGap(context.Background(), "market_stream", res, observationpb.Severity_SEVERITY_WARNING); err == nil {
		t.Error("expected error for non-gap status")
	}
	if len(cc.sent) != 0 {
		t.Errorf("published %d messages on rejection", len(cc.sent))
	}
}

func TestEmitStaleness_PublishesValidEvent(t *testing.T) {
	pub, cc := newQualityPublisher(t)
	last := pubClock.Add(-90 * time.Second)
	res := integrity.StalenessResult{
		Subject:       "market.equity.quote",
		PartitionKey:  "MSFT",
		Lag:           90 * time.Second,
		Level:         integrity.StalenessCritical,
		Threshold:     60 * time.Second,
		LastEventTime: last,
	}
	if err := pub.EmitStaleness(context.Background(), "market_stream", res, observationpb.Severity_SEVERITY_CRITICAL); err != nil {
		t.Fatalf("EmitStaleness: %v", err)
	}

	env, dqe := unframe(t, cc)
	if env.EventType != "data.market_stream.staleness_detected" {
		t.Errorf("EventType=%q", env.EventType)
	}
	st := dqe.GetStaleness()
	if st == nil {
		t.Fatal("detail is not StalenessDetail")
	}
	if st.ObservedLag.AsDuration() != 90*time.Second || st.Threshold.AsDuration() != 60*time.Second {
		t.Errorf("lag=%v threshold=%v", st.ObservedLag.AsDuration(), st.Threshold.AsDuration())
	}
	if !st.LastEventTime.AsTime().Equal(last) {
		t.Errorf("LastEventTime=%v want %v", st.LastEventTime.AsTime(), last)
	}
}

func TestEmitStaleness_RejectsFresh(t *testing.T) {
	pub, _ := newQualityPublisher(t)
	res := integrity.StalenessResult{Level: integrity.StalenessFresh}
	if err := pub.EmitStaleness(context.Background(), "market_stream", res, observationpb.Severity_SEVERITY_WARNING); err == nil {
		t.Error("expected error for fresh result")
	}
}

func TestEmitDrift_PublishesValidEvent(t *testing.T) {
	pub, cc := newQualityPublisher(t)
	ws := pubClock.Add(-1 * time.Hour)
	res := integrity.DriftResult{
		Feature:     "vol_5m",
		Metric:      integrity.MetricPSI,
		Score:       0.42,
		Threshold:   0.25,
		Drifted:     true,
		Sufficient:  true,
		WindowStart: ws,
		WindowEnd:   pubClock,
	}
	if err := pub.EmitDrift(context.Background(), "feature", res, observationpb.Severity_SEVERITY_WARNING); err != nil {
		t.Fatalf("EmitDrift: %v", err)
	}

	env, dqe := unframe(t, cc)
	if env.EventType != "data.feature.drift_detected" {
		t.Errorf("EventType=%q", env.EventType)
	}
	// Drift is window-wide: no partition_key on the envelope (so the
	// auto-stamped producer_sequence must be 0 — Validate enforces it).
	if env.PartitionKey != "" {
		t.Errorf("envelope PartitionKey=%q want empty for window-wide drift", env.PartitionKey)
	}
	if dqe.Subject != "vol_5m" || dqe.PartitionKey != "" {
		t.Errorf("DQE subject=%q pk=%q", dqe.Subject, dqe.PartitionKey)
	}
	dr := dqe.GetDrift()
	if dr == nil {
		t.Fatal("detail is not DriftDetail")
	}
	if dr.Feature != "vol_5m" || dr.Metric != "psi" || dr.Score != 0.42 || dr.Threshold != 0.25 {
		t.Errorf("DriftDetail=%+v", dr)
	}
	if !dr.WindowStart.AsTime().Equal(ws) || !dr.WindowEnd.AsTime().Equal(pubClock) {
		t.Errorf("window=[%v,%v]", dr.WindowStart.AsTime(), dr.WindowEnd.AsTime())
	}
}

func TestEmitDrift_RejectsNotDrifted(t *testing.T) {
	pub, _ := newQualityPublisher(t)
	res := integrity.DriftResult{Feature: "vol_5m", Metric: integrity.MetricPSI, Drifted: false}
	if err := pub.EmitDrift(context.Background(), "feature", res, observationpb.Severity_SEVERITY_WARNING); err == nil {
		t.Error("expected error for non-drifted result")
	}
}

func TestEmit_RejectsEmptyEntityAndUnspecifiedSeverity(t *testing.T) {
	pub, cc := newQualityPublisher(t)
	gap := integrity.GapResult{Status: integrity.StatusGap, Sequence: 3, MissingFrom: 2, MissingTo: 2}

	if err := pub.EmitGap(context.Background(), "", gap, observationpb.Severity_SEVERITY_WARNING); err == nil {
		t.Error("expected error for empty entity")
	}
	if err := pub.EmitGap(context.Background(), "market_stream", gap, observationpb.Severity_SEVERITY_UNSPECIFIED); err == nil {
		t.Error("expected error for unspecified severity")
	}
	if len(cc.sent) != 0 {
		t.Errorf("published %d messages on rejection", len(cc.sent))
	}
}
