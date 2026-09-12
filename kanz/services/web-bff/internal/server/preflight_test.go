package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/clientip"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

func artifactAt(at time.Time, pass bool) preflightArtifact {
	checks := make([]preflightArtifactCheck, 0, len(requiredPreflightChecks))
	for _, name := range requiredPreflightChecks {
		checks = append(checks, preflightArtifactCheck{Name: name, Pass: pass, Observed: json.RawMessage(`0`)})
	}
	verdict := "FAIL"
	if pass {
		verdict = "PASS"
	}
	return preflightArtifact{
		Format: preflightFormat, ObservedAt: at, VerifierCommit: "1111111111111111111111111111111111111111",
		DeployedCommit: "2222222222222222222222222222222222222222", CommandID: "command-1",
		Verdict: verdict, Checks: checks,
		WorkloadImages: map[string]string{"oms": "oms@sha256:x", "api_gateway": "gateway@sha256:x", "venue_binance": "binance@sha256:x", "venue_okx": "okx@sha256:x"},
	}
}

func writeArtifact(t *testing.T, a preflightArtifact) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPreflightEvidencePassFailUnknownAndStale(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		artifact preflightArtifact
		mutate   func(string)
		want     string
		reason   string
	}{
		{name: "pass", artifact: artifactAt(now.Add(-time.Minute), true), want: "PASS"},
		{name: "fail", artifact: artifactAt(now.Add(-time.Minute), false), want: "FAIL"},
		{name: "stale", artifact: artifactAt(now.Add(-16*time.Minute), true), want: "UNKNOWN", reason: "stale"},
		{name: "incomplete", artifact: func() preflightArtifact { a := artifactAt(now, true); a.Checks = a.Checks[:1]; return a }(), want: "UNKNOWN", reason: "incomplete"},
		{name: "unreadable", artifact: artifactAt(now, true), mutate: func(path string) { _ = os.WriteFile(path, []byte(`{"format":`), 0o600) }, want: "UNKNOWN", reason: "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeArtifact(t, tc.artifact)
			if tc.mutate != nil {
				tc.mutate(path)
			}
			got := newPreflightEvidence(path, 15*time.Minute).read(now)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q (%s)", got.Status, tc.want, got.Reason)
			}
			if tc.reason != "" && !contains(got.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to contain %q", got.Reason, tc.reason)
			}
		})
	}
}

func contains(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}

func TestPreflightEndpointRequiresSessionAndNeverProxies(t *testing.T) {
	path := writeArtifact(t, artifactAt(time.Now().UTC(), true))
	ip, _ := clientip.NewResolver("", nil)
	sessions := session.NewManager(time.Hour)
	srv, err := New(&Readiness{}, Options{
		Identity: identityclient.New("http://identity.invalid", "", time.Second), ClientIP: ip,
		Sessions: sessions, GatewayURL: "http://gateway.invalid", PreflightEvidencePath: path,
		PreflightEvidenceMaxAge: 15 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/preflight", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without session = %d, want 401", rec.Code)
	}

	id, err := sessions.Create(session.Session{Subject: "user:operator", Tenant: "__system__", AccessToken: "held-server-side", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/preflight", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with session = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got preflightStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "PASS" {
		t.Fatalf("status = %q, want PASS (%s)", got.Status, got.Reason)
	}
}
