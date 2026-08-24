package main

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/services/oms/internal/approval"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// MAKER-CHECKER ON ORDER SUBMISSION (#410, act three), AND ITS THREE POSTURES.
//
// # Why this moved out of runConsumers (#643, #713)
//
// It did not move because it was untidy. #713 added four lines of lifecycle
// wiring to the composition root — the decision recorder's shutdown drain, which
// genuinely cannot live in a builder — and
// test/arch/composition_root_length_test.go's ratchet REFUSED THEM: that function
// may shrink and may not grow. The guard's own message says what to do about it,
// and this is it. The budget was not raised.
//
// This block is the same shape as every other extraction #643 named: one
// construction, three mutually exclusive postures, three log lines and no test —
// so which posture a deployment is in was a thing you read the source to find
// out, on a control whose whole purpose is that a second person signs.
//
// # The three postures, and why none of them is a default
//
//	ENFORCING  an order at or above the threshold is HELD in order_proposals and
//	           does not trade until a DIFFERENT authenticated subject approves it.
//	           If nobody watches the pending queue, large orders expire unfilled —
//	           a trading outage, not a security posture, which is why the log says
//	           so at startup rather than leaving it to be discovered.
//	OBSERVING  a threshold is set and not enforced: orders are admitted on one
//	           signature and COUNTED, so a desk can size the queue before arming.
//	ABSENT     no threshold: every order, of any size, is committed on one
//	           person's authority.
//
// THE GATE IS BUILT IN ALL THREE. With no threshold every admitted order is still
// counted under posture="absent", so "this deployment has no dual control" is a
// series on a dashboard rather than a missing metric — and "nothing configured"
// and "this build has no gate" do not look the same either.

// buildDualControlGate wires the maker-checker gate and states its posture.
//
// It returns an error only for a configuration approval.NewGate refuses — a
// threshold that will not parse, or a currency it cannot value against. That is a
// startup failure by design: the alternative is an OMS that believes it is
// enforcing a control it never armed.
func buildDualControlGate(cfg config.Config, marks *mark.Source, reg prometheus.Registerer, logger *slog.Logger) (*approval.Gate, error) {
	gate, err := approval.NewGate(cfg.RequireDualControl, cfg.DualControlMinNotional, marks, reg)
	if err != nil {
		logger.Error("oms: dual-control gate refused its configuration", "err", err)
		return nil, err
	}
	switch {
	case gate.Armed():
		logger.Warn("oms: MAKER-CHECKER IS ENFORCING — an order at or above the threshold is HELD in "+
			"order_proposals and is NOT admitted until a second, different authenticated subject "+
			"approves it. Somebody must own the pending queue or these orders expire unfilled (#410)",
			"threshold", gate.Threshold().FloatString(2), "currency", cfg.DualControlNotionalCurrency,
			"ttl", dualcontrol.DefaultTTL.String(),
			"metric", "kanz_oms_order_signatures_total")
	case gate.Watching():
		logger.Info("oms: MAKER-CHECKER IS OBSERVING, NOT ENFORCING — orders at or above the threshold "+
			"are admitted on ONE signature and counted. Set OMS_REQUIRE_DUAL_CONTROL=true to hold them "+
			"once somebody owns the pending queue",
			"threshold", gate.Threshold().FloatString(2), "currency", cfg.DualControlNotionalCurrency,
			"metric", "kanz_oms_order_signatures_total")
	default:
		logger.Warn("oms: NO DUAL-CONTROL THRESHOLD — every order, of any size, is committed on one " +
			"person's authority (#410). Set OMS_DUAL_CONTROL_MIN_NOTIONAL (e.g. \"1000000 USD\") to " +
			"start counting how much of the flow would need a second signature")
	}
	return gate, nil
}

// announceMandatePosture states, at startup, whether an unmandated portfolio can
// trade.
//
// WHICH OF THESE TWO LINES IS IN THE LOG is the difference between "an unmandated
// portfolio trades unconstrained" and "an unmandated portfolio cannot trade at
// all", and nobody should have to read the config to find out which one they
// deployed.
func announceMandatePosture(cfg config.Config, logger *slog.Logger) {
	if cfg.RequireMandate {
		logger.Info("pre-trade compliance: MANDATE REQUIRED — an order for a portfolio with no mandate is REJECTED (MANDATE_MISSING)")
		return
	}
	logger.Warn("pre-trade compliance: MANDATE ADVISORY — an order for a portfolio with NO MANDATE is ADMITTED, unconstrained. " +
		"Put every live portfolio under mandate with kanz-mandate, or set OMS_REQUIRE_MANDATE=true to refuse instead")
}
