package measureread

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

func flagProjection(family string, flags []querypb.QualityFlag) Set {
	measure := &domainpb.RiskMeasure{Name: "Risk", Value: d(125, -1), UncertaintyAbs: d(25, -2), Coverage: &domainpb.InputCoverage{Contributed: 2}}
	set := &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{measure}}
	switch family {
	case "measures":
		return ProjectMeasures(&querypb.MeasuresResponse{PortfolioId: "PF", Set: set, QualityFlags: flags})
	case "scenario":
		return ProjectScenario(&querypb.EvaluateScenarioResponse{PortfolioId: "PF", Projected: set, QualityFlags: flags})
	default:
		return ProjectExposure(&querypb.ExposureResponse{PortfolioId: "PF", QualityFlags: flags,
			Set: &domainpb.ExposureSet{Exposures: []*domainpb.ExposureState{{Dimension: domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY,
				Bucket: "USD", Net: &commonpb.Money{Amount: d(125, -1), CurrencyCode: "USD"}}}}})
	}
}

func assertFlagRefusal(t *testing.T, set Set, flags []querypb.QualityFlag, reason string) {
	t.Helper()
	wantFlags := make([]string, len(flags))
	for i, flag := range flags {
		wantFlags[i] = strings.TrimPrefix(flag.String(), "QUALITY_FLAG_")
	}
	if !reflect.DeepEqual(set.QualityFlags, wantFlags) || set.PortfolioID != "PF" || len(set.Measures) != 1 {
		t.Fatalf("projection lost response/flag identity: %+v want flags=%v", set, wantFlags)
	}
	m := set.Measures[0]
	if m.Status != StatusUnavailable || m.Value != nil || m.UncertaintyAbs != nil || !strings.Contains(m.Reason, reason) {
		t.Fatalf("unknown trust signal retained a claim: %+v", m)
	}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"value":`) || strings.Contains(string(body), `"uncertainty_abs":`) {
		t.Fatalf("agent JSON carries a withheld numeric claim: %s", body)
	}
}

func TestUnknownQualityFlagsWithholdEveryProjection(t *testing.T) {
	for _, family := range []string{"measures", "exposure", "scenario"} {
		t.Run(family, func(t *testing.T) {
			for _, unknown := range []querypb.QualityFlag{5, -1, 1<<31 - 1, -1 << 31} {
				t.Run(fmt.Sprint(int32(unknown)), func(t *testing.T) {
					for name, flags := range map[string][]querypb.QualityFlag{
						"alone":           {unknown},
						"after_advisory":  {querypb.QualityFlag_QUALITY_FLAG_DEGRADED, unknown},
						"before_advisory": {unknown, querypb.QualityFlag_QUALITY_FLAG_STALE, querypb.QualityFlag_QUALITY_FLAG_INPUTS_UNRESOLVED},
						"duplicate":       {unknown, unknown},
					} {
						t.Run(name, func(t *testing.T) {
							first := flagProjection(family, flags)
							assertFlagRefusal(t, first, flags, unknown.String())
							if repeated := flagProjection(family, flags); !reflect.DeepEqual(repeated, first) {
								t.Fatalf("same response changed on repeat: first=%+v repeated=%+v", first, repeated)
							}
						})
					}
				})
			}
		})
	}
}

func TestQualityFlagRefusalPrecedenceAndKnownRecovery(t *testing.T) {
	for _, family := range []string{"measures", "exposure", "scenario"} {
		t.Run(family, func(t *testing.T) {
			for name, flags := range map[string][]querypb.QualityFlag{
				"currency_first": {3, 5}, "currency_last": {5, 3},
				"currency_duplicate": {5, 3, 5, 3}, "currency_unspecified": {0, 5, 3},
			} {
				t.Run(name, func(t *testing.T) { assertFlagRefusal(t, flagProjection(family, flags), flags, "CURRENCY_EXCLUDED") })
			}
			t.Run("unspecified", func(t *testing.T) {
				assertFlagRefusal(t, flagProjection(family, []querypb.QualityFlag{0}), []querypb.QualityFlag{0}, "QUALITY_FLAG_UNSPECIFIED")
			})
			for name, flags := range map[string][]querypb.QualityFlag{
				"none": {}, "degraded": {1}, "stale": {2}, "inputs": {4}, "all_advisory": {1, 2, 4, 1},
			} {
				t.Run(name, func(t *testing.T) {
					assertFlagRefusal(t, flagProjection(family, []querypb.QualityFlag{5}), []querypb.QualityFlag{5}, "5")
					set := flagProjection(family, flags)
					m := set.Measures[0]
					if m.Status != StatusMeasured || m.Value == nil || *m.Value != 12.5 || m.Reason != "" {
						t.Fatalf("known response failed to recover: %+v", set)
					}
					if family != "exposure" && (m.UncertaintyAbs == nil || *m.UncertaintyAbs != 0.25 || !m.Coverage.Reported || m.Coverage.Contributed != 2) {
						t.Fatalf("advisory response lost its uncertainty/coverage: %+v", m)
					}
				})
			}
		})
	}
}
