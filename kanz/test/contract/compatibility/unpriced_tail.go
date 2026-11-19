package compatibility

import (
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// UnpricedTailVaR is the risk FACT payload shared by the publisher and OMS
// contract tests. The required zero value and unknown excluded gross must
// survive serialization together with the reasons that prevent admission.
func UnpricedTailVaR() *domainpb.RiskMeasureSet {
	return &domainpb.RiskMeasureSet{
		PortfolioId: "P1",
		AsOf:        timestamppb.New(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)),
		Measures: []*domainpb.RiskMeasure{{
			Name:           "VaR99",
			Value:          &commonpb.Decimal{},
			SourceEventIds: []string{"portfolio-revalued"},
			Coverage: &domainpb.InputCoverage{
				ExcludedCount: 2,
				Exclusions: []*domainpb.InputExclusion{
					{InstrumentId: "A", Reason: "no_market_value"},
					{Reason: "excluded_exposure_unrepresentable"},
				},
			},
			Provenance: &domainpb.MeasureProvenance{
				Method: "historical_simulation",
				Params: map[string]string{
					"confidence":                "0.99",
					"panel_calendar":            "continuous_utc_24h",
					"panel_contiguous":          "false",
					"panel_dropped_intervals":   "0",
					"panel_missing_instruments": "0",
					"panel_policy":              "exact_interval_intersection_v1",
					"panel_price_kind":          "0",
					"panel_return_method":       "",
					"panel_scenarios":           "0",
				},
			},
		}},
	}
}
