package config

import (
	"strings"
	"testing"
)

func TestIdentityAdministratorCannotBeMappedToGatewayAuthority(t *testing.T) {
	for _, name := range []string{"baseline", "trade", "infrastructure", "fund", "approval", "mandate", "audit"} {
		t.Run(name, func(t *testing.T) {
			c := baseValid()
			roles := map[string]*string{"baseline": &c.RequiredRole, "trade": &c.TradeRole, "infrastructure": &c.OperatorRole, "fund": &c.FundRole, "approval": &c.ApproveRole, "mandate": &c.MandateRole, "audit": &c.AuditRole}
			*roles[name] = "kanz-identity-admin"
			if err := c.validateAuth(); err == nil || !strings.Contains(err.Error(), "reserved for identity") {
				t.Fatalf("collision accepted or wrong failure: %v", err)
			}
		})
	}
}
