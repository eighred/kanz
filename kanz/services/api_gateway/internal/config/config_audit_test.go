package config

import (
	"strings"
	"testing"
)

// #627. WHO MAY READ THE RECORD OF WHO DID WHAT.
//
// The audit read surface had no caller and no route: the only peer any policy
// admitted to its port was kanz-observability, via the rule that must admit
// whatever port serves /metrics. Fronting it through the gateway is the repair,
// and these are the two configurations that would undo it — one by exposing a
// surface nobody can use, one by exposing it to everybody.

// auditBase is a config that passes validateAuth for reasons unrelated to the
// audit plane, so a failure below is attributable to it.
func auditBase() Config {
	return Config{
		JWTSecret:     "dev-secret",
		AllowDevHS256: true,
		RequiredRole:  "kanz-user",
		TradeRole:     "kanz-trader",
	}
}

// THE UNCONFIGURED CASE IS A POSTURE, NOT AN OVERSIGHT. With no reader named the
// six routes are not registered and the gateway starts: it still serves every
// read, every order and every login.
func TestAnAuditPlaneAbsentNeedsNoRole(t *testing.T) {
	c := auditBase()
	if err := c.validateAuth(); err != nil {
		t.Fatalf("a gateway with no audit reader must still start: %v", err)
	}
}

// FRONTING AUDIT WITHOUT A READER IS REFUSED (#535). That listener serves the six
// compliance reads and nothing else, so naming the address without naming a
// reader exposes a surface every principal that exists gets a 403 from — a total
// outage of the capability wearing a strict control's costume.
func TestAuditWiredWithoutAReaderIsRefused(t *testing.T) {
	c := auditBase()
	c.AuditAddr = "https://audit.kanz-services.svc:8102"

	err := c.validateAuth()
	if err == nil {
		t.Fatal("a gateway fronting audit with no API_GATEWAY_AUDIT_ROLE started. Those routes " +
			"serve the tenant's compliance record, and with no role carrying authz.Audit every " +
			"one of them answers 403 to every principal that exists (#535)")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_AUDIT_ROLE") {
		t.Fatalf("the refusal must name the key an operator has to set, got: %v", err)
	}
}

// THE ONE COLLISION THIS CAPABILITY CANNOT SURVIVE. The baseline role is carried
// by EVERY authenticated caller, so naming it here does not grant an authority —
// it deletes one, and serves every principal in the tenant the complete record of
// every other principal's actions.
func TestAnAuditRoleThatIsTheBaselineRoleIsRefused(t *testing.T) {
	c := auditBase()
	c.AuditRole = c.RequiredRole

	err := c.validateAuth()
	if err == nil {
		t.Fatal("a gateway whose audit reader IS the baseline role started. Every authenticated " +
			"caller carries the baseline, so the whole tenant reads which trader was refused by " +
			"the pre-trade gate, who overrode a price, and who signed a mandate change")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_AUDIT_ROLE") {
		t.Fatalf("the refusal must name the key an operator has to change, got: %v", err)
	}
}

// AND THE COLLISIONS THAT ARE DELIBERATELY NOT REFUSED, which is a decision
// rather than an omission (#627).
//
// Every other capability here refuses to share a role name because two
// authorities that can act would merge. Reading the record is not acting in it:
// an auditor who also operates the estate holds one authority more than an
// auditor, not one control less. A firm small enough to have a single compliance
// officer should not have to invent a second person to satisfy the gateway, and
// forcing it would be answered with a shared token rather than a second hire.
//
// The trade case is the sharper one and it is allowed for the same reason — a
// desk head who supervises the traders they also trade alongside is an ordinary
// arrangement, and it is the FIRM's judgement, not this file's.
func TestAnAuditRoleMayShareTheOperatorAndTradeRoles(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  func() Config
	}{
		{"operator", func() Config {
			c := auditBase()
			c.OperatorRole = "kanz-operator"
			c.AuditRole = "kanz-operator"
			return c
		}},
		{"trade", func() Config {
			c := auditBase()
			c.AuditRole = c.TradeRole
			return c
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.cfg().validateAuth(); err != nil {
				t.Fatalf("sharing the audit role with the %s role must be the deployment's call, "+
					"not a refusal to start: %v", c.name, err)
			}
		})
	}
}
