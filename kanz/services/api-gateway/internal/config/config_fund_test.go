package config

import (
	"strings"
	"testing"
)

// #535. WHO MAY MOVE THE FUND'S OWN CAPITAL.
//
// POST /v1/portfolios/{id}/cash-movements posts a subscription, a redemption or a
// fee to the book of record. It demands authz.Fund, and authz.Fund was carried by
// no role in the estate — so the route answered 403 to every principal that
// exists while reading, from outside, as a working control.
//
// The pairing mirrors the operator plane's, and these cases pin both directions:
// the funding surface is ABSENT unless a funder is named (404, not 403), and it
// cannot be EXPOSED without one (refuse to start). The collisions are the
// segregation of duties: the person who can move money is never the person who
// trades it.

// fundBase is a config that passes validateAuth for reasons unrelated to funding,
// so a failure below is attributable to the funding plane.
func fundBase() Config {
	return Config{
		JWTSecret:     "dev-secret",
		AllowDevHS256: true,
		RequiredRole:  "kanz-user",
		TradeRole:     "kanz-trader",
	}
}

// The UNCONFIGURED case, and it is the decision rather than an oversight: with no
// accounting upstream and no funder, the route is not registered and the gateway
// starts. Requiring the variable unconditionally would refuse to start the
// platform's sole ingress — every read, every order, every login — over a surface
// that is not wired at all.
func TestFundingAbsentNeedsNoRole(t *testing.T) {
	c := fundBase()
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a gateway that fronts no book of record must not require a fund role: %v", err)
	}
}

func TestFundingExposedRequiresARole(t *testing.T) {
	c := fundBase()
	c.AccountingAddr = "http://accounting.kanz-services.svc:8080"
	err := c.validateAuth()
	if err == nil {
		t.Fatal("fronting the book of record with NO fund role was accepted. authz.Fund would then " +
			"be carried by nobody, so POST /v1/portfolios/{id}/cash-movements answers 403 to every " +
			"principal that exists — a total outage of the capability wearing the shape of a control " +
			"(#535).")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_FUND_ROLE") {
		t.Errorf("error must name the variable to set; got: %v", err)
	}
}

func TestFundRoleMustNotBeTheBaselineRole(t *testing.T) {
	c := fundBase()
	c.FundRole = c.RequiredRole
	err := c.validateAuth()
	if err == nil {
		t.Fatal("fund role equal to the baseline role was accepted — every authenticated caller " +
			"carries the baseline role, so that lets anyone who can read post a redemption against " +
			"the IBOR.")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_REQUIRED_ROLE") {
		t.Errorf("error must explain the collision; got: %v", err)
	}
}

// THE ONE THAT MATTERS MOST, and the reason authz.Fund is not authz.Trade: one
// credential that can both bring the fund's cash in and spend it is the control
// every auditor of a fund asks about first.
func TestFundRoleMustNotBeTheTradeRole(t *testing.T) {
	c := fundBase()
	c.FundRole = c.TradeRole
	err := c.validateAuth()
	if err == nil {
		t.Fatal("fund role equal to the trade role was accepted. The person who can move money is " +
			"never the person who trades it; collapsing them hands every strategy operator the " +
			"authority to book a redemption against the book of record.")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_TRADE_ROLE") {
		t.Errorf("error must explain the collision; got: %v", err)
	}
}

func TestFundRoleMustNotBeTheOperatorRole(t *testing.T) {
	c := fundBase()
	c.OperatorAddr = "operator.kanz-operator.svc:9090"
	c.OperatorRole = "kanz-operator"
	c.FundRole = c.OperatorRole
	err := c.validateAuth()
	if err == nil {
		t.Fatal("fund role equal to the operator role was accepted. Operating the estate is " +
			"draining nodes and rotating credentials; an SRE holding it must not be able to move " +
			"the fund's cash.")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_OPERATOR_ROLE") {
		t.Errorf("error must explain the collision; got: %v", err)
	}
}

func TestFundingFullyConfiguredIsAccepted(t *testing.T) {
	c := fundBase()
	c.AccountingAddr = "http://accounting.kanz-services.svc:8080"
	c.FundRole = "kanz-treasury"
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a fully configured funding surface was rejected: %v", err)
	}
}

// An UNSET operator role must not collide with a NAMED fund role. Both are
// optional, and Go's zero value would make "" == "" a collision if the check were
// written without the guard — refusing a perfectly ordinary deployment that
// fronts a book of record and no control plane.
func TestFundRoleDoesNotCollideWithAnUnsetOperatorRole(t *testing.T) {
	c := fundBase()
	c.FundRole = "kanz-treasury"
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a named fund role with no control plane configured was rejected: %v", err)
	}
}

// Load reads it from the environment under the name the deploy manifest uses. A
// field wired to the wrong key is a role nobody can ever set.
func TestLoadReadsTheFundRoleFromTheEnvironment(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_JWT_SECRET", "dev-secret")
	t.Setenv("API_GATEWAY_ALLOW_DEV_HS256", "true")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")
	t.Setenv("API_GATEWAY_FUND_ROLE", "kanz-treasury")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FundRole != "kanz-treasury" {
		t.Fatalf("FundRole = %q, want %q — API_GATEWAY_FUND_ROLE is not wired to the field, so no "+
			"deployment can name a funder", cfg.FundRole, "kanz-treasury")
	}
}
