package app

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/benchmarks"
	"github.com/eighred/kanz/internal/validation"
)

// HOW MUCH OF THIS BOOK IS PRICED BY SOMETHING NOBODY CHECKED (#471).
//
// internal/validation is a complete SR 11-7 model-validation gate — signed,
// time-bound, deny-by-default — and until this slice it had ZERO importers
// outside its own tests. Every analytic in the quant library served production
// having never been graded against anything, and nothing anywhere said so.
//
// That is a compliance finding, not a code-quality one. A supervisor's question
// is not "is your Black-Scholes right", it is "show me the validation, its date,
// and who signed it". "It has good unit tests" is not an answer to that, because
// unit tests are written by the same people, from the same understanding, as the
// code — a shared misreading of a formula passes both.
//
// IT NOW REFUSES, AND ONLY BECAUSE THE LIST IS IN HAND (#471 slice 3).
//
// The earlier slices counted and warned, for the reason OMS_REQUIRE_MANDATE and
// OMS_REQUIRE_VERIFIED_ACCOUNT ship false: arming a control that refuses
// everything nobody has corrected yet is an outage dressed as a control, and
// eight of ten analytics were then unvalidated. Eleven of twelve now hold current
// passing validations, and the twelfth — isda_simm — is a NAMED exemption in
// benchmarks.ValidationExemptions() carrying the licence reason that makes it
// ungradeable here.
//
// SO THE REFUSAL COSTS NOTHING TODAY AND EVERYTHING TOMORROW. Nothing currently
// deployed fails it. What it stops is the next analytic added to the inventory
// without evidence, and the next case set deleted — each of which used to move
// one series on a gauge nobody was alerting on, and now stops the engine.
//
// WHY A NAMED EXEMPTION RATHER THAN "REQUIRE 11 OF 12". A count is satisfied by
// any eleven. It would accept a DIFFERENT analytic going unvalidated in silence,
// which is the exact failure the whole issue is about. See ValidationExemptions.

// ErrUnvalidatedAnalytic is returned when RISK_REQUIRE_VALIDATED_ANALYTICS is
// armed and an analytic holds no current passing validation and is not a named
// exemption. It reaches the composition root, which returns it from run — the
// service does not start.
var ErrUnvalidatedAnalytic = errors.New("risk analytics: analytic serves without a current validation")

// AnalyticsPosture registers the validation-coverage gauges, states the posture
// once at startup, and enforces it when require is true.
//
// THE GAUGE IS THE DURABLE HALF. A startup log line is gone at the next rollout;
// the gauge is queryable the morning an auditor asks. It carries a series for
// EVERY analytic in the inventory, INCLUDING THE ZEROES — a metric that appeared
// only for validated analytics would report 100% coverage on a platform that had
// validated one thing, which is a worse answer than none.
//
// IT REGISTERS BEFORE IT REFUSES, deliberately: on the refusal path the process
// is about to exit, but on every other path the collector must exist whatever
// else this function decides, and a registration ordered after a conditional is
// one edit away from being skipped on the branch that needs it most.
func AnalyticsPosture(reg prometheus.Registerer, logger *slog.Logger, gate *validation.Gate, require bool) error {
	inv := benchmarks.Inventory()
	exempt := benchmarks.ValidationExemptions()
	reg.MustRegister(&analyticsPosture{gate: gate, inventory: inv, exempt: exempt, required: require})

	var validated, unvalidated, exempted, staleExemptions []string
	states := map[string]string{}
	for _, a := range inv {
		_, state := analyticState(gate, exempt, a)
		states[a] = state
		switch state {
		case stateValidated:
			validated = append(validated, a)
			if _, ok := exempt[a]; ok {
				staleExemptions = append(staleExemptions, a)
			}
		case stateExempt:
			exempted = append(exempted, a)
		default:
			unvalidated = append(unvalidated, a)
		}
	}
	sort.Strings(validated)
	sort.Strings(unvalidated)
	sort.Strings(exempted)
	sort.Strings(staleExemptions)

	// A DEAD EXEMPTION IS A DEFECT, AND IT IS LOUD HERE AND FATAL IN THE BUILD.
	//
	// test/arch/analytics_inventory_test.go is the arm that actually retires the
	// entry: it fails the build the moment an exempted analytic gains evidence in
	// Reports(). This is the same check one layer out, for the build that got
	// past it — but it is an Error log rather than a refusal ON PURPOSE. The
	// condition means an analytic got BETTER; stopping a risk engine because
	// coverage improved would be the one refusal nobody would forgive, and the
	// repair (delete the entry) is a code change, not an operator action.
	for _, a := range staleExemptions {
		logger.Error("risk analytics: exemption is DEAD — this analytic now holds a validation and "+
			"no longer needs a licence to serve without one",
			"analytic", a, "reason_on_file", exempt[a],
			"fix", "delete the entry from benchmarks.ValidationExemptions(); the arch guard demands it")
	}

	if require && len(unvalidated) > 0 {
		// FAIL LOUDLY, AND NAME EVERY ONE. A refusal that names the first
		// offender sends an operator round the loop once per analytic; the whole
		// list is what makes the repair a single piece of work. The state is
		// carried per analytic because absent / failed / expired have completely
		// different repairs: write the benchmark, fix the analytic, revalidate.
		detail := make([]string, 0, len(unvalidated))
		for _, a := range unvalidated {
			detail = append(detail, a+"="+states[a])
		}
		logger.Error("risk analytics: REFUSING TO START — RISK_REQUIRE_VALIDATED_ANALYTICS is armed "+
			"and an analytic pricing this book holds no current, passing, signed validation",
			"unvalidated", detail, "validated", validated, "exempt", exempted)
		return fmt.Errorf("%w: %s (RISK_REQUIRE_VALIDATED_ANALYTICS is armed; record a benchmark "+
			"in internal/risk/benchmarks, or add a NAMED exemption with its reason to "+
			"benchmarks.ValidationExemptions() — there is no count threshold to satisfy)",
			ErrUnvalidatedAnalytic, strings.Join(detail, ", "))
	}

	// STATE WHICH POSTURE WAS DEPLOYED, ALWAYS, AND NEVER THE SAME LINE FOR BOTH.
	// "Every analytic is validated" and "validation is required" are different
	// facts, and an operator reading the log must not have to consult the config
	// to learn which one they have.
	if require {
		logger.Info("risk analytics: VALIDATION REQUIRED — an analytic without a current, passing, "+
			"signed validation stops this engine at startup",
			"validated", validated, "exempt_by_name", exempted,
			"exemptions", exempt)
		return nil
	}
	if len(unvalidated) == 0 {
		logger.Info("risk analytics: VALIDATION ADVISORY — every analytic holds a current passing "+
			"validation, and nothing would refuse if one stopped. Set "+
			"RISK_REQUIRE_VALIDATED_ANALYTICS=true to keep it that way",
			"validated", validated, "exempt_by_name", exempted)
		return nil
	}
	// WARN, not Info. "Some of the analytics pricing this book have never been
	// graded against an independent benchmark" is a sentence that must have been
	// read before anyone signs off on a risk number, and Info is where it gets
	// filtered out.
	logger.Warn("risk analytics: VALIDATION ADVISORY — NOT every analytic is validated, the "+
		"unvalidated ones still serve, and an unchecked price looks exactly like a checked one",
		"validated", validated, "not_validated", unvalidated, "exempt_by_name", exempted,
		"effect", "none: this counts the gap, it does not refuse",
		"fix", "record a benchmark in internal/risk/benchmarks, then set RISK_REQUIRE_VALIDATED_ANALYTICS=true to refuse instead")
	return nil
}

// Validation states. Exactly one is true for an analytic at a given moment, and
// they are DELIBERATELY NOT COLLAPSED into a bare 0: "failed its benchmark" and
// "nobody ever benchmarked it" are different findings with different responses,
// and this repository's rule is that "nothing configured" and "checked, and fine"
// must never look the same. The same argument applies one step further in —
// "checked, and WRONG" must not look like either.
const (
	stateValidated = "validated" // current, passing, signed
	stateFailed    = "failed"    // graded, and it did not reproduce its benchmark
	stateExpired   = "expired"   // it passed, but too long ago to still vouch for it
	stateAbsent    = "absent"    // no benchmark has ever been recorded
	// stateExempt is "no benchmark has ever been recorded, AND that is argued
	// for by name". It is a FIFTH state and not a flavour of absent, because
	// once the gate is armed the two have opposite operational meanings: absent
	// stops the engine at its next start, exempt does not. An operator alerting
	// on this metric needs to tell "a gap that will page me" from "the licensed
	// one somebody already reasoned about" without reading the source.
	//
	// ITS VALUE IS STILL 0. Exempt is not validated — a coverage ratio computed
	// off this gauge must not improve because something was excused.
	stateExempt = "exempt"
)

func analyticState(gate *validation.Gate, exempt map[string]string, analytic string) (float64, string) {
	r, ok := gate.Report(analytic)
	if !ok {
		if _, ex := exempt[analytic]; ex {
			return 0, stateExempt
		}
		return 0, stateAbsent
	}
	// EVIDENCE OUTRANKS THE EXEMPTION, in both directions. An exempted analytic
	// that has been graded reports what the grading FOUND — including `failed`.
	// Reporting it as exempt would hide a benchmarked-and-wrong analytic behind
	// its own licence, which is the worst reading of any of the five.
	err := gate.Promote(analytic)
	switch {
	case err == nil:
		return 1, stateValidated
	case !r.Passed:
		return 0, stateFailed
	case errors.Is(err, validation.ErrDenied):
		return 0, stateExpired
	default:
		// Promote grew a denial reason this switch does not know. Reporting it
		// as validated would be the one wrong direction, so it reads as absent.
		return 0, stateAbsent
	}
}

// analyticsPosture is a Collector rather than a gauge set at startup, and that
// is the whole point of it.
//
// A VALIDATION EXPIRES — DefaultValidity is 90 days, because SR 11-7 is about
// CURRENT validation and not a one-time sign-off. A gauge written once at boot
// would report validated=1 for as long as the process lived, sailing straight
// past the expiry it exists to enforce, and the day it started lying is the day
// nothing would change in the output. Evaluating the gate at SCRAPE time means
// the metric goes to zero by itself, on the day the evidence goes stale, with no
// ticker to schedule and nothing to forget to restart.
type analyticsPosture struct {
	gate      *validation.Gate
	inventory []string
	exempt    map[string]string
	required  bool
}

var analyticsValidatedDesc = prometheus.NewDesc(
	"kanz_risk_analytics_validated",
	"1 when this analytic holds a current, passing, signed model validation; 0 otherwise. The "+
		"state label says why a zero is a zero — absent (never benchmarked), failed (benchmarked "+
		"and wrong), expired (validated too long ago to still vouch for it), or exempt (never "+
		"benchmarked, by a named and argued licence). Whether a zero refuses anything depends on "+
		"kanz_risk_analytics_validation_enforced (#471).",
	[]string{"analytic", "state"}, nil,
)

// A GATE THAT IS ARMED AND A GATE THAT IS OFF MUST NOT LOOK THE SAME.
//
// Without this series, an estate where every analytic reads 1 is indistinguishable
// from one where RISK_REQUIRE_VALIDATED_ANALYTICS was never set — the metric above
// is identical in both, and the difference is entirely in what happens on the NEXT
// deploy. "Nothing configured" and "checked, and fine" looking alike is the exact
// thing this repository refuses, and a startup log line does not answer it because
// it is gone by the time anyone asks.
//
// It is emitted by the same collector rather than as a separate gauge so the two
// can never disagree about which process they describe.
var analyticsValidationEnforcedDesc = prometheus.NewDesc(
	"kanz_risk_analytics_validation_enforced",
	"1 when RISK_REQUIRE_VALIDATED_ANALYTICS is armed on this process, so an analytic without a "+
		"current passing validation (and without a named exemption) stops the engine at startup; "+
		"0 when the validation posture is advisory and an unvalidated analytic serves anyway. Not "+
		"derivable from kanz_risk_analytics_validated: a fully validated estate looks identical "+
		"under both postures (#471).",
	nil, nil,
)

func (p *analyticsPosture) Describe(ch chan<- *prometheus.Desc) {
	ch <- analyticsValidatedDesc
	ch <- analyticsValidationEnforcedDesc
}

func (p *analyticsPosture) Collect(ch chan<- prometheus.Metric) {
	for _, a := range p.inventory {
		v, state := analyticState(p.gate, p.exempt, a)
		ch <- prometheus.MustNewConstMetric(analyticsValidatedDesc, prometheus.GaugeValue, v, a, state)
	}
	enforced := 0.0
	if p.required {
		enforced = 1
	}
	ch <- prometheus.MustNewConstMetric(analyticsValidationEnforcedDesc, prometheus.GaugeValue, enforced)
}

// LoadValidations records the benchmark evidence this build carries into the
// gate, and is what makes the posture above non-empty.
//
// A BENCHMARK THAT FAILS IS STILL RECORDED. It is signed audit evidence that the
// analytic was graded and did not reproduce its published value — the gate simply
// never promotes on it, and the gauge reads "failed" rather than "absent". This
// is the distinction that would be lost by returning early on a bad result, and
// losing it would let a wrong analytic hide among the ones nobody has checked.
//
// It returns an error ONLY for a malformed case set, which is a defect in the
// benchmark package rather than a finding about an analytic. That is a crash at
// the composition root and should be: it means this build cannot state its own
// validation posture, and a risk engine that cannot say what it has validated
// must not quietly serve as though it had.
func LoadValidations(gate *validation.Gate, now func() time.Time) error {
	reports, err := benchmarks.Reports(now(), validation.HashSigner{})
	if err != nil {
		return fmt.Errorf("risk analytics: benchmark case sets are malformed: %w", err)
	}
	for _, r := range reports {
		if err := gate.Record(r); err != nil {
			return fmt.Errorf("risk analytics: record validation for %s: %w", r.Analytic, err)
		}
	}
	return nil
}
