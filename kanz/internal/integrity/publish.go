package integrity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// Publisher is the emit half of the data-integrity layer's detect-then-emit
// split (DATA-07): it turns a detector result (DATA-01 gap, DATA-02
// staleness, DATA-04 drift) into an observation.v1.DataQualityEvent and
// publishes it onto the bus. The detectors classify; the Publisher reports.
//
// # Event-type + subject
//
// Quality events flow on "data.{entity}.{kind}_detected" (subject-taxonomy
// §1: data.market_stream.gap_detected, data.feature.drift_detected). The
// {entity} segment classifies the monitored stream (its value is orthogonal
// to the kind — staleness can occur on a market_stream or a feature), so the
// caller supplies it per emit. The DataQualityEvent.subject FIELD names the
// upstream stream the problem was found on (e.g. "market.equity.trade"), NOT
// the data.* subject this report itself flows on.
//
// # FACT-grade OBSERVATION
//
// DataQualityEvent carries EVENT_CLASS_OBSERVATION but is the documented
// exception to observation lossy-tolerance (event-class-rules §4): it must
// not be shed and gets durable retention. The class is OBSERVATION; the
// "FACT-grade" property is a delivery/retention policy enforced by the bus
// topology (EVT-08/09), not the envelope class.
//
// # Shared Producer
//
// Like RISK-10, the Publisher wraps a bus.Producer rather than constructing
// one, so producer_sequence stays monotonic across the kinds it emits. The
// orchestrator owns the Producer (with the integrity service's source +
// version) and decides WHEN to emit (per detection, or batched) — DATA-07
// only owns the translation.
type Publisher struct {
	producer *bus.Producer
	now      func() time.Time
}

const (
	domainData               = "data"
	schemaRefDataQuality     = "observation.v1.DataQualityEvent:1"
	schemaVersionDataQuality = 1

	suffixGapDetected       = "gap_detected"
	suffixStalenessDetected = "staleness_detected"
	suffixDriftDetected     = "drift_detected"
)

// NewPublisher returns a Publisher around the given bus.Producer, using the
// wall clock to stamp each quality event's event_time (the detection time).
func NewPublisher(producer *bus.Producer) (*Publisher, error) {
	return NewPublisherWithClock(producer, time.Now)
}

// NewPublisherWithClock is NewPublisher with an injectable clock so emitted
// event_time is deterministic in tests (the package's clock-injection
// convention — cf. NewReconcilerWithClock). A nil now falls back to time.Now.
func NewPublisherWithClock(producer *bus.Producer, now func() time.Time) (*Publisher, error) {
	if producer == nil {
		return nil, errors.New("integrity: producer is nil")
	}
	if now == nil {
		now = time.Now
	}
	return &Publisher{producer: producer, now: now}, nil
}

// EmitGap publishes a DataQualityEvent for a detected sequence gap (DATA-01).
// res.Status must be StatusGap; any other status is a caller error (the
// Publisher reports detected problems, it does not re-detect). severity is
// triage policy (DATA-09) and must not be SEVERITY_UNSPECIFIED.
func (p *Publisher) EmitGap(ctx context.Context, entity string, res GapResult, severity observationpb.Severity) error {
	if res.Status != StatusGap {
		return fmt.Errorf("integrity: EmitGap requires StatusGap, got %s", res.Status)
	}
	if err := validateEmit(entity, severity); err != nil {
		return err
	}
	subject := res.Key.EventType
	pk := res.Key.PartitionKey
	dqe := &observationpb.DataQualityEvent{
		Subject:      subject,
		PartitionKey: pk,
		Severity:     severity,
		Summary: fmt.Sprintf("sequence gap on %s/%s: expected %d, received %d (%d missing)",
			subject, pk, res.MissingFrom, res.Sequence, res.MissingCount()),
		Detail: &observationpb.DataQualityEvent_Gap{Gap: &observationpb.GapDetail{
			ProducerSource:   res.Key.Source,
			ExpectedSequence: res.MissingFrom,
			ReceivedSequence: res.Sequence,
			MissingCount:     res.MissingCount(),
		}},
	}
	return p.emit(ctx, entity, suffixGapDetected, pk, dqe)
}

// EmitStaleness publishes a DataQualityEvent for excessive staleness
// (DATA-02). res must be a staleness alarm (Stale() — Stale or Critical).
func (p *Publisher) EmitStaleness(ctx context.Context, entity string, res StalenessResult, severity observationpb.Severity) error {
	if !res.Stale() {
		return fmt.Errorf("integrity: EmitStaleness requires a staleness alarm, got %s", res.Level)
	}
	if err := validateEmit(entity, severity); err != nil {
		return err
	}
	dqe := &observationpb.DataQualityEvent{
		Subject:      res.Subject,
		PartitionKey: res.PartitionKey,
		Severity:     severity,
		Summary: fmt.Sprintf("%s staleness on %s/%s: lag %s exceeds %s",
			res.Level, res.Subject, res.PartitionKey, res.Lag, res.Threshold),
		Detail: &observationpb.DataQualityEvent_Staleness{Staleness: &observationpb.StalenessDetail{
			ObservedLag:   durationpb.New(res.Lag),
			Threshold:     durationpb.New(res.Threshold),
			LastEventTime: timestamppb.New(res.LastEventTime),
		}},
	}
	return p.emit(ctx, entity, suffixStalenessDetected, res.PartitionKey, dqe)
}

// EmitDrift publishes a DataQualityEvent for input-distribution drift
// (DATA-04). res.Drifted must be true. Drift is per-feature and window-wide,
// so the DataQualityEvent.subject is the feature name and partition_key is
// empty (a stream-wide problem).
func (p *Publisher) EmitDrift(ctx context.Context, entity string, res DriftResult, severity observationpb.Severity) error {
	if !res.Drifted {
		return errors.New("integrity: EmitDrift requires res.Drifted")
	}
	if err := validateEmit(entity, severity); err != nil {
		return err
	}
	dqe := &observationpb.DataQualityEvent{
		Subject:  res.Feature,
		Severity: severity,
		Summary: fmt.Sprintf("%s drift on %s: score %.4f exceeds %.4f",
			res.Metric, res.Feature, res.Score, res.Threshold),
		Detail: &observationpb.DataQualityEvent_Drift{Drift: &observationpb.DriftDetail{
			Feature:     res.Feature,
			Metric:      string(res.Metric),
			Score:       res.Score,
			Threshold:   res.Threshold,
			WindowStart: timestamppb.New(res.WindowStart),
			WindowEnd:   timestamppb.New(res.WindowEnd),
		}},
	}
	return p.emit(ctx, entity, suffixDriftDetected, "", dqe)
}

func validateEmit(entity string, severity observationpb.Severity) error {
	if entity == "" {
		return errors.New("integrity: entity required for data.{entity}.* event_type")
	}
	if severity == observationpb.Severity_SEVERITY_UNSPECIFIED {
		return errors.New("integrity: severity must not be SEVERITY_UNSPECIFIED")
	}
	return nil
}

// emit stamps the common envelope fields and publishes. event_time is the
// detection time (now); the bus.Producer auto-stamps identity + lineage and,
// for the OBSERVATION class, idempotency_key = event_id.
func (p *Publisher) emit(ctx context.Context, entity, suffix, partitionKey string, dqe *observationpb.DataQualityEvent) error {
	eventType := fmt.Sprintf("%s.%s.%s", domainData, entity, suffix)
	return p.producer.Publish(ctx, bus.Event{
		Subject:          eventType,
		EventType:        eventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_OBSERVATION,
		SchemaVersion:    schemaVersionDataQuality,
		Domain:           domainData,
		EventTime:        p.now(),
		PartitionKey:     partitionKey,
		PayloadSchemaRef: schemaRefDataQuality,
		Payload:          dqe,
	})
}
