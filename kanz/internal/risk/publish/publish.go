// Package publish is the risk engine's bus publish layer (RISK-10).
// Translates domain values (RISK-03 in-memory shapes) into
// kanz-schemas/proto/domain/v1 wire shapes and emits them as FACT
// envelopes via the shared bus.Producer (EVT-17).
//
// # Two event types
//
//   - "risk.portfolio.exposure_recomputed" — payload
//     domain.v1.ExposureSet, emitted whenever RISK-06 recomputes
//     exposures for a portfolio.
//
//   - "risk.portfolio.measures_computed" — payload
//     domain.v1.RiskMeasureSet, emitted whenever RISK-07/08
//     recomputes measures for a portfolio. Carries source_event_ids
//     for lineage to the events that produced each measure
//     (envelope causation_id is single-valued and only meaningful
//     when there is one triggering event; aggregated measures
//     pulled from many inputs use the source_event_ids list).
//
// # Scope-split with RISK-04/05
//
// RISK-04's Ingestor handles bus → engine (proto→domain via the
// Applier interface). RISK-10's Publisher handles engine → bus
// (domain→proto via the translation helpers here). Together the
// two are the engine's bus boundary; they share the same
// bus.Producer pattern and the same wire-frame convention. RISK-10
// does NOT decide when to publish — that's the caller's
// orchestration concern (e.g. publish after each state apply, or
// on a scheduled cadence, or only on material change).
//
// # Shared Producer
//
// The Publisher takes a bus.Producer rather than constructing its
// own so producer_sequence stays monotonic across event types that
// share a (source, partition_key). The orchestrator creates one
// Producer per engine instance and hands it to Publisher (and any
// other publish layers).
package publish

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// Event-type names follow kanz-schemas/docs/subject-taxonomy.md §1
// ({domain}.{entity}.{event_type}, past-tense for FACTs). These are
// the values external consumers (downstream services, dashboards,
// audit pipeline) subscribe to.
const (
	EventTypeExposureRecomputed = "risk.portfolio.exposure_recomputed"
	EventTypeMeasuresComputed   = "risk.portfolio.measures_computed"
)

// Payload schema-refs registered with the schema registry (EVT-16).
const (
	schemaRefExposureSet  = "domain.v1.ExposureSet:1"
	schemaRefMeasureSet   = "domain.v1.RiskMeasureSet:1"
	schemaVersionExposure = 1
	schemaVersionMeasures = 1
)

// Publisher emits the risk engine's output FACTs onto the bus.
type Publisher struct {
	producer *bus.Producer
}

// NewPublisher returns a Publisher around the given bus.Producer.
// Producer must already be initialized with the engine's source +
// producer_version (ProducerConfig at EVT-17b).
func NewPublisher(producer *bus.Producer) (*Publisher, error) {
	if producer == nil {
		return nil, errors.New("publish: producer is nil")
	}
	return &Publisher{producer: producer}, nil
}

// EmitExposure publishes an ExposureSet FACT for the portfolio.
// The bus.Producer auto-stamps envelope identity + lineage from
// ctx; EmitExposure fills the type-specific envelope fields and
// translates domain → proto.
func (p *Publisher) EmitExposure(ctx context.Context, exposure *domain.ExposureSet) error {
	if exposure == nil {
		return errors.New("publish: exposure is nil")
	}
	payload := toProtoExposureSet(exposure)
	return p.producer.Publish(ctx, bus.Event{
		Subject:          EventTypeExposureRecomputed,
		EventType:        EventTypeExposureRecomputed,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersionExposure,
		Domain:           "risk",
		EventTime:        exposure.AsOf(),
		PartitionKey:     string(exposure.PortfolioID()),
		PayloadSchemaRef: schemaRefExposureSet,
		Payload:          payload,
	})
}

// EmitMeasures publishes a RiskMeasureSet FACT for the portfolio.
// sourceEventIDs are the event_ids of the inputs whose state
// produced these measures (typically the state events the engine
// had ingested at computation time) — propagated into the payload
// for replay + audit. May be nil/empty when lineage is implicit
// via envelope causation_id.
func (p *Publisher) EmitMeasures(ctx context.Context, measures *domain.MeasureSet, sourceEventIDs []string) error {
	if measures == nil {
		return errors.New("publish: measures is nil")
	}
	payload := toProtoMeasureSet(measures, sourceEventIDs)
	return p.producer.Publish(ctx, bus.Event{
		Subject:          EventTypeMeasuresComputed,
		EventType:        EventTypeMeasuresComputed,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersionMeasures,
		Domain:           "risk",
		EventTime:        measures.AsOf(),
		PartitionKey:     string(measures.PortfolioID()),
		PayloadSchemaRef: schemaRefMeasureSet,
		Payload:          payload,
	})
}

// --- Translation: domain → proto --------------------------------------

func toProtoExposureSet(es *domain.ExposureSet) *domainpb.ExposureSet {
	items := es.Items()
	exposures := make([]*domainpb.ExposureState, 0, len(items))
	asOf := timestamppb.New(es.AsOf())
	pid := string(es.PortfolioID())
	for _, e := range items {
		exposures = append(exposures, &domainpb.ExposureState{
			PortfolioId: pid,
			Dimension:   toProtoDimension(e.Dimension),
			Bucket:      e.Key,
			Gross:       e.Gross,
			Net:         e.Net,
			AsOf:        asOf,
		})
	}
	return &domainpb.ExposureSet{
		PortfolioId: pid,
		Exposures:   exposures,
		AsOf:        asOf,
	}
}

func toProtoMeasureSet(ms *domain.MeasureSet, sourceEventIDs []string) *domainpb.RiskMeasureSet {
	names := ms.Names()
	measures := make([]*domainpb.RiskMeasure, 0, len(names))
	for _, n := range names {
		m, ok := ms.Lookup(n)
		if !ok {
			continue
		}
		measures = append(measures, &domainpb.RiskMeasure{
			Name:           string(m.Name),
			Value:          m.Value,
			UncertaintyAbs: m.UncertaintyAbs,
			SourceEventIds: sourceEventIDs,
		})
	}
	return &domainpb.RiskMeasureSet{
		PortfolioId: string(ms.PortfolioID()),
		Measures:    measures,
		AsOf:        timestamppb.New(ms.AsOf()),
	}
}

// toProtoDimension maps the closed domain enum to the proto enum.
// Domain values without a wire counterpart map to UNSPECIFIED so a
// future domain-side addition that hasn't yet propagated to the
// schema still publishes (with consumers seeing the unknown bucket
// as UNSPECIFIED per event-class-rules.md §3 enum-unknown semantics).
func toProtoDimension(d domain.ExposureDimension) domainpb.ExposureDimension {
	switch d {
	case domain.ExposureByInstrument:
		return domainpb.ExposureDimension_EXPOSURE_DIMENSION_INSTRUMENT
	case domain.ExposureByCurrency:
		return domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY
	case domain.ExposureBySector:
		return domainpb.ExposureDimension_EXPOSURE_DIMENSION_SECTOR
	default:
		return domainpb.ExposureDimension_EXPOSURE_DIMENSION_UNSPECIFIED
	}
}

