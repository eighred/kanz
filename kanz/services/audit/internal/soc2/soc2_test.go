package soc2

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/services/audit/internal/audit"
)

func rec(id string, kind audit.Kind, at time.Time) *audit.Record {
	return &audit.Record{EventID: id, Kind: kind, OccurredAt: at}
}

func TestCollect_CountsSamplesAndGaps(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)
	mid := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	records := []*audit.Record{
		rec("a1", audit.KindAuthzDecision, mid),
		rec("a2", audit.KindAuthzDecision, mid),
		rec("d1", audit.KindDataQuality, mid),
		rec("c1", audit.KindCommandOutcome, mid), // evidences CC8.1 AND PI1.1
		// No KindDecision-only record; PI1.1 is still satisfied by the command outcome.
		rec("out", audit.KindAuthzDecision, to.AddDate(0, 1, 0)), // outside the window — ignored
	}

	report := Collect(DefaultControls(), records, from, to)

	byID := map[string]ControlEvidence{}
	for _, ce := range report.Controls {
		byID[ce.Control.ID] = ce
	}
	if got := byID["CC6.1"].Count; got != 2 {
		t.Fatalf("CC6.1 count = %d want 2 (the out-of-window record excluded)", got)
	}
	if !byID["CC6.1"].Satisfied || len(byID["CC6.1"].Samples) != 2 {
		t.Fatalf("CC6.1 should be satisfied with 2 samples: %+v", byID["CC6.1"])
	}
	if byID["CC8.1"].Count != 1 || byID["PI1.1"].Count != 1 {
		t.Fatalf("command outcome must evidence BOTH CC8.1 and PI1.1: cc8=%d pi1=%d",
			byID["CC8.1"].Count, byID["PI1.1"].Count)
	}
	if report.TotalCount != 4 {
		t.Fatalf("total in-window supporting records = %d want 4", report.TotalCount)
	}
	if !report.Satisfied || len(report.Gaps) != 0 {
		t.Fatalf("every control evidenced ⇒ satisfied, no gaps: %+v", report.Gaps)
	}
}

func TestCollect_FlagsUnevidencedControlAsGap(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	// Only an authz decision — CC7.2 (monitoring), CC8.1 (change), PI1.1 have no
	// evidence in the window, so they are gaps.
	report := Collect(DefaultControls(), []*audit.Record{rec("a1", audit.KindAuthzDecision, from)}, from, to)
	if report.Satisfied {
		t.Fatal("a window missing evidence for a mapped control must not be satisfied")
	}
	if len(report.Gaps) != 3 {
		t.Fatalf("expected 3 unevidenced controls, got gaps=%v", report.Gaps)
	}
}

// The continuous-evidence pipeline pulls the window from the store and collects.
func TestCollectFromStore_PullsWindowAndCollects(t *testing.T) {
	store := audit.NewMemory()
	ctx := context.Background()
	now := time.Now()
	for _, r := range []*audit.Record{
		{EventID: "a1", Kind: audit.KindAuthzDecision, EventType: "authz", OccurredAt: now},
		{EventID: "q1", Kind: audit.KindDataQuality, EventType: "dq", OccurredAt: now},
		{EventID: "cmd1", Kind: audit.KindCommandOutcome, EventType: "cmd", OccurredAt: now},
	} {
		r.TenantID = "test"
		if _, err := store.Append(ctx, r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	report, err := CollectFromStore(ctx, store, "test", now.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalCount != 3 {
		t.Fatalf("collected %d supporting records, want 3", report.TotalCount)
	}
}

type boundedEvidenceStore struct {
	audit.Store
	filter audit.Filter
}

func (s *boundedEvidenceStore) Query(_ context.Context, f audit.Filter) ([]*audit.Record, error) {
	s.filter = f
	return make([]*audit.Record, MaxEvidenceRecords+1), nil
}
func TestEvidenceWindowCannotBecomeAnUnboundedOrTruncatedPass(t *testing.T) {
	s := &boundedEvidenceStore{}
	_, err := CollectFromStore(context.Background(), s, "acme", time.Unix(1, 0), time.Unix(2, 0))
	if !errors.Is(err, ErrTooManyRecords) || s.filter.Limit != MaxEvidenceRecords+1 || s.filter.Tenant != "acme" {
		t.Fatal(err, s.filter)
	}
}
