package arch

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStagedIdentityAdministrationRoleCannotOperateInfrastructure(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), "services/identity/deploy/admin-role-patch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var patch struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string
						Env  []struct {
							Name  string
							Value string
							Patch string `yaml:"$patch"`
						}
					}
				}
			}
		}
	}
	if err := yaml.Unmarshal(b, &patch); err != nil {
		t.Fatal(err)
	}
	role, removesLegacy := "", false
	for _, c := range patch.Spec.Template.Spec.Containers {
		if c.Name != "identity" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "IDENTITY_ADMIN_ROLE" {
				role = e.Value
			}
			if e.Name == "IDENTITY_OPERATOR_ROLE" && e.Patch == "delete" {
				removesLegacy = true
			}
		}
	}
	if role != "kanz-identity-admin" || !removesLegacy {
		t.Fatal("rollout must explicitly select isolated identity administration and remove legacy authority")
	}
	for _, key := range []string{"API_GATEWAY_REQUIRED_ROLE", "API_GATEWAY_TRADE_ROLE", "API_GATEWAY_OPERATOR_ROLE", "API_GATEWAY_FUND_ROLE", "API_GATEWAY_APPROVE_ROLE", "API_GATEWAY_MANDATE_ROLE", "API_GATEWAY_AUDIT_ROLE"} {
		if value, found := workloadEnv(t, "api-gateway", key); found && value == role {
			t.Fatalf("%s maps identity administration to gateway capability", key)
		}
	}
}
