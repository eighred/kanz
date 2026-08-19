package config

import (
	"strings"
	"testing"
)

// #562 / #410 ACT TWO. WHO MAY CHANGE WHAT GOVERNS A PORTFOLIO.
//
// A mandate is the control every order is checked against, so this is the plane
// where a shared role name is worst: it does not merge two unrelated authorities,
// it merges the two halves of ONE escalation — relax the constraint that would
// have refused an order, then clear the order it would have refused.
//
// Every case below fails SILENTLY without the check. compliance still refuses
// self-approval on each act individually, so both records show two distinct
// people; nothing anywhere compares them, and the trail an auditor reads looks
// exactly like a working control.

// mandateBase is a config that passes validateAuth for reasons unrelated to the
// mandate plane, so a failure below is attributable to it.
func mandateBase() Config {
	return Config{
		JWTSecret:     "dev-secret",
		AllowDevHS256: true,
		RequiredRole:  "kanz-user",
		TradeRole:     "kanz-trader",
	}
}

// THE UNCONFIGURED CASE IS A POSTURE, NOT AN OVERSIGHT. With no signatory named
// the three mandate routes are not registered and the gateway starts: it still
// serves every read, every order and every login. cmd/kanz-mandate remains the
// path such a deployment has.
func TestAMandatePlaneAbsentNeedsNoRole(t *testing.T) {
	c := mandateBase()
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a gateway with no mandate signatory must still start: %v", err)
	}
}

// FRONTING COMPLIANCE WITHOUT A SIGNATORY IS REFUSED, and this is the deliberate
// difference from the APPROVE plane. DataMasterAddr also serves three read routes,
// so a datamaster with no approver is a legitimate posture. ComplianceAddr points
// at compliance's mandate listener, which serves the three mandate routes and
// nothing else — so setting it without naming a signatory exposes a surface every
// principal that exists gets a 403 from, which is #535 exactly.
func TestComplianceWiredWithoutASignatoryIsRefused(t *testing.T) {
	c := mandateBase()
	c.ComplianceAddr = "https://compliance.kanz-services.svc:8095"

	err := c.validateAuth()
	if err == nil {
		t.Fatal("a gateway fronting compliance with no API_GATEWAY_MANDATE_ROLE started. Those " +
			"routes change the mandate every order is checked against, and with no role carrying " +
			"authz.Mandate they answer 403 to EVERY principal that exists — a total outage of the " +
			"capability that reads as a strict control (#535)")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_MANDATE_ROLE") {
		t.Errorf("error must name the variable to fix; got: %v", err)
	}
}

// THE COLLISION #562 EXISTS TO PREVENT, and the one #539 predicted would not be a
// collision at all.
//
// One signatory holding both gives the second signature on relaxing a mandate AND
// the second signature on the order that mandate would have refused. Each act
// passes its own self-approval check, each record names two people, and the
// escalation is invisible in exactly the trail somebody would audit.
func TestAMandateRoleThatIsAlsoTheApproverIsRefused(t *testing.T) {
	c := mandateBase()
	c.ApproveRole = "kanz-compliance"
	c.MandateRole = "kanz-compliance"

	err := c.validateAuth()
	if err == nil {
		t.Fatal("one role was accepted as BOTH the order approver and the mandate signatory. " +
			"That person can sign away the limit and then sign the trade the limit existed to " +
			"stop — two acts, each individually correct, and nothing compares them (#562)")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_MANDATE_ROLE") ||
		!strings.Contains(err.Error(), "API_GATEWAY_APPROVE_ROLE") {
		t.Errorf("error must name BOTH variables, since either could be the one to change; got: %v", err)
	}
}

func TestAMandateRoleThatIsTheBaselineRoleIsRefused(t *testing.T) {
	c := mandateBase()
	c.MandateRole = c.RequiredRole

	err := c.validateAuth()
	if err == nil {
		t.Fatal("the baseline role was accepted as the mandate signatory. EVERY authenticated " +
			"caller carries it, so any two users in the tenant can rewrite what governs a " +
			"portfolio and the pre-trade gate then enforces whatever they agreed between them")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_MANDATE_ROLE") {
		t.Errorf("error must name the variable to fix; got: %v", err)
	}
}

func TestAMandateRoleThatIsTheTradeRoleIsRefused(t *testing.T) {
	c := mandateBase()
	c.MandateRole = c.TradeRole

	err := c.validateAuth()
	if err == nil {
		t.Fatal("the trade role was accepted as the mandate signatory. A trader who can change " +
			"the mandate does not need to break the pre-trade gate, only to widen it")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_MANDATE_ROLE") {
		t.Errorf("error must name the variable to fix; got: %v", err)
	}
}

// THE REMAINING TWO, ASSERTED TOGETHER because they share one argument: an
// authority over the estate or over the fund's cash is not an authority over what
// the fund may hold, and both drift into being a seniority badge.
func TestAMandateRoleThatCollidesWithOperatorOrFundIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		with func(*Config)
	}{
		{"operator", func(cfg *Config) {
			cfg.OperatorAddr = "operator.kanz-operator.svc:9091"
			cfg.OperatorRole = "kanz-sre"
			cfg.MandateRole = "kanz-sre"
		}},
		{"fund", func(cfg *Config) {
			cfg.AccountingAddr = "https://accounting.kanz-services.svc:8101"
			cfg.FundRole = "kanz-treasury"
			cfg.MandateRole = "kanz-treasury"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := mandateBase()
			c.with(&cfg)

			err := cfg.validateAuth()
			if err == nil {
				t.Fatalf("the %s role was accepted as the mandate signatory", c.name)
			}
			if !strings.Contains(err.Error(), "API_GATEWAY_MANDATE_ROLE") {
				t.Errorf("error must name the variable to fix; got: %v", err)
			}
		})
	}
}

// A FULLY SEGREGATED DEPLOYMENT STARTS. Without this every case above is
// satisfied by a validateAuth that refuses everything, and the plane would be
// unconfigurable rather than strict — the direction that produces a config nobody
// can satisfy and a check somebody deletes.
func TestAllFiveRolesDistinctIsAValidDeployment(t *testing.T) {
	c := mandateBase()
	c.OperatorAddr = "operator.kanz-operator.svc:9091"
	c.OperatorRole = "kanz-sre"
	c.AccountingAddr = "https://accounting.kanz-services.svc:8101"
	c.FundRole = "kanz-treasury"
	c.ApproveRole = "kanz-compliance"
	c.ComplianceAddr = "https://compliance.kanz-services.svc:8095"
	c.MandateRole = "kanz-mandate-officer"

	if err := c.validateAuth(); err != nil {
		t.Fatalf("a deployment naming five distinct roles was refused: %v", err)
	}
}
