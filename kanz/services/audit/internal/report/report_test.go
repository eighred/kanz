package report

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/audit/internal/audit"
)

func seedStore(t *testing.T) *audit.Memory {
	t.Helper()
	st := audit.NewMemory()
	ctx := context.Background()
	recs := []*audit.Record{
		{EventID: "a1", CorrelationID: "C1", Kind: audit.KindAuthzDecision, Summary: "DENY", TenantID: "t1", RecordedAt: time.Unix(1000, 0)},
		{EventID: "o1", CorrelationID: "C1", Kind: audit.KindCommandOutcome, Summary: "filled", TenantID: "t1", RecordedAt: time.Unix(1000, 0)},
		{EventID: "a2", CorrelationID: "C2", Kind: audit.KindAuthzDecision, Summary: "ALLOW", TenantID: "t2", RecordedAt: time.Unix(2000, 0)},
	}
	for _, r := range recs {
		if _, err := st.Append(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestGenerateWithAttestation(t *testing.T) {
	st := seedStore(t)
	tmpl := BuiltIns()["authz-decisions"]
	rep, err := Generate(context.Background(), st, tmpl, func() time.Time { return time.Unix(3000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Records) != 2 {
		t.Fatalf("authz records=%d want 2", len(rep.Records))
	}
	if !rep.Integrity.Verified || rep.Integrity.Records != 3 {
		t.Fatalf("attestation=%+v want verified over 3 records", rep.Integrity)
	}
}

func TestGenerateAttestationFailsOnTamper(t *testing.T) {
	st := seedStore(t)
	// Tamper: mutate a stored record's summary so its hash no longer matches.
	all, _ := st.All(context.Background())
	all[0].Summary = "FORGED"

	rep, err := Generate(context.Background(), st, BuiltIns()["full-log"], time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Integrity.Verified {
		t.Fatal("attestation passed over a tampered log")
	}
	if !strings.Contains(rep.Integrity.Detail, "index 0") {
		t.Fatalf("detail %q should locate the tamper", rep.Integrity.Detail)
	}
}

func TestRenderCSVCarriesIntegrity(t *testing.T) {
	st := seedStore(t)
	rep, _ := Generate(context.Background(), st, BuiltIns()["full-log"], time.Now)
	b, err := rep.RenderCSV()
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, "integrity_verified: true") {
		t.Fatalf("CSV missing integrity header:\n%s", out)
	}
	if !strings.Contains(out, "event_id") {
		t.Fatal("CSV missing column header")
	}
}

func TestEvaluatePurgeRetentionAndHold(t *testing.T) {
	now := time.Unix(10000, 0)
	recs := []*audit.Record{
		{EventID: "old-free", CorrelationID: "C1", TenantID: "t1", RecordedAt: now.Add(-2 * time.Hour)},
		{EventID: "old-held", CorrelationID: "HOLD", TenantID: "t1", RecordedAt: now.Add(-2 * time.Hour)},
		{EventID: "young", CorrelationID: "C2", TenantID: "t1", RecordedAt: now.Add(-1 * time.Minute)},
	}
	d := EvaluatePurge(recs, RetentionPolicy{Retain: time.Hour}, []LegalHold{{Correlations: []string{"HOLD"}}}, now)
	if len(d.Eligible) != 1 || d.Eligible[0].EventID != "old-free" {
		t.Fatalf("eligible=%v want [old-free]", ids(d.Eligible))
	}
	if len(d.Held) != 1 || d.Held[0].EventID != "old-held" {
		t.Fatalf("held=%v want [old-held]", ids(d.Held))
	}
	if d.Retained != 1 {
		t.Fatalf("retained=%d want 1 (young)", d.Retained)
	}
}

func TestZeroRetentionKeepsEverything(t *testing.T) {
	now := time.Unix(10000, 0)
	recs := []*audit.Record{{EventID: "ancient", RecordedAt: time.Unix(1, 0)}}
	d := EvaluatePurge(recs, RetentionPolicy{}, nil, now)
	if d.Retained != 1 || len(d.Eligible) != 0 {
		t.Fatalf("zero retention purged: %+v", d)
	}
}

func ids(rs []*audit.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.EventID
	}
	return out
}
