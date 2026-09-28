package authz

import (
	"encoding/json"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"net/http"
	"slices"
)

// Permissions reports the actual grants and registered routes. Portfolio lists
// remain scope constraints, and domain admission still decides individual acts.
func (m *Mux) Permissions(w http.ResponseWriter, r *http.Request) {
	p := middleware.PrincipalFromContext(r.Context())
	if p == nil {
		forbidden(w)
		return
	}
	caps := make([]Capability, 0)
	routes := make([]Route, 0)
	for _, route := range m.routes {
		if m.grants.Allows(p.Roles, route.Capability) {
			routes = append(routes, route)
			if !slices.Contains(caps, route.Capability) {
				caps = append(caps, route.Capability)
			}
		}
	}
	slices.Sort(caps)
	slices.SortFunc(routes, func(a, b Route) int {
		if a.Pattern < b.Pattern {
			return -1
		}
		if a.Pattern > b.Pattern {
			return 1
		}
		return 0
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"subject": p.Subject, "tenant": p.Tenant, "capabilities": caps, "portfolios": append([]string{}, p.Portfolios...), "routes": routes})
}
