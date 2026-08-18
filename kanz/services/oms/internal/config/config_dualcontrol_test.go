package config

// THE ARMING GATE FOR MAKER-CHECKER ON ORDER SUBMISSION (#410).
//
// These run Load with no I/O at all, which is the whole reason the refusal lives
// there rather than in main.go: the OMS_VENUE_ACCOUNTS refusal was once reached
// only AFTER the OMS had connected to NATS and opened Postgres, so the one
// guarantee the basket contract rested on had no test that ran it. Same class of
// control, same placement, same reasoning.

import (
	"strings"
	"testing"
)

// TestArmingWithoutAThresholdRefusesToStart is the owner's ruling verbatim: a
// default here would silently pick a number nobody chose, on a control whose
// entire purpose is that a human decided the number.
//
// IT ASSERTS THE NAMED ERROR, not merely that one was returned. Load has many
// ways to fail and "an error came back" would pass with the threshold check
// deleted, as long as anything else in Load refused.
func TestArmingWithoutAThresholdRefusesToStart(t *testing.T) {
	t.Setenv("OMS_REQUIRE_DUAL_CONTROL", "true")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted OMS_REQUIRE_DUAL_CONTROL=true with no threshold — dual control would " +
			"then apply above a number nobody chose, which is the one thing this control cannot do")
	}
	msg := err.Error()
	// BOTH VARIABLES BY NAME. An operator reading this line must not have to
	// guess which one to set, and must be able to see that the two belong
	// together — that is what the ruling asks for.
	for _, want := range []string{"OMS_REQUIRE_DUAL_CONTROL", "OMS_DUAL_CONTROL_MIN_NOTIONAL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name %s: %q", want, msg)
		}
	}
}

// TestArmingWithAThresholdIsNowAccepted is the inverse of the refusal this file
// used to assert.
//
// Until migrations/0009_order_proposals.sql existed, Load refused this
// combination outright, because arming with nowhere to put a held order would
// have REJECTED every order at or above the threshold rather than holding it.
// The proposals table is that place, so the flag is a real posture — and this
// test is what stops the refusal being reinstated by a merge, which would take
// the whole control offline while every gate below it still passed.
func TestArmingWithAThresholdIsNowAccepted(t *testing.T) {
	t.Setenv("OMS_REQUIRE_DUAL_CONTROL", "true")
	t.Setenv("OMS_DUAL_CONTROL_MIN_NOTIONAL", "1000000 USD")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load refused the armed posture: %v\n\nThe OMS now has somewhere to hold an order "+
			"awaiting approval (migrations/0009), so this is a supported configuration; refusing it "+
			"means the control cannot be turned on at all", err)
	}
	if !cfg.RequireDualControl {
		t.Fatal("OMS_REQUIRE_DUAL_CONTROL=true did not arm the gate — orders at or above the " +
			"threshold would be ADMITTED on one signature while the operator believes they are held")
	}
	if cfg.DualControlMinNotional == nil {
		t.Fatal("armed with a nil threshold — Decide would compare every order against nothing")
	}
}

// TestTheThresholdAloneIsTheSupportedPosture: the control is present, nothing is
// held, and every order is still admitted on one signature and counted. This is
// the state an operator deploys to calibrate.
func TestTheThresholdAloneIsTheSupportedPosture(t *testing.T) {
	t.Setenv("OMS_DUAL_CONTROL_MIN_NOTIONAL", "1000000 USD")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load refused a threshold with no arming flag, which is the ONLY posture that can be "+
			"deployed today: %v", err)
	}
	if cfg.DualControlMinNotional == nil {
		t.Fatal("threshold parsed to nil — the control would read as ABSENT while the operator has set it")
	}
	if got := cfg.DualControlMinNotional.RatString(); got != "1000000" {
		t.Errorf("threshold = %s, want 1000000", got)
	}
	if cfg.DualControlNotionalCurrency != "USD" {
		t.Errorf("currency = %q, want USD", cfg.DualControlNotionalCurrency)
	}
	if cfg.RequireDualControl {
		t.Error("a threshold alone must not arm the refusal — arming is a separate, explicit decision")
	}
}

// TestNoThresholdMeansTheControlIsAbsent. Absent is a real, deployable state and
// must not be an error: it is what every OMS in the estate runs today.
func TestNoThresholdMeansTheControlIsAbsent(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load refused the default configuration: %v", err)
	}
	if cfg.DualControlMinNotional != nil {
		t.Fatal("an unset OMS_DUAL_CONTROL_MIN_NOTIONAL produced a threshold — a default here is " +
			"exactly what the ruling forbids")
	}
	if cfg.DualControlNotionalCurrency != "" {
		t.Errorf("currency = %q with no threshold set; the two must be present or absent together",
			cfg.DualControlNotionalCurrency)
	}
}

// TestABareThresholdNumberIsRefused. This is the estate's first
// money-denominated config value and there is no FX on the admission path, so an
// amount with no currency would be compared against order notionals denominated
// in whatever OMS_BASE_CURRENCY happens to be — silently, and in whichever
// direction the rate runs.
func TestABareThresholdNumberIsRefused(t *testing.T) {
	t.Setenv("OMS_DUAL_CONTROL_MIN_NOTIONAL", "1000000")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a threshold with no currency — the number would be compared against " +
			"notionals it has no stated unit for")
	}
	if !strings.Contains(err.Error(), "currency") {
		t.Fatalf("refusal does not say the currency is missing: %q", err.Error())
	}
}

// TestAThresholdInAnotherCurrencyIsRefused. There is no FX conversion on the
// admission path (RISK-06) and the compliance engine never reads
// GetCurrencyCode() at all, so a JPY threshold against USD notionals is off by
// two orders of magnitude in the PERMISSIVE direction.
func TestAThresholdInAnotherCurrencyIsRefused(t *testing.T) {
	t.Setenv("OMS_BASE_CURRENCY", "USD")
	t.Setenv("OMS_DUAL_CONTROL_MIN_NOTIONAL", "1000000 JPY")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a JPY threshold against USD-valued orders — no FX layer exists on " +
			"this path to reconcile them, so the comparison is silently mis-scaled")
	}
	for _, want := range []string{"JPY", "USD", "OMS_BASE_CURRENCY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %s: %q", want, err.Error())
		}
	}
}

// TestANonPositiveThresholdIsRefused. Zero or negative puts EVERY order at or
// above the threshold — which is not "dual control on large orders" but "dual
// control on everything", arrived at by arithmetic rather than by a decision.
func TestANonPositiveThresholdIsRefused(t *testing.T) {
	for _, v := range []string{"0 USD", "-1 USD"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("OMS_DUAL_CONTROL_MIN_NOTIONAL", v)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %q as a threshold", v)
			}
			if !strings.Contains(err.Error(), "positive") {
				t.Fatalf("refusal does not say the threshold must be positive: %q", err.Error())
			}
		})
	}
}

// TestAnUnparseableThresholdIsAnErrorNotAnAbsence. Falling back to "no
// threshold" on a typo is the failure this repository names outright: "nothing
// configured" and "checked, and fine" must never look the same — and here the
// operator BELIEVES they configured it.
func TestAnUnparseableThresholdIsAnErrorNotAnAbsence(t *testing.T) {
	t.Setenv("OMS_DUAL_CONTROL_MIN_NOTIONAL", "1,000,000 USD")

	_, err := Load()
	if err == nil {
		t.Fatal("a malformed threshold was accepted — the operator would deploy believing the " +
			"control was calibrated while nothing was ever compared")
	}
	if !strings.Contains(err.Error(), "OMS_DUAL_CONTROL_MIN_NOTIONAL") {
		t.Fatalf("refusal does not name the variable: %q", err.Error())
	}
}
