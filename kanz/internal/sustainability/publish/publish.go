// Package publish records filing-request ESG/carbon observations. It is not the
// vendor feed and confers no reference-data or financial admission authority.
package publish

import (
	"errors"
	"fmt"
	"math"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/sustainability"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	sustainabilitypb "github.com/eighred/kanz/kanz-schemas-go/sustainability/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	ObservedSubject   = "risk.sustainability.observed"
	AggregatedSubject = "risk.sustainability.aggregated"
	MaxHoldings       = 2048
)

// Input retains exact money from validated request tokens, before float64 is
// used at the existing analytics edge. Absence of ESG is not a measured zero.
type Input struct {
	Holding                          sustainability.Holding
	MarketValue, Revenue, Enterprise *commonpb.Decimal
	ESGPresent                       bool
}

type NewID func() (string, error)

// Build uses the existing calculations and their returned coverage. Every
// instrument/type and the aggregate receives its own persisted UUIDv7.
func Build(scope filing.PublicationScope, inputs []Input, next NewID) ([]*sustainabilitypb.InstrumentObservation, *sustainabilitypb.PortfolioObservation, error) {
	if err := scope.Validate(); err != nil {
		return nil, nil, err
	}
	if next == nil || len(inputs) > MaxHoldings {
		return nil, nil, errors.New("sustainability publication: invalid input inventory")
	}
	// Keep the actual computation's input order. Reordering floating model
	// sums here could produce a different aggregate from the filed calculation.
	seen := make(map[string]bool, len(inputs))
	ids := map[string]bool{scope.ReportID: true}
	allocate := func() (string, error) {
		id, err := next()
		if err != nil || !filing.UUIDv7(id) || ids[id] {
			return "", errors.New("sustainability publication: distinct UUIDv7 required")
		}
		ids[id] = true
		return id, nil
	}
	stamp := timestamppb.New(scope.AsOf.UTC())
	recorded := timestamppb.New(scope.RecordedAt.UTC())
	if stamp.CheckValid() != nil || recorded.CheckValid() != nil {
		return nil, nil, errors.New("sustainability publication: invalid as-of")
	}
	observations := make([]*sustainabilitypb.InstrumentObservation, 0, 2*len(inputs))
	holdings := make([]sustainability.Holding, 0, len(inputs))
	esgCount := 0
	for _, in := range inputs {
		h := in.Holding
		if !filing.BoundedIdentifier(h.InstrumentID) || seen[h.InstrumentID] || !finite(h.MarketValue, h.ESG.Overall, h.ESG.Environmental, h.ESG.Social, h.ESG.Governance, h.Carbon.Scope1, h.Carbon.Scope2, h.Carbon.Scope3, h.Carbon.Revenue, h.Carbon.EVIC) {
			return nil, nil, errors.New("sustainability publication: ambiguous or non-finite issuer input")
		}
		seen[h.InstrumentID] = true
		for i, money := range []*commonpb.Decimal{in.MarketValue, in.Revenue, in.Enterprise} {
			if money == nil && i > 0 && []float64{h.MarketValue, h.Carbon.Revenue, h.Carbon.EVIC}[i] == 0 {
				continue
			}
			r, ok := dec.FromProtoChecked(money)
			if money == nil || !ok {
				return nil, nil, errors.New("sustainability publication: exact source money required")
			}
			f, _ := r.Float64()
			if f != []float64{h.MarketValue, h.Carbon.Revenue, h.Carbon.EVIC}[i] || (i > 0 && r.Sign() < 0) {
				return nil, nil, errors.New("sustainability publication: source money does not match the computed input")
			}
		}
		makeObservation := func() (*sustainabilitypb.InstrumentObservation, error) {
			id, err := allocate()
			if err != nil {
				return nil, err
			}
			return &sustainabilitypb.InstrumentObservation{ObservationId: id, ReportId: scope.ReportID, TenantId: scope.Tenant, PortfolioId: scope.Portfolio, AsOf: proto.Clone(stamp).(*timestamppb.Timestamp), RecordedAt: proto.Clone(recorded).(*timestamppb.Timestamp), Origin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifestDigest: scope.InputDigest}, nil
		}
		if in.ESGPresent {
			esgCount++
			o, err := makeObservation()
			if err != nil {
				return nil, nil, err
			}
			o.Datum = &sustainabilitypb.InstrumentObservation_Esg{Esg: &sustainabilitypb.ESGScore{InstrumentId: h.InstrumentID, Overall: h.ESG.Overall, Environmental: h.ESG.Environmental, Social: h.ESG.Social, Governance: h.ESG.Governance}}
			observations = append(observations, o)
		}
		o, err := makeObservation()
		if err != nil {
			return nil, nil, err
		}
		carbon := &sustainabilitypb.CarbonMetrics{InstrumentId: h.InstrumentID, Scope1: h.Carbon.Scope1, Scope2: h.Carbon.Scope2, Scope3: h.Carbon.Scope3}
		// Non-positive denominators are missing coverage in the existing model;
		// do not present its degraded value as an authoritative measured zero.
		if h.Carbon.Revenue > 0 {
			carbon.Revenue = proto.Clone(in.Revenue).(*commonpb.Decimal)
		}
		if h.Carbon.EVIC > 0 {
			carbon.EnterpriseValue = proto.Clone(in.Enterprise).(*commonpb.Decimal)
		}
		o.Datum = &sustainabilitypb.InstrumentObservation_Carbon{Carbon: carbon}
		observations = append(observations, o)
		holdings = append(holdings, h)
	}
	waci, intensity := sustainability.WeightedAverageCarbonIntensity(holdings)
	emissions, attribution := sustainability.FinancedEmissions(holdings)
	if !finite(waci, emissions, intensity.Fraction(), attribution.Fraction()) {
		return nil, nil, errors.New("sustainability publication: non-finite aggregate")
	}
	id, err := allocate()
	if err != nil {
		return nil, nil, err
	}
	aggregate := &sustainabilitypb.PortfolioObservation{ObservationId: id, ReportId: scope.ReportID, TenantId: scope.Tenant, PortfolioId: scope.Portfolio, AsOf: stamp, Origin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifestDigest: scope.InputDigest, WeightedAverageCarbonIntensity: waci, FinancedEmissions: emissions, IntensityCoverage: coverage(intensity), AttributionCoverage: coverage(attribution), TotalHoldings: uint32(len(holdings)), EsgObservedHoldings: uint32(esgCount)}
	aggregate.RecordedAt = recorded
	if len(holdings) > 0 && esgCount == len(holdings) {
		esg := sustainability.WeightedAverageESG(holdings)
		if !finite(esg.Overall, esg.Environmental, esg.Social, esg.Governance) {
			return nil, nil, errors.New("sustainability publication: non-finite ESG aggregate")
		}
		aggregate.Esg = &sustainabilitypb.ESGAggregate{Overall: esg.Overall, Environmental: esg.Environmental, Social: esg.Social, Governance: esg.Governance}
	}
	return observations, aggregate, nil
}

func coverage(c sustainability.Coverage) *sustainabilitypb.CarbonCoverage {
	return &sustainabilitypb.CarbonCoverage{Datum: c.Datum, Fraction: c.Fraction(), UncoveredCount: uint32(c.UncoveredCount), UncoveredSample: append([]string(nil), c.Uncovered...)}
}

func finite(values ...float64) bool {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

// Event restores the domain identity persisted with the artifact; retries do not
// allocate a second FACT identity. The canonical bus Producer still validates it.
func Event(payload proto.Message) (bus.Event, error) {
	var id, tenant, portfolio, reportID, digest, subject, binding string
	var at, recorded *timestamppb.Timestamp
	switch p := payload.(type) {
	case *sustainabilitypb.InstrumentObserved:
		o := p.GetObservation()
		if o == nil || o.Origin != sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED || (o.GetEsg() == nil && o.GetCarbon() == nil) {
			return bus.Event{}, errors.New("sustainability publication: missing observation")
		}
		if err := ValidateInstrument(o); err != nil {
			return bus.Event{}, err
		}
		id, tenant, portfolio, reportID, digest, at = o.ObservationId, o.TenantId, o.PortfolioId, o.ReportId, o.InputManifestDigest, o.AsOf
		recorded = o.RecordedAt
		subject, binding = ObservedSubject, p.ContextBindingHash
	case *sustainabilitypb.PortfolioAggregated:
		o := p.GetObservation()
		if o == nil || o.Origin != sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED {
			return bus.Event{}, errors.New("sustainability publication: missing aggregate")
		}
		if err := ValidateAggregate(o); err != nil {
			return bus.Event{}, err
		}
		id, tenant, portfolio, reportID, digest, at = o.ObservationId, o.TenantId, o.PortfolioId, o.ReportId, o.InputManifestDigest, o.AsOf
		recorded = o.RecordedAt
		subject, binding = AggregatedSubject, p.ContextBindingHash
	default:
		return bus.Event{}, fmt.Errorf("sustainability publication: unsupported payload %T", payload)
	}
	if at == nil || at.CheckValid() != nil || recorded == nil || recorded.CheckValid() != nil || !filing.UUIDv7(id) || id == reportID || !filing.Digest(binding) {
		return bus.Event{}, errors.New("sustainability publication: invalid persisted identity or binding")
	}
	if err := (filing.PublicationScope{Tenant: tenant, Portfolio: portfolio, ReportID: reportID, InputDigest: digest, AsOf: at.AsTime(), RecordedAt: recorded.AsTime()}).Validate(); err != nil {
		return bus.Event{}, err
	}
	return bus.Event{EventID: id, Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "risk", EventTime: recorded.AsTime(), PartitionKey: tenant + "/" + portfolio, CorrelationID: reportID, TenantID: tenant, Payload: payload}, nil
}

// ValidateInstrument checks the datum representation. Scope/identity/origin are
// independently checked by Event and the complete filing context validator.
func ValidateInstrument(o *sustainabilitypb.InstrumentObservation) error {
	if o == nil || (o.GetEsg() == nil && o.GetCarbon() == nil) {
		return errors.New("sustainability publication: missing instrument datum")
	}
	if esg := o.GetEsg(); esg != nil {
		if !filing.BoundedIdentifier(esg.InstrumentId) || !finite(esg.Overall, esg.Environmental, esg.Social, esg.Governance) {
			return errors.New("sustainability publication: invalid ESG observation")
		}
	}
	if carbon := o.GetCarbon(); carbon != nil {
		if !filing.BoundedIdentifier(carbon.InstrumentId) || !finite(carbon.Scope1, carbon.Scope2, carbon.Scope3) {
			return errors.New("sustainability publication: invalid carbon observation")
		}
		for _, money := range []*commonpb.Decimal{carbon.Revenue, carbon.EnterpriseValue} {
			if money == nil {
				continue
			}
			value, ok := dec.FromProtoChecked(money)
			if !ok || value.Sign() <= 0 {
				return errors.New("sustainability publication: invalid observed denominator")
			}
		}
	}
	return nil
}

// ValidateAggregate checks computed values and their explicit coverage record.
func ValidateAggregate(o *sustainabilitypb.PortfolioObservation) error {
	if o == nil {
		return errors.New("sustainability publication: missing aggregate")
	}
	if o.TotalHoldings > MaxHoldings || o.EsgObservedHoldings > o.TotalHoldings || !finite(o.WeightedAverageCarbonIntensity, o.FinancedEmissions) {
		return errors.New("sustainability publication: invalid aggregate")
	}
	for i, coverage := range []*sustainabilitypb.CarbonCoverage{o.IntensityCoverage, o.AttributionCoverage} {
		if coverage == nil || coverage.Datum != []string{"revenue", "evic"}[i] || !finite(coverage.Fraction) || coverage.Fraction < 0 || coverage.Fraction > 1 || coverage.UncoveredCount > o.TotalHoldings || len(coverage.UncoveredSample) > 32 || uint32(len(coverage.UncoveredSample)) > coverage.UncoveredCount {
			return errors.New("sustainability publication: invalid coverage")
		}
		for _, id := range coverage.UncoveredSample {
			if !filing.BoundedIdentifier(id) {
				return errors.New("sustainability publication: invalid uncovered identity")
			}
		}
	}
	if esg := o.Esg; esg != nil && (o.TotalHoldings == 0 || o.EsgObservedHoldings != o.TotalHoldings || !finite(esg.Overall, esg.Environmental, esg.Social, esg.Governance)) {
		return errors.New("sustainability publication: aggregate invents ESG coverage")
	}
	return nil
}
