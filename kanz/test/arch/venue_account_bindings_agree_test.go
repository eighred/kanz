package arch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE OMS AND THE RISK ENGINE MUST NAME THE SAME EXCHANGE ACCOUNTS (#408).
//
// Two services read the portfolio → exchange-account bindings, from two
// environment variables, in the same syntax, through the same parser:
//
//	OMS_VENUE_ACCOUNTS          which account an order may SPEND from
//	RISK_ENGINE_VENUE_ACCOUNTS  which account a portfolio is LIQUIDATED in
//
// # Why the second one exists at all, and why this guard is the price of it
//
// no_dark_measure_seam_test.go's exemption for RegisterMarginRisk refused this
// shape in advance, and the objection was the right one: "giving it a second
// copy would be a second answer that can disagree with the first — on precisely
// the mapping #415 made load-bearing".
//
// What makes the second copy admissible is not that the objection was wrong. It
// is that the disagreement is now a RED BUILD rather than a discovery. The
// alternative considered was relaying the join through a new per-portfolio FACT
// published by the OMS: that copies the whole margin payload onto the wire to
// carry one mapping, adds a hop that goes stale on its own schedule, and makes a
// risk measure depend on the OMS being alive. It trades a configuration
// duplication for a data duplication, which is the worse of the two.
//
// # What divergence costs, if it ever escapes this guard
//
// It does not fail. The engine measures a real account that is not the one the
// portfolio trades in, and reports a liquidation distance for collateral that is
// not backing the position. The visible symptom is the OTHER half: the account
// the OMS actually spends from is then unbound here, no adapter's observation
// matches it, and it ages out — so
// kanz_risk_margin_skipped_total{reason="margin_unknown"} climbs and the measure
// REFUSES rather than reporting a plausible number. Fail-closed, and still a
// control that has stopped working.
//
// # SCOPE, HONESTLY
//
// This checks the manifests IN GIT. A value injected at deploy time — a patched
// overlay, an operator's `kubectl set env`, a sealed secret — is outside it, and
// nothing here can see that. What it does guarantee is that the two specs
// COMMITTED to this repository cannot drift apart in a review, which is where
// the last four estate-config defects were introduced.
func TestVenueAccountBindingsAgreeAcrossServices(t *testing.T) {
	root := moduleRoot(t)
	deploy := filepath.Join(root, "infra", "deploy")

	// EACH PAIR IS ONE DEPLOYMENT UNIT. The platform pods read the base
	// manifests; a tenant's pods read its overlay, and the two must agree WITHIN
	// a unit rather than across the estate — acme's bindings are not the platform
	// deployment's bindings and are not meant to be.
	for _, pair := range []struct {
		unit string
		oms  string
		risk string
	}{
		{"platform", filepath.Join(deploy, "oms-deploy.yaml"), filepath.Join(deploy, "risk-engine-rollout.yaml")},
		{"acme", filepath.Join(deploy, "tenants", "acme", "oms-acme.yaml"),
			filepath.Join(deploy, "tenants", "acme", "risk-engine-acme.yaml")},
	} {
		t.Run(pair.unit, func(t *testing.T) {
			omsSpec, foundOMS := envValue(t, pair.oms, "OMS_VENUE_ACCOUNTS")
			riskSpec, foundRisk := envValue(t, pair.risk, "RISK_ENGINE_VENUE_ACCOUNTS")

			// NON-VACUITY. A renamed variable would leave this guard comparing two
			// absences and passing — the failure it exists to prevent, one level up.
			if !foundOMS {
				t.Fatalf("%s declares no OMS_VENUE_ACCOUNTS — this guard is comparing nothing",
					pair.oms)
			}
			if !foundRisk {
				t.Fatalf("%s declares no RISK_ENGINE_VENUE_ACCOUNTS. The risk engine would register "+
					"no LiquidationProximity, so a mandate naming it refuses every order it checks, "+
					"and this guard would silently stop comparing (#408 control 4)", pair.risk)
			}
			if omsSpec != riskSpec {
				t.Errorf("the %s unit binds different exchange accounts in the two services:\n"+
					"  OMS_VENUE_ACCOUNTS         = %q  (%s)\n"+
					"  RISK_ENGINE_VENUE_ACCOUNTS = %q  (%s)\n"+
					"The engine would measure a liquidation distance against collateral the OMS does "+
					"not spend from, while the account it DOES spend from is observed by nobody and "+
					"ages out to UNKNOWN. Neither half fails; one reports a number about the wrong "+
					"account and the other refuses.",
					pair.unit, omsSpec, pair.oms, riskSpec, pair.risk)
			}
		})
	}
}

// envValue reads one container environment variable's literal value out of a
// manifest, from any document and any container in it.
//
// PARSED, NOT GREPPED, for the reason this directory keeps relearning: a regexp
// matches the variable's name inside the comment block that explains it, and
// three guards in this repository have passed while checking a comment.
//
// found=false is returned for a variable that is absent OR sourced from
// elsewhere (valueFrom), because neither can be compared here — and the caller
// must treat that as a failure rather than as "they match".
func envValue(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	type container struct {
		Env []struct {
			Name      string     `yaml:"name"`
			Value     string     `yaml:"value"`
			ValueFrom *yaml.Node `yaml:"valueFrom"`
		} `yaml:"env"`
	}
	// The two manifests are shaped differently — a Deployment and an Argo Rollout
	// — and both put the pod spec under spec.template.spec, so one shape reads
	// both without either being special-cased.
	type doc struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers     []container `yaml:"containers"`
					InitContainers []container `yaml:"initContainers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var d doc
		if err := dec.Decode(&d); err != nil {
			break
		}
		cs := append(append([]container{}, d.Spec.Template.Spec.Containers...),
			d.Spec.Template.Spec.InitContainers...)
		for _, c := range cs {
			for _, e := range c.Env {
				if e.Name != name {
					continue
				}
				if e.ValueFrom != nil {
					return "", false
				}
				return e.Value, true
			}
		}
	}
	return "", false
}
