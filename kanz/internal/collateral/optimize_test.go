package collateral

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/eighred/kanz/internal/dec"
	"math/rand"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

func eligible(h dec.Exact) Eligibility { return Eligibility{Eligible: true, Haircut: h} }
func asset(id string, available, cost dec.Exact) Asset {
	return Asset{ID: id, Currency: "USD", Available: available, Cost: cost}
}
func requirement(id string, amount dec.Exact, schedule map[string]Eligibility) Requirement {
	return Requirement{AgreementID: id, Currency: "USD", Amount: amount, Schedule: schedule}
}
func solve(t *testing.T, a []Asset, r []Requirement) AllocationResult {
	t.Helper()
	got, err := Optimize(context.Background(), a, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAllocation(a, r, got); err != nil {
		t.Fatal(err)
	}
	return got
}
func TestOptimizeScarceEligibilityAndPermutations(t *testing.T) {
	a := []Asset{asset("A", "100", "1"), asset("B", "100", "2")}
	r := []Requirement{requirement("flexible", "100", map[string]Eligibility{"A": eligible("0"), "B": eligible("0")}), requirement("restricted", "100", map[string]Eligibility{"A": eligible("0")})}
	want := solve(t, a, r)
	if !want.Feasible || want.Cost != "300" || len(want.Allocations) != 2 {
		t.Fatalf("%+v", want)
	}
	for i := 0; i < 4; i++ {
		a[0], a[1] = a[1], a[0]
		if i%2 == 0 {
			r[0], r[1] = r[1], r[0]
		}
		if got := solve(t, a, r); !reflect.DeepEqual(got, want) {
			t.Fatalf("input order changed result: %+v", got)
		}
	}
}
func TestOptimizeHaircutsAndExactFractions(t *testing.T) {
	a := []Asset{asset("BOND", "1000", "0.05")}
	r := []Requirement{requirement("AG", "400", map[string]Eligibility{"BOND": eligible("0.2")})}
	got := solve(t, a, r)
	if !got.Feasible || got.Cost != "25" || got.Allocations[0].UsedValue != "500" {
		t.Fatalf("%+v", got)
	}
	a = []Asset{asset("A", "2", "3")}
	r = []Requirement{requirement("AG", "1", map[string]Eligibility{"A": eligible("0.25")})}
	got = solve(t, a, r)
	if got.Allocations[0].UsedValue != "4/3" || got.Cost != "4" {
		t.Fatalf("%+v", got)
	}
	// Different agreement haircuts require a weighted LP, not plain min-cost flow.
	a = []Asset{asset("A", "2", "1"), asset("B", "2", "2")}
	r = []Requirement{requirement("X", "1", map[string]Eligibility{"A": eligible("0.5"), "B": eligible("0")}), requirement("Y", "1", map[string]Eligibility{"A": eligible("0"), "B": eligible("0.5")})}
	got = solve(t, a, r)
	if !got.Feasible || got.Cost != "3" {
		t.Fatalf("%+v", got)
	}
}

func TestOptimizeImprovesFeasibleButExpensiveAllocation(t *testing.T) {
	a := []Asset{asset("A", "1", "1"), asset("B", "1", "2"), asset("C", "1", "100")}
	r := []Requirement{
		requirement("X", "1", map[string]Eligibility{"A": eligible("0"), "B": eligible("0")}),
		requirement("Y", "1", map[string]Eligibility{"A": eligible("0"), "C": eligible("0")}),
	}
	got := solve(t, a, r)
	if !got.Feasible || got.Cost != "3" {
		t.Fatalf("a feasible allocation costing 101 must be improved to 3: %+v", got)
	}
}

func TestInfeasibilityCertificateCannotBeForged(t *testing.T) {
	a := []Asset{asset("A", "1", "1")}
	r := []Requirement{requirement("X", "2", map[string]Eligibility{"A": eligible("0")})}
	got := solve(t, a, r)
	got.Certificate.Coverage["X"] = "0"
	if VerifyAllocation(a, r, got) == nil {
		t.Fatal("accepted a false infeasibility witness")
	}
}

func TestPivotLimitIsNotAnInfeasibilityResult(t *testing.T) {
	m, err := buildAllocationModel([]Asset{asset("A", "1", "1")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tableau := newAllocationTableau(m)
	tableau.pivots = maxAllocationPivots
	if err := tableau.pivot(context.Background(), 0, 0); !errors.Is(err, ErrAllocationLimit) {
		t.Fatal(err)
	}
}
func TestOptimizeRefusesInfeasibleWithoutPartialPostings(t *testing.T) {
	for _, a := range [][]Asset{nil, {asset("A", "0", "1")}, {asset("A", "0.999999999999999999999", "1")}} {
		schedule := map[string]Eligibility{}
		if len(a) > 0 {
			schedule["A"] = eligible("0")
		}
		got := solve(t, a, []Requirement{requirement("AG", "1", schedule)})
		if got.Feasible || len(got.Allocations) != 0 {
			t.Fatalf("%+v", got)
		}
	}
	if got := solve(t, nil, nil); !got.Feasible {
		t.Fatal(got)
	}
	if got := solve(t, nil, []Requirement{requirement("AG", "0", nil)}); !got.Feasible {
		t.Fatal(got)
	}
}
func TestOptimizeRejectsUnavailableAndAmbiguousInputs(t *testing.T) {
	for _, bad := range []dec.Exact{"", "NaN", "Inf", "-1", "1e9999", "1/0"} {
		if _, err := Optimize(context.Background(), []Asset{asset("A", bad, "1")}, nil); !errors.Is(err, ErrAllocationInput) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	a := []Asset{asset("A", "1", "1")}
	cases := []struct {
		a []Asset
		r []Requirement
	}{
		{[]Asset{a[0], a[0]}, nil},
		{a, []Requirement{requirement("AG", "1", nil), requirement("AG", "1", nil)}},
		{a, []Requirement{{AgreementID: "AG", Currency: "EUR", Amount: "1"}}},
		{a, []Requirement{requirement("AG", "1", map[string]Eligibility{"UNKNOWN": eligible("0")})}},
		{a, []Requirement{requirement("AG", "1", map[string]Eligibility{"A": eligible("1")})}},
		{a, []Requirement{requirement("AG", "1", map[string]Eligibility{"A": eligible("")})}},
	}
	for i, c := range cases {
		if _, err := Optimize(context.Background(), c.a, c.r); !errors.Is(err, ErrAllocationInput) {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	if _, err := Optimize(context.Background(), make([]Asset, MaxAllocationAssets+1), nil); !errors.Is(err, ErrAllocationLimit) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Optimize(ctx, a, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestCertificateRejectsTampering(t *testing.T) {
	a := []Asset{asset("A", "10", "2")}
	r := []Requirement{requirement("AG", "3", map[string]Eligibility{"A": eligible("0")})}
	good := solve(t, a, r)
	for _, mutate := range []func(*AllocationResult){
		func(v *AllocationResult) { v.Cost = "0" }, func(v *AllocationResult) { v.Allocations[0].UsedValue = "30" },
		func(v *AllocationResult) { v.Allocations[0].PostedValue = "2" }, func(v *AllocationResult) { v.Certificate.Coverage["AG"] = "0" },
		func(v *AllocationResult) { v.Feasible = false }, func(v *AllocationResult) { v.Allocations = append(v.Allocations, v.Allocations[0]) },
		func(v *AllocationResult) { v.Currency = "EUR" },
	} {
		data, _ := json.Marshal(good)
		var bad AllocationResult
		if err := json.Unmarshal(data, &bad); err != nil {
			t.Fatal(err)
		}
		mutate(&bad)
		if VerifyAllocation(a, r, bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
func TestOptimizeAgainstIndependentEnumeration(t *testing.T) {
	// Zero-haircut transportation matrices are integral. Enumerating all integer
	// postings is therefore an independent optimum oracle for these small LPs.
	rng := rand.New(rand.NewSource(1235))
	for trial := 0; trial < 300; trial++ {
		cap := [2]int{rng.Intn(4), rng.Intn(4)}
		need := [2]int{rng.Intn(4), rng.Intn(4)}
		cost := [2]int{rng.Intn(4), rng.Intn(4)}
		allowed := [4]bool{}
		for i := range allowed {
			allowed[i] = rng.Intn(2) == 0
		}
		exact := func(v int) dec.Exact { return dec.Exact(strconv.Itoa(v)) }
		a := []Asset{asset("A", exact(cap[0]), exact(cost[0])), asset("B", exact(cap[1]), exact(cost[1]))}
		r := []Requirement{requirement("X", exact(need[0]), map[string]Eligibility{}), requirement("Y", exact(need[1]), map[string]Eligibility{})}
		for i, ok := range allowed {
			if ok {
				r[i/2].Schedule[a[i%2].ID] = eligible("0")
			}
		}
		best := -1
		for ax := 0; ax <= cap[0]; ax++ {
			for ay := 0; ay <= cap[0]-ax; ay++ {
				for bx := 0; bx <= cap[1]; bx++ {
					for by := 0; by <= cap[1]-bx; by++ {
						if ax+bx != need[0] || ay+by != need[1] || (!allowed[0] && ax > 0) || (!allowed[1] && bx > 0) || (!allowed[2] && ay > 0) || (!allowed[3] && by > 0) {
							continue
						}
						c := (ax+ay)*cost[0] + (bx+by)*cost[1]
						if best < 0 || c < best {
							best = c
						}
					}
				}
			}
		}
		got := solve(t, a, r)
		if got.Feasible != (best >= 0) || (best >= 0 && got.Cost != exact(best)) {
			t.Fatalf("trial %d: want %d got %+v assets=%+v needs=%+v", trial, best, got, a, r)
		}
	}
}
func TestOptimizeConcurrentCallsDoNotMutateInputs(t *testing.T) {
	a := []Asset{asset("A", "10", "1")}
	r := []Requirement{requirement("X", "5", map[string]Eligibility{"A": eligible("0")})}
	before, _ := json.Marshal(r)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Optimize(context.Background(), a, r); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, _ := json.Marshal(r)
	if string(before) != string(after) || a[0].Available != "10" {
		t.Fatal("input mutated")
	}
}
