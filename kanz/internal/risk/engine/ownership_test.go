package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
)

// THE QUERY PATH IS THE FOURTH CONSUMER OF THE RING (#110).
//
// The read path never consulted ownership. It asked the store and, on a miss,
// fell back to risk.Cache — the last-known-good value. On a sharded replica
// both halves are wrong, and the fallback half is the dangerous one: the cache
// is filled by THIS replica's own recomputes, so a portfolio it used to own
// leaves a plausible entry behind. Serving it is a real number computed from a
// book this replica no longer holds, and the degraded-fallback machinery was
// built for "the store lost it", not "somebody else owns it".
//
// The other half is quieter and still wrong: with an empty cache the answer was
// ErrPortfolioNotFound, which says the portfolio does not exist. It does. And
// because one Service fans out across every replica, the caller got that answer
// for the fraction of identical requests that happened to land on a non-owner.

// fixedOwnership is the ring's verdict, stated directly. Ownership is a pure
// function of (members, key), so nothing here needs a ring to be exercised.
type fixedOwnership struct {
	mine  v1.PortfolioID
	owner string
}

func (f fixedOwnership) Owns(id v1.PortfolioID) bool { return id == f.mine }
func (f fixedOwnership) OwnerOf(v1.PortfolioID) string {
	return f.owner
}

// primed builds an engine whose CACHE holds a value for id while its STORE does
// not — the shape a replica is in just after losing a portfolio, and the one
// that used to produce a confident wrong answer.
func primed(t *testing.T, id v1.PortfolioID, own engine.Ownership) *engine.EngineImpl {
	t.Helper()
	asOf := time.Unix(1000, 0).UTC()

	// Fill the cache the only way production does: compute through an engine
	// whose store holds the portfolio.
	warm := state.NewStore()
	env := &envelopepb.Envelope{IdempotencyKey: "k1", PartitionKey: string(id)}
	pos := &domainpb.PositionState{
		PortfolioId:  string(id),
		InstrumentId: "AAPL",
		MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 500}, CurrencyCode: "USD"},
		AsOf:         timestamppb.New(asOf),
	}
	if err := warm.ApplyPositionChanged(context.Background(), env, pos); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	cache := risk.NewCache()
	seeder := engine.New(warm, compute.DefaultRegistry(), cache, risk.NewDetector())
	if _, err := seeder.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: id}); err != nil {
		t.Fatalf("seed exposure: %v", err)
	}
	if _, err := seeder.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: id}); err != nil {
		t.Fatalf("seed measures: %v", err)
	}
	if _, ok := cache.LookupExposure(id); !ok {
		t.Fatal("the cache was not primed — every assertion below would be vacuous")
	}

	// The engine under test shares that cache but has an EMPTY store, exactly
	// like a replica that has just given the portfolio up.
	return engine.New(state.NewStore(), compute.DefaultRegistry(), cache, risk.NewDetector(),
		engine.WithOwnership(own))
}

// A NON-OWNER REFUSES EVEN WITH A PLAUSIBLE CACHED ANSWER IN HAND.
func TestOwnership_RefusesRatherThanServingTheCache(t *testing.T) {
	const id = v1.PortfolioID("PORT-THEIRS")
	e := primed(t, id, fixedOwnership{mine: "PORT-MINE", owner: "risk-engine-2"})
	ctx := context.Background()

	if _, err := e.Exposure(ctx, v1.ExposureRequest{PortfolioID: id}); !errors.Is(err, v1.ErrPortfolioNotOwned) {
		t.Errorf("Exposure = %v, want ErrPortfolioNotOwned — a cached value from when this replica "+
			"did own it is a real number computed from a book it no longer has", err)
	}
	if _, err := e.Measures(ctx, v1.MeasuresRequest{PortfolioID: id}); !errors.Is(err, v1.ErrPortfolioNotOwned) {
		t.Errorf("Measures = %v, want ErrPortfolioNotOwned", err)
	}
	if _, err := e.EvaluateScenario(ctx, v1.ScenarioRequest{PortfolioID: id}); !errors.Is(err, v1.ErrPortfolioNotOwned) {
		t.Errorf("EvaluateScenario = %v, want ErrPortfolioNotOwned — a scenario is computed from "+
			"the same state as every other read", err)
	}
}

// THE REFUSAL IS NOT A NOT-FOUND, AND IT NAMES WHERE TO GO.
//
// Collapsing the two would keep the exact defect: "no such portfolio" is false,
// and it is what two thirds of a three-replica fleet would answer.
func TestOwnership_TheRefusalIsDistinctFromNotFound(t *testing.T) {
	const id = v1.PortfolioID("PORT-THEIRS")
	e := primed(t, id, fixedOwnership{mine: "PORT-MINE", owner: "risk-engine-2"})

	_, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: id})
	if errors.Is(err, v1.ErrPortfolioNotFound) {
		t.Fatalf("the refusal also matches ErrPortfolioNotFound (%v) — a caller branching on "+
			"not-found concludes the portfolio does not exist, which is the lie this replaces", err)
	}
	if got := err.Error(); !strings.Contains(got, "risk-engine-2") {
		t.Errorf("the refusal %q does not name the owning replica; without it the caller has a "+
			"dead end rather than a route", got)
	}
}

// AN OWNED PORTFOLIO IS ANSWERED NORMALLY — including through the degraded
// cache fallback, which stays available for the case it was built for.
func TestOwnership_OwnedPortfoliosStillUseTheDegradedFallback(t *testing.T) {
	const id = v1.PortfolioID("PORT-MINE")
	e := primed(t, id, fixedOwnership{mine: id, owner: "self"})

	resp, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: id})
	if err != nil {
		t.Fatalf("Exposure for an owned portfolio with an empty store = %v; the last-known-good "+
			"fallback must survive this change", err)
	}
	if resp.Set == nil {
		t.Error("the fallback returned a nil exposure set")
	}
}

// A NIL OWNERSHIP IS THE UNSHARDED DEFAULT AND CHANGES NOTHING.
func TestOwnership_UnshardedEngineAnswersEverything(t *testing.T) {
	const id = v1.PortfolioID("PORT-ANY")
	e := primed(t, id, nil)

	if _, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: id}); err != nil {
		t.Fatalf("an unsharded engine refused %q: %v", id, err)
	}
}
