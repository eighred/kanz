package grpcsrv_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/services/risk-engine/internal/grpcsrv"
)

// #1004: the two shock kinds that were never on the wire, and the named-catalog
// dispatch that could not exist without them.
//
// Everything these tests exercise was already built and already correct — the
// scenario engine applied SectorShock and VolShock, the factor classifier was
// wired at the composition root, and the historical/climate catalog was written
// and unit-tested. The oneof was two members short, so none of it could be
// asked for. What is asserted here is REACHABILITY: a request a desk can send
// arrives at the engine as the shocks the catalog defines.

// captureShocks returns a server whose engine records the shocks it was handed,
// plus the recorder. The engine itself is not under test here — the decode and
// the named-scenario resolution in front of it are.
func captureShocks(t *testing.T) (*grpcsrv.Server, *[]v1.ScenarioShock) {
	t.Helper()
	var got []v1.ScenarioShock
	ms := domain.NewMeasureSet("PF1", asOf, map[v1.MeasureName]v1.Measure{})
	srv := grpcsrv.New(fakeEngine{scenario: func(r v1.ScenarioRequest) (v1.ScenarioResponse, error) {
		got = r.Shocks
		return v1.ScenarioResponse{PortfolioID: "PF1", Projected: ms}, nil
	}}, "acme")
	return srv, &got
}

func TestEvaluateScenarioDecodesSectorAndVolShocks(t *testing.T) {
	srv, got := captureShocks(t)

	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId: "PF1",
		Shocks: []*querypb.ScenarioShock{
			{Shock: &querypb.ScenarioShock_Sector{Sector: &querypb.SectorShock{
				Taxonomy: "GICS", Code: "40",
				Pct: &commonpb.Decimal{Coefficient: -55, Exponent: -2}}}},
			{Shock: &querypb.ScenarioShock_Vol{Vol: &querypb.VolShock{
				UnderlyingId: "SPX",
				AbsBump:      &commonpb.Decimal{Coefficient: 15, Exponent: -2}}}},
		},
	})
	if err != nil {
		// Before #1004 this was the whole defect: apiShocks had no case for
		// either kind, so its default arm refused a shock the engine could apply.
		t.Fatalf("a sector/vol scenario was refused: %v", err)
	}
	if len(*got) != 2 {
		t.Fatalf("want 2 shocks decoded, got %d (%#v)", len(*got), *got)
	}
	sec, ok := (*got)[0].(v1.SectorShock)
	if !ok {
		t.Fatalf("first shock is %#v, want v1.SectorShock", (*got)[0])
	}
	if sec.Taxonomy != "GICS" || sec.Code != "40" {
		t.Errorf("sector shock decoded as %+v, want GICS:40 — a shock that names the wrong "+
			"sector shocks the wrong positions and reports no error", sec)
	}
	if sec.Pct.GetCoefficient() != -55 || sec.Pct.GetExponent() != -2 {
		t.Errorf("sector pct decoded as %+v, want -55e-2", sec.Pct)
	}
	vol, ok := (*got)[1].(v1.VolShock)
	if !ok {
		t.Fatalf("second shock is %#v, want v1.VolShock", (*got)[1])
	}
	if vol.UnderlyingID != "SPX" || vol.AbsBump.GetCoefficient() != 15 {
		t.Errorf("vol shock decoded as %+v, want SPX +15e-2", vol)
	}
}

// TestNamedScenarioResolvesToTheCatalogCurve is the reachability claim in full:
// a caller sends a NAME and the engine receives the curve. The dispersion
// assertion (financials below staples) is the point of a named historical
// stress — a parallel shift cannot express it, and a resolution that silently
// produced no shocks would return the unshocked book under the name "GFC_2008".
func TestNamedScenarioResolvesToTheCatalogCurve(t *testing.T) {
	srv, got := captureShocks(t)

	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId:  "PF1",
		ScenarioName: "GFC_2008",
	})
	if err != nil {
		t.Fatalf("GFC_2008 was refused: %v", err)
	}
	if len(*got) != 11 {
		t.Fatalf("GFC_2008 resolved to %d shocks, want the 11 GICS sectors — an empty or short "+
			"curve is a stress that leaves positions unshocked while carrying the crisis's name",
			len(*got))
	}
	byCode := map[string]v1.SectorShock{}
	for _, sh := range *got {
		sec, ok := sh.(v1.SectorShock)
		if !ok {
			t.Fatalf("GFC_2008 produced a %#v, want only v1.SectorShock", sh)
		}
		if sec.Taxonomy != "GICS" {
			t.Errorf("shock %+v is not on the GICS taxonomy the classifier resolves against", sec)
		}
		byCode[sec.Code] = sec
	}
	const (
		financials = "40"
		staples    = "30"
	)
	fin, okFin := byCode[financials]
	stp, okStp := byCode[staples]
	if !okFin || !okStp {
		t.Fatalf("GFC_2008 curve is missing financials (%v) or staples (%v): %v", okFin, okStp, byCode)
	}
	// Same exponent across the curve, so the coefficients are directly
	// comparable; assert that rather than assume it, because a mixed-exponent
	// curve would make this comparison silently meaningless.
	if fin.Pct.GetExponent() != stp.Pct.GetExponent() {
		t.Fatalf("financials %+v and staples %+v carry different exponents — this comparison "+
			"would be reading scaled values as if they were the same", fin.Pct, stp.Pct)
	}
	if fin.Pct.GetCoefficient() >= stp.Pct.GetCoefficient() {
		t.Errorf("financials %v is not below staples %v in the 2008 curve — a named historical "+
			"stress that does not differentiate is a parallel shift wearing a crisis's name",
			fin.Pct.GetCoefficient(), stp.Pct.GetCoefficient())
	}
}

// TestNamedClimateScenarioResolves covers the second catalog: the NGFS-style
// transition/physical pair is reached through the SAME field, so a desk running
// a climate stress does not need a second route (and internal/regulatory's
// TCFD/SFDR filings have an engine-native scenario behind them).
func TestNamedClimateScenarioResolves(t *testing.T) {
	for _, name := range []string{"DISORDERLY_TRANSITION", "HOT_HOUSE_PHYSICAL"} {
		t.Run(name, func(t *testing.T) {
			srv, got := captureShocks(t)
			if _, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
				PortfolioId:  "PF1",
				ScenarioName: name,
			}); err != nil {
				t.Fatalf("%s was refused: %v", name, err)
			}
			if len(*got) == 0 {
				t.Fatalf("%s resolved to no shocks — the climate catalog is not reachable "+
					"through scenario_name", name)
			}
		})
	}
}

// TestUnknownScenarioNameIsRefusedAndNamesTheCatalog. The alternative — resolve
// to nothing and evaluate — returns the CURRENT BOOK as a projection under the
// requested name, which is #640's failure one layer out.
func TestUnknownScenarioNameIsRefusedAndNamesTheCatalog(t *testing.T) {
	srv, got := captureShocks(t)

	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId:  "PF1",
		ScenarioName: "GFC", // the plausible typo: the catalog key is GFC_2008
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for an unknown scenario, got %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("the engine was called with %d shocks for an unresolvable scenario — an "+
			"unknown name must never reach evaluation as an empty shock list", len(*got))
	}
	// The catalog is listed so a typo is a one-step fix rather than a guess.
	// This is also the only discovery surface a caller has today.
	for _, want := range []string{"GFC_2008", "COVID_2020", "DISORDERLY_TRANSITION", "HOT_HOUSE_PHYSICAL"} {
		if !strings.Contains(status.Convert(err).Message(), want) {
			t.Errorf("refusal %q does not name %s — the caller cannot tell what IS available",
				status.Convert(err).Message(), want)
		}
	}
}

func TestScenarioWithBothShocksAndNameIsRefused(t *testing.T) {
	srv, got := captureShocks(t)

	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId:  "PF1",
		ScenarioName: "GFC_2008",
		Shocks: []*querypb.ScenarioShock{
			{Shock: &querypb.ScenarioShock_ParallelShift{ParallelShift: &querypb.ParallelShift{
				Pct: &commonpb.Decimal{Coefficient: -5, Exponent: -2}}}},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument when both shocks and scenario_name are set, got %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("the engine was called with %d shocks for an ambiguous request — whichever "+
			"source won, the caller was answered with a scenario they did not ask for", len(*got))
	}
}

// TestScenarioWithNoPerturbationIsRefused. An empty request used to evaluate
// cleanly and return the CURRENT BOOK with measures, quality flags and an owner
// tenant — a projection indistinguishable from a stress that genuinely moved
// nothing. "Nothing configured" and "checked, and fine" must not look the same,
// least of all on a risk answer.
func TestScenarioWithNoPerturbationIsRefused(t *testing.T) {
	srv, got := captureShocks(t)

	_, err := srv.EvaluateScenario(context.Background(), &querypb.EvaluateScenarioRequest{
		PortfolioId: "PF1",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a scenario with no shocks and no name, got %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("the engine evaluated a scenario with %d shocks — an empty request must not "+
			"come back as the unshocked book wearing a projection's shape", len(*got))
	}
}
