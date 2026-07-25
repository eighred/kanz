package config

import (
	"strings"
	"testing"
)

// OPS-M2b. The /v1/control routes provision and drain nodes and write the exchange
// API credentials the venue adapters sign with. These cases pin who may reach them.
//
// The pairing is deliberate: the control plane is ABSENT unless an address is
// configured, and once it is present the role becomes mandatory and must be its own.
// "Exposed but unauthorized" and "authorized but unexposed" are both refused.

func baseValid() Config {
	return Config{
		JWTSecret:    "dev-secret",
		RequiredRole: "kanz-user",
		TradeRole:    "kanz-trader",
	}
}

func TestControlPlaneAbsentNeedsNoRole(t *testing.T) {
	// No OperatorAddr ⇒ the routes are never registered, so demanding a role for
	// them would be configuration for a feature that does not exist here.
	c := baseValid()
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a gateway with no control plane must not require an operator role: %v", err)
	}
}

func TestControlPlaneExposedRequiresARole(t *testing.T) {
	c := baseValid()
	c.OperatorAddr = "operator.kanz-operator.svc:9090"
	err := c.validateAuth()
	if err == nil {
		t.Fatal("exposing the control plane with no operator role was accepted. Node provisioning " +
			"and exchange-credential writes would then be reachable by whatever the surrounding " +
			"checks happened to allow — an omission, not a default.")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_OPERATOR_ROLE") {
		t.Errorf("error must name the variable to set; got: %v", err)
	}
}

func TestOperatorRoleMustNotBeTheBaselineRole(t *testing.T) {
	// Every authenticated caller carries the baseline role.
	c := baseValid()
	c.OperatorAddr = "operator.kanz-operator.svc:9090"
	c.OperatorRole = c.RequiredRole
	err := c.validateAuth()
	if err == nil {
		t.Fatal("operator role equal to the baseline role was accepted — that hands node " +
			"provisioning and credential writes to everyone who can read.")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_REQUIRED_ROLE") {
		t.Errorf("error must explain the collision; got: %v", err)
	}
}

func TestOperatorRoleMustNotBeTheTradeRole(t *testing.T) {
	// The separation runs BOTH ways, which is why this is its own case rather than a
	// variant of the one above: a trader must not be able to rotate the credentials
	// their own orders are signed with.
	c := baseValid()
	c.OperatorAddr = "operator.kanz-operator.svc:9090"
	c.OperatorRole = c.TradeRole
	err := c.validateAuth()
	if err == nil {
		t.Fatal("operator role equal to the trade role was accepted. Operating the estate and " +
			"moving capital are different authorities; collapsing them lets a trader rewrite the " +
			"venue keys their fills settle against.")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_TRADE_ROLE") {
		t.Errorf("error must explain the collision; got: %v", err)
	}
}

func TestControlPlaneFullyConfiguredIsAccepted(t *testing.T) {
	c := baseValid()
	c.OperatorAddr = "operator.kanz-operator.svc:9090"
	c.OperatorRole = "kanz-operator"
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a fully configured control plane was rejected: %v", err)
	}
}
