package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Declaring a v2 route on authz.Mux does not expose it when the outer server
// mounts only /v1/. Exercise the actual composition and require its shared
// authentication, body bound and version negotiation before any upstream call.
func TestVersionedRoutesUseTheGatewayMiddleware(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		for _, tc := range []struct {
			name, body, authorization, apiVersion string
			status                                int
			forwarded                             bool
		}{
			{name: "authenticated", body: `{}`, authorization: "Bearer fixture", status: http.StatusAccepted, forwarded: true},
			{name: "anonymous", body: `{}`, status: http.StatusUnauthorized},
			{name: "oversized", body: strings.Repeat(" ", (1<<20)+1), authorization: "Bearer fixture", status: http.StatusRequestEntityTooLarge},
			{name: "unsupported_version", body: `{}`, authorization: "Bearer fixture", apiVersion: "unrecognized", status: http.StatusNotAcceptable},
		} {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				router, backend := routerFor(t, "", baselineRole)
				req := httptest.NewRequest(http.MethodPost, "/"+version+"/model-portfolios/propose", strings.NewReader(tc.body))
				if tc.authorization != "" {
					req.Header.Set("Authorization", tc.authorization)
				}
				if tc.apiVersion != "" {
					req.Header.Set("X-API-Version", tc.apiVersion)
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, req)
				if recorder.Code != tc.status || backend.called != tc.forwarded {
					t.Fatalf("actual %s composition: status%d forwarded%v, want%d/%v", version, recorder.Code, backend.called, tc.status, tc.forwarded)
				}
			})
		}
	}
}
