package main

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// WHERE AN UNTARGETED ORDER GOES, DECIDED WHERE IT CAN BE TESTED (#437, #643).
//
// # The four postures, and why only one of them is fatal
//
// The router used to answer venues[0] — whichever adapter the config happened to
// list first, chosen by nobody, while the type called itself a smart order
// router. #437 made it a named choice. That left this composition root holding a
// four-way decision with two log lines, one hard refusal and one silent success
// path, inline among fifty other constructions, where no test could reach any of
// them.
//
//	OMS_DEFAULT_VENUE_MIC names a venue this OMS holds   → INFO, that venue
//	OMS_DEFAULT_VENUE_MIC names one it does NOT hold     → REFUSE TO START
//	unset, and more than one adapter is configured       → WARN, no default
//	unset, and one adapter is configured                 → that one, by construction
//
// THE FATAL ONE IS THE SECOND, and it is fatal because starting is worse: the
// deployment looks configured, logs nothing unusual, and refuses every untargeted
// order with an error naming a venue the operator believes is wired. That
// refusal is safe HERE specifically because OMS_DEFAULT_VENUE_MIC is new in
// #437 and no deployment sets it — the only way to reach it is to set it wrong
// today, so it cannot turn an existing estate's silent state into an outage.
//
// THE THIRD IS DELIBERATELY NOT FATAL. Every order from the fan-out producers
// carries a target, so a deployment with several adapters trades perfectly well
// with no default; refusing to start over a path nothing currently takes would be
// a self-inflicted outage. An order that does arrive untargeted is refused with
// the candidates named.

// venueRouting is the wired router and the cost signal it ranks with.
type venueRouting struct {
	// Router resolves an order's venue: the one it names, or the default.
	Router *execution.Router
	// Costs is the realized-shortfall fold the router consults for an untargeted
	// order. The composition root feeds it from the fill FACTs.
	Costs *execution.VenueCosts
}

// buildVenueRouter resolves the routing decision above and returns the router.
//
// MEASURED VENUE COST, FOR AN UNTARGETED ORDER (#437 B, on #436's signal).
// costwatch folds every fill's realized shortfall into Costs, and the router
// reads it when an order names no venue. It is a PREFERENCE over venues that have
// already passed every correctness check — it cannot admit a venue they refused,
// and it abstains below its evidence floor, leaving the DECLARED default in
// charge. So the worst it does is prefer the wrong one of two acceptable venues,
// better informed than the config line it defers to.
//
// In-process, deliberately: the OMS publishes order.cost.recorded and does not
// subscribe to it. Reading its own FACT back would put a broker round trip inside
// a feedback loop. The honest cost is that each replica ranks on the fills it
// saw — acceptable for a preference, and the sample floor means a replica with
// thin evidence abstains rather than acting on noise.
func buildVenueRouter(cfg config.Config, venues []execution.Venue, logger *slog.Logger) (venueRouting, error) {
	costs := execution.NewVenueCosts()
	router := execution.NewRouter(venues,
		execution.WithDefaultVenue(cfg.DefaultVenueMIC),
		execution.WithCostRanker(costs))

	if v, ok := router.DefaultVenue(); ok {
		logger.Info("oms: orders naming no venue will be worked at", "venue", v.MIC(),
			"named_explicitly", cfg.DefaultVenueMIC != "")
		return venueRouting{Router: router, Costs: costs}, nil
	}
	if cfg.DefaultVenueMIC != "" {
		return venueRouting{}, fmt.Errorf("oms: OMS_DEFAULT_VENUE_MIC=%q but no configured adapter holds it "+
			"(this OMS holds %s) — an untargeted order would be refused naming a venue you "+
			"believe is wired", cfg.DefaultVenueMIC, strings.Join(micsOf(venues), ", "))
	}
	if len(venues) > 1 {
		logger.Warn("oms: NO DEFAULT VENUE and more than one is configured — an order naming no "+
			"venue will be REFUSED rather than sent somewhere nobody chose. Set OMS_DEFAULT_VENUE_MIC "+
			"to pick one", "venues", strings.Join(micsOf(venues), ","))
	}
	return venueRouting{Router: router, Costs: costs}, nil
}
