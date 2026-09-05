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

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/pkg/bus"
)

// Event-type names follow kanz-schemas/README.md § Subject Taxonomy §1
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
	producer      *bus.Producer
	onMeasureFrom func(measure, method string)
}

// Option customizes a Publisher.
type Option func(*Publisher)

// WithMeasureMethodObserver is called once per measure on every measures FACT,
// with the model that measure declared ("" when it declared none).
//
// IT OBSERVES WHAT WAS ANNOUNCED, NOT WHAT WAS REGISTERED, and the difference is
// the whole point (#1037). A registry probe would have to EXECUTE each measure
// against a book to learn its model — which fits a factor model and moves the
// skip counters at boot — and would still only describe the registry rather than
// the numbers the OMS gate actually folds. This subject is what gates order
// admission, so this is where the question is answered.
func WithMeasureMethodObserver(fn func(measure, method string)) Option {
	return func(p *Publisher) { p.onMeasureFrom = fn }
}

// NewPublisher returns a Publisher around the given bus.Producer.
// Producer must already be initialized with the engine's source +
// producer_version (ProducerConfig at EVT-17b).
func NewPublisher(producer *bus.Producer, opts ...Option) (*Publisher, error) {
	if producer == nil {
		return nil, errors.New("publish: producer is nil")
	}
	p := &Publisher{producer: producer}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// EmitExposure publishes an ExposureSet FACT for the portfolio.
// The bus.Producer auto-stamps envelope identity + lineage from
// ctx; EmitExposure fills the type-specific envelope fields and
// translates domain → proto.
func (p *Publisher) EmitExposure(ctx context.Context, exposure *domain.ExposureSet) error {
	if exposure == nil {
		return errors.New("publish: exposure is nil")
	}
	payload := ToProtoExposureSet(exposure)
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
	payload := ToProtoMeasureSet(measures, sourceEventIDs)
	if p.onMeasureFrom != nil {
		for _, m := range payload.GetMeasures() {
			p.onMeasureFrom(m.GetName(), m.GetProvenance().GetMethod())
		}
	}
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
		QualityFlags:     measureQualityFlags(measures),
	})
}

// measureQualityFlags stamps the envelope's data-integrity flags for a
// measures FACT.
//
// domain.v1.RiskMeasureSet has no SET-level field for "this is partial",
// so a partial measure set would otherwise reach every bus consumer —
// the compliance monitor, the archiver, the web app — as a
// complete-looking number (#257). envelope.v1.QUALITY_FLAG_DEGRADED is
// defined as "produced from a degraded or PARTIAL source", which is
// exactly this case. A consumer that gates on risk numbers must check
// envelope quality_flags, not just the payload.
//
// CORRECTED 2026-08-19 (#509): this doc used to say the signal rides the
// envelope "rather than waiting on a payload schema change", and read as
// though the payload carried no coverage at all. Since that change
// landed, domain.v1.RiskMeasure.coverage carries the PER-MEASURE record
// — which measure was computed over nothing, how much it lost, and a
// bounded sample of what. The envelope flag is not redundant: it is the
// one bit a consumer can route on without decoding the payload, and it
// is also what covers the currency exclusions, which are a property of
// the SET and still have no payload field.
//
// BOTH COVERAGE RECORDS FEED IT, and the bus cannot tell them apart. The
// query surface carries CURRENCY_EXCLUDED and INPUTS_UNRESOLVED as
// separate api/v1 flags because a caller can act on the difference; the
// envelope enum has one value for "partial source", so both collapse into
// it here. That is a genuine loss of resolution and it is recorded rather
// than hidden: a bus consumer learns THAT the set is partial and must
// re-query to learn why. Widening it means adding an envelope.v1 value,
// which is a schema change for every domain, not just risk.
//
// THIS FUNCTION IS THE SECOND PLACE THAT DECIDES "IS THIS RESPONSE
// PARTIAL", the first being engine.withCoverageFlags, and it re-derives
// the answer rather than reading v1.QualityFlags off a response — because
// the publish path has no response, only the set. The two must agree; the
// coupling is that both ask the SAME MeasureSet the same two questions,
// so a third coverage record added to the set without a case here is the
// drift to watch for.
func measureQualityFlags(measures *domain.MeasureSet) []envelopepb.QualityFlag {
	if len(measures.CurrencyExclusions()) == 0 && len(measures.UnresolvedMeasures()) == 0 {
		return nil
	}
	return []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED}
}

// --- Translation: domain → proto --------------------------------------
//
// ToProtoExposureSet / ToProtoMeasureSet are exported so the risk query
// gRPC server (API-01b, services/risk-engine/internal/grpcsrv) serves the
// EXACT same wire payloads this publisher emits — one domain→proto mapping,
// not two divergent copies. They live here (the publish layer that owns the
// domain↔proto boundary) rather than being re-derived in grpcsrv.

func ToProtoExposureSet(es *domain.ExposureSet) *domainpb.ExposureSet {
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

func ToProtoMeasureSet(ms *domain.MeasureSet, sourceEventIDs []string) *domainpb.RiskMeasureSet {
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
			Coverage:       toProtoInputCoverage(m.Coverage),
			Provenance:     toProtoProvenance(m.Provenance),
		})
	}
	return &domainpb.RiskMeasureSet{
		PortfolioId: string(ms.PortfolioID()),
		Measures:    measures,
		AsOf:        timestamppb.New(ms.AsOf()),
	}
}

// toProtoInputCoverage carries one measure's coverage record onto the
// wire, or nil when the measure does not report one.
//
// THE NIL IS THE WHOLE POINT, and it is why domain.v1.InputCoverage is
// a message rather than two scalars on RiskMeasure. v1.InputCoverage's
// zero value means "this measure does not report input coverage", NOT
// "everything resolved" — GrossExposure, NetExposure and HHI read the
// portfolio directly and have no provider that could decline. Emitting
// a present-but-empty message for those would tell a caller they were
// checked and found complete, which is a stronger claim than the
// engine makes; a scalar pair could not tell the two apart at all.
//
// An empty portfolio therefore reports absent rather than covered:
// nothing was assessed, and saying "covered everything" of a book with
// no positions is the flattering reading of the same silence.
func toProtoInputCoverage(c v1.InputCoverage) *domainpb.InputCoverage {
	if c.Contributed == 0 && c.ExcludedCount == 0 && len(c.Exclusions) == 0 {
		return nil
	}
	var exclusions []*domainpb.InputExclusion
	for _, e := range c.Exclusions {
		exclusions = append(exclusions, &domainpb.InputExclusion{
			InstrumentId: string(e.InstrumentID),
			Reason:       e.Reason,
		})
	}
	return &domainpb.InputCoverage{
		Contributed:   uint32(c.Contributed),
		ExcludedCount: uint32(c.ExcludedCount),
		Exclusions:    exclusions,
	}
}

// toProtoProvenance carries the model behind one measure onto the wire,
// or nil when the producer declared none.
//
// THE NIL IS LOAD-BEARING FOR THE SAME REASON IT IS ON COVERAGE, and it
// is why domain.v1.MeasureProvenance is a message. Absent means "this
// producer declares no model" — every measure this platform published
// before #1037 is in that state, so the OMS gate must fold an
// undeclared measure rather than refuse it. A present-but-empty message
// would look like a declaration and would turn every legacy measure
// into a method the gate cannot classify.
//
// THE METHOD IS THE ONLY REQUIRED FIELD. A provenance with an empty
// method says less than no provenance at all, because it looks like a
// declaration; it is dropped rather than published.
func toProtoProvenance(p v1.MeasureProvenance) *domainpb.MeasureProvenance {
	if !p.Declared() {
		return nil
	}
	out := &domainpb.MeasureProvenance{
		Method:  string(p.Method),
		ModelId: p.ModelID,
		Params:  p.Params,
	}
	if !p.ModelAsOf.IsZero() {
		out.ModelAsOf = timestamppb.New(p.ModelAsOf)
	}
	return out
}

// toProtoDimension maps the closed domain enum to the proto enum. Domain values
// without a wire counterpart map to UNSPECIFIED so a future domain-side
// addition that hasn't yet propagated to the schema still publishes (with
// consumers seeing the unknown bucket as UNSPECIFIED per kanz-schemas/README.md
// § Event Class Rules §3 enum-unknown semantics).
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
