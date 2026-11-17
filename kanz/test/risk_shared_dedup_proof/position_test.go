// Package riskdedupproof checks the observable state of a disposable two-shard risk deployment.
package riskdedupproof

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/risk/ingest"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

const (
	tenant     = "dedup-tenant"
	portfolio  = "dedup-portfolio"
	instrument = "dedup-instrument"
)

type observation struct {
	id                           string
	at                           time.Time
	quantity, price, marketValue int64
}

// Prices and market values are cents. The market-value references are specified
// independently so an incorrect quantity, price or sign cannot validate itself.
var positions = []observation{
	{"018fd000-0000-7000-8000-000000000001", time.Unix(1717416000, 0).UTC(), -3, 1250, -3750},
	{"018fd000-0000-7000-8000-000000000002", time.Unix(1717416001, 0).UTC(), 7, 425, 2975},
	{"018fd000-0000-7000-8000-000000000003", time.Unix(1717416002, 0).UTC(), 2, 1175, 2350},
}

func cents(value int64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: value, Exponent: -2}
}

func (o observation) event() bus.Event {
	return bus.Event{
		EventID: o.id, TenantID: tenant, Subject: subject.PositionFor(tenant, portfolio, instrument),
		EventType: ingest.EventTypePositionChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: "risk", EventTime: o.at, PartitionKey: portfolio, PayloadSchemaRef: "domain.v1.PositionState:1",
		Payload: &domainpb.PositionState{
			PortfolioId: portfolio, InstrumentId: instrument, AsOf: timestamppb.New(o.at),
			Quantity: &commonpb.Decimal{Coefficient: o.quantity}, AveragePrice: cents(o.price),
			MarketValue: &commonpb.Money{Amount: cents(o.marketValue), CurrencyCode: "USD"},
		},
	}
}

func matchesUSD(m *commonpb.Money, expected int64) bool {
	return m.GetCurrencyCode() == "USD" && m.GetAmount() != nil && dec.InDomain(m.GetAmount()) && dec.Cmp(m.Amount, cents(expected)) == 0
}

func (o observation) checkExposure(set *domainpb.ExposureSet) error {
	if set.GetPortfolioId() != portfolio || !set.GetAsOf().AsTime().Equal(o.at) {
		return fmt.Errorf("portfolio or exposure time mismatch")
	}
	wantGross := o.marketValue
	if wantGross < 0 {
		wantGross = -wantGross
	}
	expected := map[domainpb.ExposureDimension]string{
		domainpb.ExposureDimension_EXPOSURE_DIMENSION_INSTRUMENT: instrument,
		domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY:   "USD",
	}
	for _, row := range set.GetExposures() {
		dimension := row.GetDimension()
		if dimension != domainpb.ExposureDimension_EXPOSURE_DIMENSION_INSTRUMENT && dimension != domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY {
			continue
		}
		bucket, exists := expected[dimension]
		if !exists || row.GetBucket() != bucket || row.GetPortfolioId() != portfolio || !row.GetAsOf().AsTime().Equal(o.at) ||
			!matchesUSD(row.GetGross(), wantGross) || !matchesUSD(row.GetNet(), o.marketValue) {
			return fmt.Errorf("unexpected exposure in dimension %s", dimension)
		}
		delete(expected, dimension)
	}
	if len(expected) != 0 {
		return fmt.Errorf("missing exposure dimensions")
	}
	return nil
}

func TestProofFinancialReference(t *testing.T) {
	for _, value := range positions {
		if got := value.event().Payload.(*domainpb.PositionState); got.GetQuantity().GetCoefficient()*got.GetAveragePrice().GetCoefficient() != value.marketValue {
			t.Fatalf("fixture %s disagrees with reference", value.id)
		}
	}
	o := positions[0]
	first := &domainpb.ExposureState{PortfolioId: portfolio, AsOf: timestamppb.New(o.at), Dimension: domainpb.ExposureDimension_EXPOSURE_DIMENSION_INSTRUMENT,
		Bucket: instrument, Gross: &commonpb.Money{CurrencyCode: "USD", Amount: cents(3750)}, Net: &commonpb.Money{CurrencyCode: "USD", Amount: cents(-3750)}}
	second := proto.CloneOf(first)
	second.Dimension, second.Bucket = domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY, "USD"
	set := &domainpb.ExposureSet{PortfolioId: portfolio, AsOf: timestamppb.New(o.at), Exposures: []*domainpb.ExposureState{first, second}}
	if err := o.checkExposure(set); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*domainpb.ExposureSet){
		"missing currency":  func(s *domainpb.ExposureSet) { s.Exposures = s.Exposures[:1] },
		"duplicate leaf":    func(s *domainpb.ExposureSet) { s.Exposures = append(s.Exposures, s.Exposures[0]) },
		"wrong sign":        func(s *domainpb.ExposureSet) { s.Exposures[0].Net.Amount = cents(3750) },
		"wrong currency":    func(s *domainpb.ExposureSet) { s.Exposures[1].Net.CurrencyCode = "EUR" },
		"missing gross":     func(s *domainpb.ExposureSet) { s.Exposures[1].Gross = nil },
		"stale observation": func(s *domainpb.ExposureSet) { s.AsOf = timestamppb.New(o.at.Add(-time.Second)) },
	} {
		t.Run(name, func(t *testing.T) {
			copyOfSet := proto.CloneOf(set)
			change(copyOfSet)
			if o.checkExposure(copyOfSet) == nil {
				t.Fatal("invalid financial output accepted")
			}
		})
	}
}
