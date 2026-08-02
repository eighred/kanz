package bus

import (
	"strings"
	"testing"
)

func TestRequireTenantScope(t *testing.T) {
	cases := []struct {
		name       string
		envTenant  string
		serving    string
		wantErr    bool
		wantPhrase string
	}{
		{
			name:      "dedicated service, own tenant",
			envTenant: "acme", serving: "acme",
		},
		{
			name:      "dedicated service, ANOTHER tenant — the fold this exists to stop",
			envTenant: "beta", serving: "acme",
			wantErr: true, wantPhrase: "cross-tenant event",
		},
		{
			name:      "dedicated service, untenanted envelope — cannot be proven to belong here",
			envTenant: "", serving: "acme",
			wantErr: true, wantPhrase: "untenanted event",
		},
		{
			// The branch that keeps today's estate running. The gateway stamps the
			// CALLER's tenant on the command while the shipped OMS serves
			// __system__, so every genuine order is already a mismatch — refusing
			// them would reject all of them. Fixed by per-tenant compute (#97), not
			// by this check.
			name:      "shared __system__ bucket accepts a real tenant's event",
			envTenant: "acme", serving: SystemTenant,
		},
		{
			name:      "shared bucket accepts its own",
			envTenant: SystemTenant, serving: SystemTenant,
		},
		{
			name:      "a service with no configured tenant is refused outright",
			envTenant: "acme", serving: "",
			wantErr: true, wantPhrase: "no configured tenant",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireTenantScope(tc.envTenant, tc.serving)
			if tc.wantErr && err == nil {
				t.Fatalf("RequireTenantScope(%q, %q) = nil, want an error", tc.envTenant, tc.serving)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("RequireTenantScope(%q, %q) = %v, want nil", tc.envTenant, tc.serving, err)
			}
			if tc.wantPhrase != "" && !strings.Contains(err.Error(), tc.wantPhrase) {
				t.Errorf("error = %q, want it to contain %q so the operator learns which "+
					"of the refusals fired", err.Error(), tc.wantPhrase)
			}
		})
	}
}

// The refusal must name BOTH tenants. An operator reading a DLQ entry needs to
// know which tenant sent it and which service refused it; "cross-tenant event"
// alone does not tell them where to look.
func TestCrossTenantRefusalNamesBothTenants(t *testing.T) {
	err := RequireTenantScope("beta", "acme")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"beta", "acme"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}
