package config

import (
	"strings"
	"testing"
)

// #539 / #410 ACT ONE. WHO MAY GIVE THE SECOND SIGNATURE.
//
// datamaster's maker-checker workflow — propose an override, sign it as a
// different person, list what is pending — was reachable by nobody: the gateway
// is its only permitted caller and routed none of the three endpoints. The
// routes are the repair; API_GATEWAY_APPROVE_ROLE is who may use them.
//
// The collisions below are the whole control. Every other role on this config
// tolerates overlap as a policy choice somebody could defend; this one cannot,
// and the reason it needs a test rather than a comment is that it FAILS SILENTLY.
// datamaster still refuses self-approval, so a shared role name produces a trail
// showing two distinct people signing — which is exactly what an auditor checks,
// and exactly what would be satisfied by two members of the same desk.

// approveBase is a config that passes validateAuth for reasons unrelated to
// approval, so a failure below is attributable to the approver plane.
func approveBase() Config {
	return Config{
		JWTSecret:     "dev-secret",
		AllowDevHS256: true,
		RequiredRole:  "kanz-user",
		TradeRole:     "kanz-trader",
	}
}

// THE UNCONFIGURED CASE IS A POSTURE, NOT AN OVERSIGHT. With no approver named
// the three override routes are not registered and the gateway starts: it still
// serves every read, every order and every login.
func TestApprovalAbsentNeedsNoRole(t *testing.T) {
	c := approveBase()
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a gateway with no approver must still start: %v", err)
	}
}

// AND WIRING DATAMASTER DOES NOT MAKE IT REQUIRED — the deliberate difference
// from the funding plane, whose address exists only to serve the funding route.
// DataMasterAddr also serves /v1/securities/{id}, /v1/prices/{id} and
// /v1/exceptions, so a read-only master-data deployment is legitimate. Forcing it
// to name an approver would make the pairing check a lie operators learn to
// satisfy with a placeholder — and a placeholder approver is worse than none,
// because it looks like the control is staffed.
func TestDataMasterWiredWithoutAnApproverStillStarts(t *testing.T) {
	c := approveBase()
	c.DataMasterAddr = "http://datamaster.kanz-services.svc:8080"
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a read-only master-data deployment must not be forced to name an approver: %v", err)
	}
}

func TestApproveRoleMustNotBeTheBaselineRole(t *testing.T) {
	c := approveBase()
	c.ApproveRole = c.RequiredRole

	err := c.validateAuth()
	if err == nil {
		t.Fatal("the baseline role was accepted as the approver. EVERY authenticated caller carries " +
			"it, so every user in the tenant becomes a signatory and any two of them clear each " +
			"other's overrides — four eyes that are the same two, twice.")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_APPROVE_ROLE") {
		t.Errorf("error must name the variable to fix; got: %v", err)
	}
}

// THE COLLISION THAT MATTERS. If the approver role IS the trade role, every
// trader holds the second signature on every other trader's proposal — and
// nothing downstream can tell, because internal/dualcontrol only checks that the
// approver is a different SUBJECT.
func TestApproveRoleMustNotBeTheTradeRole(t *testing.T) {
	c := approveBase()
	c.ApproveRole = c.TradeRole

	err := c.validateAuth()
	if err == nil {
		t.Fatal("the trade role was accepted as the approver. Two people on the same desk then " +
			"satisfy a control that exists to put a different function in the loop, and the audit " +
			"trail records it as genuine four-eyes.")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_TRADE_ROLE") {
		t.Errorf("error must name the colliding variable; got: %v", err)
	}
}

func TestApproveRoleMustNotBeTheOperatorRole(t *testing.T) {
	c := approveBase()
	c.OperatorAddr = "http://operator.kanz-services.svc:8080"
	c.OperatorRole = "kanz-operator"
	c.ApproveRole = c.OperatorRole

	err := c.validateAuth()
	if err == nil {
		t.Fatal("the operator role was accepted as the approver — an SRE who drains nodes is not " +
			"the second pair of eyes on the marks the book is valued at")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_OPERATOR_ROLE") {
		t.Errorf("error must name the colliding variable; got: %v", err)
	}
}

// THE ONE MOST LIKELY TO BE TYPED IN, because both are senior authorities and
// that is exactly how the approver drifts into a seniority badge rather than a
// separate function.
func TestApproveRoleMustNotBeTheFundRole(t *testing.T) {
	c := approveBase()
	c.AccountingAddr = "http://accounting.kanz-services.svc:8080"
	c.FundRole = "kanz-treasury"
	c.ApproveRole = c.FundRole

	err := c.validateAuth()
	if err == nil {
		t.Fatal("the fund role was accepted as the approver — the person who moves the fund's cash " +
			"would clear the prices the fund is valued at")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_FUND_ROLE") {
		t.Errorf("error must name the colliding variable; got: %v", err)
	}
}

// A DISTINCT APPROVER IS ACCEPTED. Without this the four refusals above are
// satisfied by a config that rejects every approver, which would be the same
// class of defect one layer up.
func TestADistinctApproveRoleIsAccepted(t *testing.T) {
	c := approveBase()
	c.OperatorAddr = "http://operator.kanz-services.svc:8080"
	c.OperatorRole = "kanz-operator"
	c.AccountingAddr = "http://accounting.kanz-services.svc:8080"
	c.FundRole = "kanz-treasury"
	c.ApproveRole = "kanz-compliance"

	if err := c.validateAuth(); err != nil {
		t.Fatalf("a distinct approver role was refused: %v", err)
	}
}

// An unset role must not collide with another unset role — the empty-string trap
// the fund plane already pins, arriving here through a fourth comparison.
func TestApproveRoleDoesNotCollideWithAnUnsetFundRole(t *testing.T) {
	c := approveBase()
	c.ApproveRole = "kanz-compliance"

	if err := c.validateAuth(); err != nil {
		t.Fatalf("an approver with no funder configured was refused: %v", err)
	}
}

func TestLoadReadsTheApproveRoleFromTheEnvironment(t *testing.T) {
	t.Setenv("API_GATEWAY_JWT_SECRET", "dev-secret")
	t.Setenv("API_GATEWAY_ALLOW_DEV_HS256", "true")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")
	t.Setenv("API_GATEWAY_APPROVE_ROLE", "kanz-compliance")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ApproveRole != "kanz-compliance" {
		t.Fatalf("ApproveRole = %q, want %q — API_GATEWAY_APPROVE_ROLE is not wired to the field, "+
			"so no deployment can name an approver and the override routes stay unregistered",
			cfg.ApproveRole, "kanz-compliance")
	}
}
