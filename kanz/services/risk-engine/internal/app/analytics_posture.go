package app

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
// WHAT THIS DOES NOT DO IS REFUSE. Arming a refusal now would stop every pricing
// path in the platform, because eight of the ten analytics below have no
// benchmark recorded yet. That is the same shape as OMS_REQUIRE_VERIFIED_ACCOUNT
// and OMS_REQUIRE_MANDATE: build the control, count the gap, warn about it, and
// arm it WITH THE LIST IN HAND. #471 slice 3 is that arming, and it is a lead's
// call rather than this slice's.
//
// The posture is stated instead, and stated in a way that cannot go stale.

// AnalyticsPosture registers the validation-coverage gauge and states the gap
// once at startup.
//
// THE GAUGE IS THE DURABLE HALF. A startup log line is gone at the next rollout;
// the gauge is queryable the morning an auditor asks. It carries a series for
// EVERY analytic in the inventory, INCLUDING THE ZEROES — a metric that appeared
// only for validated analytics would report 100% coverage on a platform that had
// validated one thing, which is a worse answer than none.
func AnalyticsPosture(reg prometheus.Registerer, logger *slog.Logger, gate *validation.Gate) {
	inv := benchmarks.Inventory()
	reg.MustRegister(&analyticsPosture{gate: gate, inventory: inv})

	var validated, unvalidated []string
	for _, a := range inv {
		if _, state := analyticState(gate, a); state == stateValidated {
			validated = append(validated, a)
			continue
		}
		unvalidated = append(unvalidated, a)
	}
	sort.Strings(validated)
	sort.Strings(unvalidated)

	if len(unvalidated) == 0 {
		logger.Info("risk analytics: every analytic holds a current passing validation",
			"analytics", validated)
		return
	}
	// WARN, not Info. "Eight of the ten analytics pricing this book have never
	// been graded against an independent benchmark" is a sentence that must have
	// been read before anyone signs off on a risk number, and Info is where it
	// gets filtered out.
	logger.Warn("risk analytics: NOT every analytic is validated — the unvalidated ones still serve, "+
		"and an unchecked price looks exactly like a checked one",
		"validated", validated, "not_validated", unvalidated,
		"effect", "none yet: this counts the gap, it does not refuse (#471 slice 3 arms that, with the list in hand)")
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
)

func analyticState(gate *validation.Gate, analytic string) (float64, string) {
	r, ok := gate.Report(analytic)
	if !ok {
		return 0, stateAbsent
	}
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
}

var analyticsValidatedDesc = prometheus.NewDesc(
	"kanz_risk_analytics_validated",
	"1 when this analytic holds a current, passing, signed model validation; 0 otherwise. The "+
		"state label says why a zero is a zero — absent (never benchmarked), failed (benchmarked "+
		"and wrong) or expired (validated too long ago to still vouch for it). A zero does not "+
		"refuse anything yet; it is what the book is priced by regardless (#471).",
	[]string{"analytic", "state"}, nil,
)

func (p *analyticsPosture) Describe(ch chan<- *prometheus.Desc) { ch <- analyticsValidatedDesc }

func (p *analyticsPosture) Collect(ch chan<- prometheus.Metric) {
	for _, a := range p.inventory {
		v, state := analyticState(p.gate, a)
		ch <- prometheus.MustNewConstMetric(analyticsValidatedDesc, prometheus.GaugeValue, v, a, state)
	}
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
