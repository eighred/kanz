package arch

import "testing"

// TestOperatorVenueSecretRoleIsBounded fails the build if the S4a venue-secret Role
// grants anything beyond {get,create,update} on the secrets resource in a single
// namespace — a wider grant (list, patch, delete, a different resource, or a
// ClusterRole) would let the API-Manager write path reach past venue credentials.
// The verb set mirrors KubeStore.SetVenueKeys's actual Get-then-Create/Update
// sequence (kube.go) — there is no server-side apply and no list in the code path.
func TestOperatorVenueSecretRoleIsBounded(t *testing.T) {
	docs := decodeOperatorManifest(t)
	allowedVerbs := map[string]bool{"get": true, "create": true, "update": true}

	var found bool
	for _, d := range docs {
		if d.Kind != "Role" || d.Metadata.Name != "operator-venue-secret-writer" {
			continue
		}
		found = true
		if d.Metadata.Namespace == "" {
			t.Error("operator-venue-secret-writer must be namespaced, not cluster-scoped")
		}
		for _, rule := range d.Rules {
			for _, res := range rule.Resources {
				if res != "secrets" {
					t.Errorf("venue-secret role grants resource %q, want only secrets", res)
				}
			}
			for _, g := range rule.APIGroups {
				if g != "" {
					t.Errorf("venue-secret role grants apiGroup %q, want only core", g)
				}
			}
			for _, v := range rule.Verbs {
				if !allowedVerbs[v] {
					t.Errorf("venue-secret role grants verb %q outside {get,create,update}", v)
				}
			}
		}
	}
	if !found {
		t.Fatal("operator-venue-secret-writer Role not found in operator-deploy.yaml")
	}
}
