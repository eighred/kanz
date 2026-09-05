package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/eighred/kanz/test/load/internal/promscrape"
)

// preflight decides whether this run may happen at all, and returns the opening
// metrics scrape the report's control deltas are computed against.
//
// # Why a preflight is not paperwork here
//
// A read-path load test that is pointed at the wrong environment produces a
// wasted afternoon. A WRITE-path one places orders. The worst available outcome
// of this file being weak is a harness that silently drove a live venue, and
// there is no version of that which is recoverable by noticing afterwards.
//
// # Why it interrogates the system under test rather than the operator
//
// The obvious control is an env var — ORDERFLOW_I_KNOW_THIS_IS_A_SIMULATOR=true.
// It is worthless: it records the belief of the person running the load test,
// which is exactly the thing that is wrong in the case that matters. Every check
// below is a FACT THE SYSTEM UNDER TEST ASSERTS ABOUT ITSELF — its venue posture
// read from its own metrics, and its answer to a single real order. An operator
// who points this at a production gateway does not get to overrule any of them,
// because none of them is an input they control.
//
// # Fail closed, in every direction
//
// A metrics endpoint that cannot be reached, a metric family that is absent, a
// canary that is refused, a canary that is never announced — every one of those
// REFUSES. "Unknown" is the third value CLAUDE.md names and a critical unknown
// fails closed: this harness never proceeds on the strength of something it could
// not check.
func preflight(ctx context.Context, cfg config, s *submitter, l *ledger) (promscrape.Scrape, error) {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	before, err := promscrape.Fetch(pctx, cfg.metricsURL)
	if err != nil {
		return nil, fmt.Errorf("the OMS's metrics (%s) could not be read, so this harness cannot "+
			"establish whether an order submitted here reaches an exchange or a simulator. It will not "+
			"guess: %w", cfg.metricsURL, err)
	}
	if err := refuseUnlessSimulated(before); err != nil {
		return nil, err
	}
	if err := canary(pctx, cfg, s, l); err != nil {
		return nil, err
	}
	// Re-read AFTER the canary, so the canary's own effect on the watched counters
	// is inside the baseline rather than showing up as a delta the load caused.
	before, err = promscrape.Fetch(pctx, cfg.metricsURL)
	if err != nil {
		return nil, fmt.Errorf("the baseline metrics scrape failed after the canary: %w", err)
	}
	logPosture(cfg, before)
	return before, nil
}

// refuseUnlessSimulated is the check that stops a load test from trading.
//
// It reads the two gauges the OMS exports about its own venue posture (#865,
// services/oms/cmd/oms/venues.go). Three refusals, and the third one is the
// subtle one:
//
//   - the family is ABSENT — this OMS does not report its posture, so nothing
//     here knows what it routes to. Absent is UNKNOWN, and unknown fails closed.
//     It is also the shape an OMS predating the metric has, which is precisely
//     when a wrong answer would be most costly.
//   - live adapters > 0 — every order this harness submits could reach an
//     exchange. Refuse, naming the count.
//   - simulated venues == 0 with no live ones either — this OMS routes NOWHERE.
//     Nothing would trade, so it is safe; but every order would be refused and
//     the run would report a throughput number for the refusal path, which is the
//     #859 defect wearing a write-path costume. Refuse, and say which one it is.
func refuseUnlessSimulated(s promscrape.Scrape) error {
	live, okLive := s.Value("kanz_oms_live_venue_adapters")
	sim, okSim := s.Value("kanz_oms_simulated_venues")
	if !okLive || !okSim {
		return errors.New("this OMS does not export kanz_oms_live_venue_adapters / " +
			"kanz_oms_simulated_venues, so whether an order submitted here reaches an exchange is " +
			"UNKNOWN. A critical unknown fails closed. Either the metrics URL points at a different " +
			"process, or this OMS predates the venue-posture gauges (#865) — in which case upgrade it " +
			"rather than running load at an exchange you cannot rule out")
	}
	if live > 0 {
		return fmt.Errorf("this OMS holds %.0f LIVE venue adapter(s) (kanz_oms_live_venue_adapters). "+
			"Every order this harness submits would be placed at a real exchange, once per submission, "+
			"at the offered rate. Point it at a deployment with no OMS_VENUE_ENDPOINTS", live)
	}
	if sim < 1 {
		return errors.New("this OMS has NO venues at all: kanz_oms_live_venue_adapters and " +
			"kanz_oms_simulated_venues are both zero. Nothing would trade, and nothing would be " +
			"measured either — the router refuses a MIC it has no venue for, so every order would be " +
			"refused and the run would report the throughput of the refusal path. Set OMS_SIM_VENUE_MIC")
	}
	return nil
}

// canary submits ONE order and requires it to be admitted before any load starts.
//
// # Why one real order beats any amount of configuration checking
//
// #859 is the lesson this implements. test/load/config.js sent `?as_of=` on the
// measures read for months and every response was a 200, because the engine
// ignored the field: the harness reported green throughput for a contract nothing
// honoured. The same trap on the write path is worse and quieter — a gateway 202
// says the COMMAND was published, not that the order was admitted, so a run whose
// every order is refused by the pre-trade gate looks identical to a healthy one in
// every HTTP-level metric.
//
// The canary closes it end to end: an order goes through the real front door, and
// this harness waits for THAT ORDER'S OWN ORDER_ACCEPTED FACT to come back off the
// bus. Anything else — a non-202, a refusal, a silence — stops the run and names
// which of them happened, because the three send an operator to three different
// places.
func canary(ctx context.Context, cfg config, s *submitter, l *ledger) error {
	sub, code, body, err := s.submitAndRead(ctx)
	if err != nil {
		return fmt.Errorf("the canary order could not be submitted to %s: %w", cfg.baseURL, err)
	}
	switch code {
	case http.StatusAccepted:
	case http.StatusLocked:
		return fmt.Errorf("the platform is HALTED: the gateway answered 423 (%q). Every order would be "+
			"refused by the brake and the run would measure the brake", body)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the gateway refused the canary with http %d (%q). The token must carry the "+
			"deployment's REQUIRED role, its TRADE role, the tenant, and the portfolio entitlement — the "+
			"OMS denies an empty portfolio claim (#225), so a token minted without --portfolio "+
			"authenticates cleanly and then has every order refused NOT_ENTITLED", code, body)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("the gateway is READ-ONLY: it answered 503 (%q). It has no bus producer, so "+
			"no order it accepts can reach the OMS", body)
	default:
		return fmt.Errorf("the canary order was answered http %d (%q); a load run would produce that "+
			"answer at the offered rate and measure nothing else", code, body)
	}

	deadline := time.Now().Add(cfg.drainFor)
	for {
		if r, ok := l.lookup(sub.id); ok {
			if r.kind == subjectAccepted {
				log.Printf("orderflow: canary order %s ADMITTED in %s — the write path is live end to end",
					sub.id, time.Since(sub.at).Round(time.Millisecond))
				return nil
			}
			return fmt.Errorf("the canary order %s was REFUSED by the OMS (%s). A load run against a "+
				"refusing path measures the refusal, not admission — the usual causes are an "+
				"unentitled portfolio, an instrument the mandate forbids, a price the gate cannot "+
				"value, and a portfolio no mandate governs under OMS_REQUIRE_MANDATE", sub.id, r.kind)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the canary order %s was accepted by the gateway and produced NO terminal "+
				"FACT within %s. The command was published and admission did not answer: check that the "+
				"OMS consumes order.order.submit on this broker (a tenant-routed publish needs the "+
				"broker's tenant mapping — see test/backing/nats-dev.conf), that %s is the SAME process, "+
				"and that its outbox relay is running", sub.id, cfg.drainFor, cfg.metricsURL)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// logPosture prints what this stack is, ahead of the run, so a number is never
// read without the conditions that produced it.
//
// NOT A REFUSAL. A local rig has no security master, so the pre-trade gate's
// classifier is unwired and SECTOR/ISSUER/ASSET_CLASS mandate rules are refused
// as unverifiable rather than evaluated; refusing to run on that would mean this
// harness could only ever run against a full estate. Reporting it is the honest
// middle: the measured admission cost is the cost of the rules that DID run.
func logPosture(cfg config, s promscrape.Scrape) {
	sim, _ := s.Value("kanz_oms_simulated_venues")
	log.Printf("orderflow: posture — simulated venues=%.0f, live adapters=0, admission p99 budget=%s",
		sim, cfg.budget)
	// sum, NOT value: this family gained a `governance` label (#926) so it exports
	// one series per ungoverned state, and scrape.value documents that reaching it
	// with a labelled family is a caller error — it would report the first series
	// and call it the total. The number wanted here is the whole family.
	if v, _, present := s.Sum("kanz_compliance_ungoverned_orders_total"); present {
		log.Printf("orderflow: kanz_compliance_ungoverned_orders_total=%.0f at the start of the run. "+
			"If this moves, the portfolio is under NO mandate and the gate returns at its first "+
			"branch — the run would be measuring a short circuit", v)
	}
	if v, ok := s.Value("kanz_compliance_pretrade_seam_wired"); ok && v == 0 {
		log.Printf("orderflow: the pre-trade gate reports a MISSING SEAM " +
			"(kanz_compliance_pretrade_seam_wired=0). Rules depending on it PASS rather than refuse, " +
			"so the admission cost measured here is the cost of the rules that did run")
	}
}
