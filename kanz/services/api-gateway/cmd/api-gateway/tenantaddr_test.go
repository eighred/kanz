package main

import "testing"

// THE PER-TENANT RISK ADDRESS IS DERIVED BY THE RULE THE MANIFEST IS RENDERED
// BY (#668). internal/tenantgen.WorkloadName names the Service; deriving the
// dial target any other way is a second spelling of one rule, and the failure
// is a dial against a Service rendered under a different name — which surfaces
// as an unavailable upstream and points at nothing.
func TestTenantRiskEngineAddr(t *testing.T) {
	for _, tc := range []struct {
		name, base, tenant, want string
		wantErr                  bool
	}{
		{
			name: "the in-cluster target", base: "risk-engine.kanz-services.svc:9090",
			tenant: "acme", want: "risk-engine-acme.kanz-services.svc:9090",
		},
		{
			// A gRPC target is host:port with NO SCHEME. Parsing it as a URL
			// would read the whole thing as a path and suffix nothing.
			name: "no scheme is assumed", base: "risk-engine:9090",
			tenant: "acme", want: "risk-engine-acme:9090",
		},
		{
			name: "a bare host keeps its shape", base: "risk-engine",
			tenant: "acme", want: "risk-engine-acme",
		},
		{
			// Only the FIRST label. A substring replace would rewrite the name
			// inside the namespace too and resolve to nothing.
			name: "only the first label", base: "risk-engine.risk-engine-ns.svc:9090",
			tenant: "acme", want: "risk-engine-acme.risk-engine-ns.svc:9090",
		},
		{
			name: "an empty base is refused", base: "", tenant: "acme", wantErr: true,
		},
		{
			name: "a portless colon has no host to suffix", base: ":9090", tenant: "acme", wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tenantRiskEngineAddr(tc.base, tc.tenant)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("tenantRiskEngineAddr(%q) = %q, want an error", tc.base, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("tenantRiskEngineAddr(%q): %v", tc.base, err)
			}
			if got != tc.want {
				t.Fatalf("tenantRiskEngineAddr(%q, %q) = %q, want %q", tc.base, tc.tenant, got, tc.want)
			}
		})
	}
}
