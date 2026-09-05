package main

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/fxfeed"
)

// WHETHER THIS POD CAN VALUE A FOREIGN HOLDING AT ALL (#1041).
//
// buildLiveFX fails the start on a MALFORMED FX spec and returns (nil, nil) on
// an ABSENT one. That asymmetry is the shape this repository has ruled against:
// a typo is loud and a missing configuration is silent, so the deployment that
// values nothing looks exactly like the deployment that has nothing foreign to
// value. And the absent case is not hypothetical — no manifest in this
// repository sets ACCOUNTING_FX_PAIRS, ACCOUNTING_FX_SUBJECTS or
// ACCOUNTING_INSTRUMENT_CURRENCY, so it is the state of every pod that runs.
//
// WHAT CHANGED UNDERNEATH IT. The NAV path used to answer such a pod's requests
// from a domestic fast path that dropped every non-reporting cash bucket and
// summed foreign positions un-converted; it now refuses, naming the currency.
// That makes the consequence visible AT THE FIRST FOREIGN HOLDING, which is
// better than a wrong number and still worse than knowing at rollout — an
// operator learns their fund's headline number is unavailable from a failed
// valuation rather than from a deployment they could have fixed first.
//
// SO THE POSTURE IS STATED AT STARTUP: one gauge series per FX input, set to 0
// when nothing configures it, plus a WARN naming the variable that would arm it.
// A pod with FX off is a legitimate, common configuration — a single-currency
// fund needs none of it — so this is a POSTURE, not an error, exactly as
// kanz_accounting_entry_source_wired is for the unproduced entry types.
//
// THE GAUGE IS THE DURABLE HALF. A startup line is gone at the next rollout;
// `kanz_accounting_fx_wired{input="rates"} == 0` is queryable at 3am when a desk
// asks why the NAV endpoint is refusing a portfolio that traded into EUR.
//
// WHAT IT DELIBERATELY DOES NOT DO IS INVENT A DEFAULT PAIR SET. Which
// currencies a fund values in is an owner decision, and a guessed EURUSD would
// put a fabricated rate under a client's NAV — the #345 ground this repository
// already applies to invented quotes. The posture makes the decision visible; it
// does not make it.

// fxInput is one FX configuration input and what its absence costs.
type fxInput struct {
	// configured reports whether this deployment supplies the input.
	configured bool
	// what the input feeds when it is present.
	what string
	// why its absence matters, in operational terms.
	why string
	// arm names the environment variable that turns it on.
	arm string
}

// fxInputs describes every FX input the valuation path depends on, read off the
// SAME config fields buildLiveFX reads. Deriving the answer from the config
// rather than from a second copy of the parsing is what keeps the gauge and the
// behaviour from drifting: a pod that reports rates=1 is a pod where
// fxfeed.ParsePairs returned pairs, because it is the same call.
func fxInputs(cfg config.Config) map[string]fxInput {
	pairs, perr := fxfeed.ParsePairs(cfg.FXPairs)
	instr, ierr := fxfeed.ParsePairs(cfg.InstrumentCurrency)
	return map[string]fxInput{
		"rates": {
			configured: perr == nil && len(pairs) > 0,
			what: "the live FX cache: market.v1 quotes folded into a rate per foreign currency, " +
				"which is what converts a foreign cash, accrued or position leg into the reporting currency",
			why: "with no rates this pod values a DOMESTIC book only. A portfolio that holds any other " +
				"currency has no NAV at all: the valuation refuses by name rather than understating, " +
				"so the fund's headline number for that portfolio is unavailable until a rate exists",
			arm: "ACCOUNTING_FX_PAIRS",
		},
		"instrument_currency": {
			configured: ierr == nil && len(instr) > 0,
			what:       "the security-master join: which reference currency each instrument's price is quoted in",
			why: "with no join EVERY instrument is assumed quoted in the reporting currency. A foreign " +
				"position is then summed into the total as though it were domestic, and nothing in the " +
				"book can contradict it — the position leg is the half a currency check cannot infer",
			arm: "ACCOUNTING_INSTRUMENT_CURRENCY",
		},
	}
}

// stateFXPosture registers kanz_accounting_fx_wired with one series per FX input
// and says out loud which of them this deployment does not configure. It returns
// the unconfigured input names, sorted — the same list it logs — so a caller can
// assert on it.
func stateFXPosture(reg prometheus.Registerer, logger *slog.Logger, cfg config.Config) []string {
	return seedFXPosture(reg, logger, fxInputs(cfg))
}

// seedFXPosture is the half that does not read config, split out so a test can
// hand it an input map directly — the same seam stateEntrySourcePosture keeps,
// and for the same reason.
func seedFXPosture(reg prometheus.Registerer, logger *slog.Logger, inputs map[string]fxInput) []string {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kanz_accounting_fx_wired",
		Help: "1 when this deployment configures this FX input, 0 when it does not. A zero is a " +
			"posture, not an error — a single-currency fund needs none of it — but a pod with " +
			"rates=0 REFUSES the NAV of any portfolio holding a second currency (#1041).",
	}, []string{"input"})
	// REGISTERED UNCONDITIONALLY, not inside the branch that builds the FX cache.
	// A collector registered behind `if fx != nil` exports NO series in exactly
	// the deployment this posture exists to describe, and an alert written over an
	// absent series is silent rather than firing.
	reg.MustRegister(g)

	var unconfigured []string
	for name, in := range inputs {
		if in.configured {
			g.WithLabelValues(name).Set(1)
			continue
		}
		g.WithLabelValues(name).Set(0)
		unconfigured = append(unconfigured, name)
	}
	sort.Strings(unconfigured)

	if len(unconfigured) == 0 {
		logger.Info("accounting: FX is configured — this pod can value a book in more than one currency")
		return unconfigured
	}
	for _, name := range unconfigured {
		in := inputs[name]
		// WARN, not Info. "This deployment cannot value a foreign holding" is the
		// sentence an operator needs to have read BEFORE a desk books a EUR
		// subscription, and Info is where it would be filtered out.
		logger.Warn("accounting: NOTHING CONFIGURES this FX input — the valuation path runs without it",
			"input", name, "feeds", in.what, "why", in.why, "would_arm_it", in.arm,
			"gauge", "kanz_accounting_fx_wired{input=\""+name+"\"}=0")
	}
	return unconfigured
}
