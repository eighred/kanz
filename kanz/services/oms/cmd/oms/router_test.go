package main

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// EVERY BRANCH OF THE ROUTING DECISION, WHICH NONE OF THEM WAS REACHABLE BEFORE
// (#437, #643).
//
// This is a four-way decision with one hard refusal, two log lines and one silent
// success, and until buildVenueRouter existed it lived inline among fifty other
// constructions in runConsumers — so the fatal case had never been executed by
// anything, and the two advisory cases had never been read. A refusal that has
// never run is a refusal nobody has checked the wording of, and the wording is
// the whole value of this one: it tells an operator that the venue they believe
// is wired is not.

func routerLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func simVenues(mics ...string) []execution.Venue {
	out := make([]execution.Venue, 0, len(mics))
	for _, mic := range mics {
		out = append(out, execution.NewSimVenue(mic))
	}
	return out
}

// A NAMED VENUE THIS OMS HOLDS is the configured case, and the log must say
// whether the choice was explicit — "we defaulted to the only adapter" and
// "somebody chose this" are different facts about a deployment.
func TestANamedDefaultVenueIsUsedAndSaidToBeExplicit(t *testing.T) {
	logger, buf := routerLog()
	r, err := buildVenueRouter(config.Config{DefaultVenueMIC: "XLON"}, simVenues("XNAS", "XLON"), logger)
	if err != nil {
		t.Fatalf("buildVenueRouter: %v", err)
	}
	v, ok := r.Router.DefaultVenue()
	if !ok {
		t.Fatal("no default venue after naming one this OMS holds — every untargeted order would " +
			"be refused naming a venue the operator believes is wired")
	}
	if v.MIC() != "XLON" {
		t.Errorf("default venue = %s, want XLON", v.MIC())
	}
	if !strings.Contains(buf.String(), "named_explicitly=true") {
		t.Errorf("the startup log does not record that the default was CHOSEN: %s", buf.String())
	}
	if r.Costs == nil {
		t.Error("no venue-cost ranker was built — an untargeted order would be routed with no " +
			"realized-cost evidence at all (#436)")
	}
}

// THE COST RANKER REACHES THE ROUTER, WHICH IS A DIFFERENT CLAIM FROM "ONE WAS
// BUILT".
//
// A composition that constructs a VenueCosts and forgets to pass it produces a
// router that silently ignores every realized-cost measurement the estate takes:
// costwatch keeps folding fills, the dashboard keeps showing shortfall by venue,
// and #437 B's preference never applies to a single order. Nothing fails, and the
// only observable difference is which venue an untargeted order reaches.
//
// So this drives the ranker past its evidence floor and asks the ROUTER where an
// untargeted order goes. XNAS is the declared default and is measured expensive;
// XLON is measured cheap. A wired ranker prefers XLON.
func TestTheCostRankerReachesTheRouterAndOutranksTheDeclaredDefault(t *testing.T) {
	logger, _ := routerLog()
	r, err := buildVenueRouter(config.Config{DefaultVenueMIC: "XNAS"}, simVenues("XNAS", "XLON"), logger)
	if err != nil {
		t.Fatalf("buildVenueRouter: %v", err)
	}
	// Past DefaultMinSamples DISTINCT decisions on both venues, so neither
	// abstains for want of evidence and the comparison is the cost itself.
	for i := 0; i < execution.DefaultMinSamples+5; i++ {
		id := "decision-" + strconv.Itoa(i)
		r.Costs.Observe("XNAS", 45, 100_000, id)
		r.Costs.Observe("XLON", 2, 100_000, id)
	}
	if preferred, ok := r.Costs.Preferred([]string{"XNAS", "XLON"}); !ok || preferred != "XLON" {
		t.Fatalf("the ranker itself prefers %q (ok=%v) — the fixture does not exercise what this "+
			"test is about", preferred, ok)
	}

	v, err := r.Router.Route(&orderpb.OrderState{})
	if err != nil {
		t.Fatalf("routing an untargeted order: %v", err)
	}
	if v.MIC() != "XLON" {
		t.Errorf("an untargeted order routed to %s, want XLON — the cost ranker was built and never "+
			"handed to the router, so every realized-cost measurement this estate takes reaches no "+
			"routing decision (#437 B)", v.MIC())
	}
}

// A NAMED VENUE THIS OMS DOES NOT HOLD REFUSES THE START. This is the one fatal
// branch, and starting is worse than crashing: the deployment looks configured,
// logs nothing unusual, and refuses every untargeted order with an error naming a
// venue the operator believes is wired.
func TestANamedVenueNoAdapterHoldsRefusesToStart(t *testing.T) {
	logger, _ := routerLog()
	r, err := buildVenueRouter(config.Config{DefaultVenueMIC: "XPAR"}, simVenues("XNAS", "XLON"), logger)
	if err == nil {
		t.Fatal("buildVenueRouter accepted a default venue no configured adapter holds")
	}
	if r.Router != nil {
		t.Error("a router was returned alongside the refusal")
	}
	// THE MESSAGE IS THE FEATURE. An operator reading it must learn both what they
	// asked for and what this OMS actually holds; without the second half the fix
	// is a guess.
	for _, want := range []string{"XPAR", "XNAS", "XLON", "OMS_DEFAULT_VENUE_MIC"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q — an operator cannot act on it: %v", want, err)
		}
	}
}

// NO DEFAULT AND SEVERAL ADAPTERS IS A WARNING, NOT A REFUSAL. Every order from
// the fan-out producers carries a target, so this deployment trades; refusing to
// start over a path nothing takes would be a self-inflicted outage.
func TestNoDefaultWithSeveralVenuesWarnsAndStillStarts(t *testing.T) {
	logger, buf := routerLog()
	r, err := buildVenueRouter(config.Config{}, simVenues("XNAS", "XLON"), logger)
	if err != nil {
		t.Fatalf("buildVenueRouter refused to start over a path nothing currently takes: %v", err)
	}
	if _, ok := r.Router.DefaultVenue(); ok {
		t.Error("a default venue was chosen with none configured and two adapters present — the " +
			"router picked one nobody named, which is the #437 defect returning")
	}
	log := buf.String()
	if !strings.Contains(log, "NO DEFAULT VENUE") {
		t.Errorf("the ambiguous case is silent: %s", log)
	}
	if !strings.Contains(log, "XNAS") || !strings.Contains(log, "XLON") {
		t.Errorf("the warning does not name the candidates, so an operator cannot pick one: %s", log)
	}
}

// ONE ADAPTER NEEDS NO CONFIGURATION. The router resolves it by construction,
// and the log must NOT claim somebody chose it.
func TestASingleVenueIsTheDefaultWithoutBeingNamed(t *testing.T) {
	logger, buf := routerLog()
	r, err := buildVenueRouter(config.Config{}, simVenues("XNAS"), logger)
	if err != nil {
		t.Fatalf("buildVenueRouter: %v", err)
	}
	v, ok := r.Router.DefaultVenue()
	if !ok {
		t.Fatal("the only configured adapter is not the default — every untargeted order would be " +
			"refused on a deployment with nothing to be ambiguous about")
	}
	if v.MIC() != "XNAS" {
		t.Errorf("default venue = %s, want XNAS", v.MIC())
	}
	if strings.Contains(buf.String(), "named_explicitly=true") {
		t.Errorf("the log claims the default was chosen explicitly when nothing named it: %s", buf.String())
	}
}

// NO ADAPTERS AT ALL still builds — the venue set is a build-tag decision, and a
// deployment with none is refused upstream where that is knowable, not here.
// This is the non-vacuity arm for the three above: a builder that returned an
// error for everything would pass every "want err" assertion in this file.
func TestNoVenuesIsNotThisBuildersRefusal(t *testing.T) {
	logger, _ := routerLog()
	r, err := buildVenueRouter(config.Config{}, nil, logger)
	if err != nil {
		t.Fatalf("buildVenueRouter refused an empty venue set: %v", err)
	}
	if r.Router == nil || r.Costs == nil {
		t.Error("an empty venue set produced no router or no cost ranker")
	}
	if _, ok := r.Router.DefaultVenue(); ok {
		t.Error("a default venue was resolved from no venues at all")
	}
}
