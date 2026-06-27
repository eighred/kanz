package collateral

import (
	"math"
	"testing"
	"time"
)

func TestFinancing_RepoInterestAndFlows(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := FinancingTrade{
		ID: "R1", Kind: Repo, Currency: "USD",
		Principal: 1_000_000, Rate: 0.03,
		Start: start, End: start.AddDate(0, 0, 90),
	}
	wantInterest := 1_000_000 * 0.03 * (90.0 / 365.0)
	if math.Abs(repo.Interest()-wantInterest) > 1e-6 {
		t.Fatalf("interest: got %.4f want %.4f", repo.Interest(), wantInterest)
	}
	flows := repo.CashFlows()
	if len(flows) != 2 || flows[0].Amount != 1_000_000 {
		t.Fatalf("repo near-leg should be +1,000,000, got %+v", flows)
	}
	if math.Abs(flows[1].Amount-(-(1_000_000 + wantInterest))) > 1e-6 {
		t.Fatalf("repo far-leg should be -(principal+interest), got %.4f", flows[1].Amount)
	}
}

func TestCashLadder_NetsToBalance(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := FinancingTrade{ID: "R1", Kind: Repo, Currency: "USD", Principal: 1_000_000, Rate: 0.03, Start: start, End: start.AddDate(0, 0, 90)}
	revShort := FinancingTrade{ID: "RR", Kind: ReverseRepo, Currency: "USD", Principal: 400_000, Rate: 0.025, Start: start.AddDate(0, 0, 30), End: start.AddDate(0, 0, 120)}

	var flows []CashFlow
	flows = append(flows, repo.CashFlows()...)
	flows = append(flows, revShort.CashFlows()...)

	const opening = 250_000.0
	rungs := BuildLadder(opening, flows)

	// The ladder's closing balance equals opening + Σ all flows.
	var sum float64
	for _, f := range flows {
		sum += f.Amount
	}
	closing := rungs[len(rungs)-1].Balance
	if math.Abs(closing-(opening+sum)) > 1e-6 {
		t.Fatalf("ladder must net to opening+Σflows: closing=%.4f want=%.4f", closing, opening+sum)
	}
	if math.Abs(ClosingBalance(opening, flows)-closing) > 1e-9 {
		t.Fatal("ClosingBalance must match the last rung")
	}
	// Rungs are in ascending date order.
	for i := 1; i < len(rungs); i++ {
		if rungs[i].Date.Before(rungs[i-1].Date) {
			t.Fatal("ladder rungs must be date-ordered")
		}
	}
}

// TestRepo_NetCostIsInterest: a repo's only economic cost over its life is the
// financing interest — the near and far principal legs cancel in the ladder.
func TestRepo_NetCostIsInterest(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := FinancingTrade{ID: "R1", Kind: Repo, Currency: "USD", Principal: 1_000_000, Rate: 0.03, Start: start, End: start.AddDate(0, 0, 90)}
	closing := ClosingBalance(0, repo.CashFlows())
	if math.Abs(closing-(-repo.Interest())) > 1e-6 {
		t.Fatalf("repo net cash should be -interest, got %.4f vs %.4f", closing, -repo.Interest())
	}
}
