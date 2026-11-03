package arch

import "testing"

// Ingress rules add together. A second matching policy can undo the isolation
// even when the intended gateway and classifier policies remain unchanged.
func TestDatamasterRestrictedListenersHaveDistinctPeers(t *testing.T) {
	docs := allNetworkPolicies(t)
	for port, want := range map[int]map[string]bool{8111: {"api-gateway": true}, 8112: {"oms": true, "compliance": true, "risk-engine": true}} {
		seen := map[string]bool{}
		for _, d := range docs {
			if d.Metadata.Namespace != "kanz-services" || !(selectorIsWildcard(d) || selectsApp(d, "datamaster")) {
				continue
			}
			for _, rule := range d.Spec.Ingress {
				if !rulePermitsPort(rule, port) {
					continue
				}
				if len(rule.From) == 0 {
					t.Errorf("%s admits every source on %d", d.Metadata.Name, port)
				}
				for _, peer := range rule.From {
					if peer.NamespaceSelector != nil || peer.IPBlock != nil || peer.PodSelector == nil {
						t.Errorf("%s admits %s on %d", d.Metadata.Name, peer.describe(), port)
						continue
					}
					app := peer.PodSelector.MatchLabels["app"]
					if !want[app] {
						t.Errorf("%s permits unexpected app %q on %d", d.Metadata.Name, app, port)
					}
					seen[app] = true
				}
			}
		}
		for app := range want {
			egress := false
			for _, d := range docs {
				if d.Metadata.Namespace != "kanz-services" || !selectsApp(d, app) {
					continue
				}
				for _, rule := range d.Spec.Egress {
					if !rulePermitsPort(rule, port) {
						continue
					}
					for _, peer := range rule.To {
						if peer.NamespaceSelector == nil && peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == "datamaster" {
							egress = true
						}
					}
				}
			}
			if !egress {
				t.Errorf("missing explicit egress %s -> datamaster:%d", app, port)
			}
			if !seen[app] {
				t.Errorf("missing %s -> datamaster:%d", app, port)
			}
		}
	}
}
