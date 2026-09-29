package config

import (
	"strings"
	"testing"
)

// A POSTURE FLAG PARSES OR THE SERVICE REFUSES TO START (#783).
//
// These five controls used to be read as os.Getenv(key) == "true", a raw string
// comparison. Every other spelling — "True", "TRUE", "1", "T", or a value a
// mounted secret file left whitespace on — evaluated to FALSE, with no error, no
// warning and no log line. An operator armed the control, read it back in the
// manifest, and deployed a pod that reported healthy and enforced nothing.
//
// THE SPELLINGS ARE NOT EXOTIC. "1" is what a shell script writes for a boolean,
// "TRUE" is what a spreadsheet exports, and `value: |` in YAML keeps the newline.
// Each of them silently disarmed a deny-by-default control on the capital path.
//
// env.Bool refuses a value strconv.ParseBool cannot read, which is AGENTS.md's
// rule that a misconfiguration surfaces as a refusal to start rather than a
// default that looks healthy. These tests pin all three halves of that: the
// spellings that must arm, the values that must refuse, and the absence that
// must still mean the documented default.

// postureKeys are the five OMS controls and the Config field each one sets.
var postureKeys = map[string]func(Config) bool{
	"OMS_REQUIRE_MANDATE":            func(c Config) bool { return c.RequireMandate },
	"OMS_REQUIRE_VENUE_ACCOUNT":      func(c Config) bool { return c.RequireVenueAccount },
	"OMS_REQUIRE_VERIFIED_ACCOUNT":   func(c Config) bool { return c.RequireVerifiedAccount },
	"OMS_REQUIRE_ORDER_TYPE_SUPPORT": func(c Config) bool { return c.RequireOrderTypeSupport },
	// OMS_REQUIRE_DUAL_CONTROL is deliberately absent: arming it without
	// OMS_DUAL_CONTROL_MIN_NOTIONAL is a startup refusal of its own, so it cannot
	// share this table's "arms cleanly" case. It has its own test below.
}

func TestEverySpellingGoAcceptsArmsTheControl(t *testing.T) {
	for key, read := range postureKeys {
		for _, spelling := range []string{"true", "TRUE", "True", "1", "t", "T"} {
			t.Run(key+"="+spelling, func(t *testing.T) {
				t.Setenv(key, spelling)
				cfg, err := Load()
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if !read(cfg) {
					t.Fatalf("%s=%q did not arm the control. Under the old raw comparison every "+
						"spelling but \"true\" read as FALSE, so an operator who wrote this got a "+
						"pod that reported healthy and enforced nothing (#783)", key, spelling)
				}
			})
		}
	}
}

func TestAValueThatIsNotABooleanRefusesToStart(t *testing.T) {
	for key := range postureKeys {
		for _, bad := range []string{"yes", "on", "enabled", "TRUE!", "0.0"} {
			t.Run(key+"="+bad, func(t *testing.T) {
				t.Setenv(key, bad)
				_, err := Load()
				if err == nil {
					t.Fatalf("%s=%q was ACCEPTED. A value nobody can parse must be a refusal to "+
						"start, not a silent false — the operator believes this control is armed",
						key, bad)
				}
				// NAMED, not merely refused. Load has many ways to fail, and "an
				// error came back" would pass with this check deleted.
				if !strings.Contains(err.Error(), key) {
					t.Errorf("refusal does not name %s: %q", key, err)
				}
				if !strings.Contains(err.Error(), bad) {
					t.Errorf("refusal does not quote the offending value %q: %q", bad, err)
				}
			})
		}
	}
}

// TestUnsetAndBlankBothMeanTheDocumentedDefault. Eight manifests carry
// `value: ""` to mean "not configured", and env.Lookup trims before deciding —
// so a blank value and an absent one must reach the same default. Treating a
// blank as unparseable would refuse to start on eight shipped manifests.
func TestUnsetAndBlankBothMeanTheDocumentedDefault(t *testing.T) {
	for key, read := range postureKeys {
		t.Run(key+" unset", func(t *testing.T) {
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if read(cfg) {
				t.Fatalf("%s is unset and the control armed itself. The documented default for "+
					"every one of these is FALSE, and arming on absence would refuse every order "+
					"for every portfolio nobody has corrected yet", key)
			}
		})
		t.Run(key+" blank", func(t *testing.T) {
			t.Setenv(key, "   ")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load refused a blank %s: %v — eight shipped manifests carry value: \"\" "+
					"to mean 'not configured', and this would stop all of them starting", key, err)
			}
			if read(cfg) {
				t.Fatalf("%s is blank and the control armed itself", key)
			}
		})
	}
}

// TestDualControlParsesBeforeItsPairingIsChecked is the sharpest case of the
// five, and the one where the old comparison suppressed its own alarm.
//
// Arming dual control without OMS_DUAL_CONTROL_MIN_NOTIONAL is a startup refusal
// (config_dualcontrol_test.go). Under the raw comparison, "True" did not merely
// disarm maker-checker — it also stopped that refusal firing, so the OMS came up
// with neither the control nor the complaint. Now the spelling arms it and the
// pairing check runs.
func TestDualControlParsesBeforeItsPairingIsChecked(t *testing.T) {
	t.Setenv("OMS_REQUIRE_DUAL_CONTROL", "TRUE")

	_, err := Load()
	if err == nil {
		t.Fatal("OMS_REQUIRE_DUAL_CONTROL=TRUE started with no threshold. Either the spelling was " +
			"read as false — in which case maker-checker is silently off — or the pairing refusal " +
			"stopped firing; both are the #783 defect")
	}
	if !strings.Contains(err.Error(), "OMS_DUAL_CONTROL_MIN_NOTIONAL") {
		t.Fatalf("refusal does not name the missing threshold: %q. That is the whole content of "+
			"the pairing check — the operator must learn WHICH variable completes the pair", err)
	}

	// And with the threshold, the same spelling arms it.
	t.Setenv("OMS_DUAL_CONTROL_MIN_NOTIONAL", "1000000 USD")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RequireDualControl {
		t.Fatal("OMS_REQUIRE_DUAL_CONTROL=TRUE with a threshold did not arm maker-checker")
	}
}
