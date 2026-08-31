package compliance

import (
	"context"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// resident is the number of mandate versions the registry is holding for
// (tenant, portfolio). It reads byKey directly on purpose: the property under
// test is RESIDENCY, and every exported reader answers about the version it
// SELECTS — which is the one thing a leak leaves looking correct.
func resident(reg *MandateRegistry, tenant, portfolio string) int {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return len(reg.byKey[mandateKey{tenant: tenant, portfolio: portfolio}])
}

// atVersion resolves and returns the chosen version number, failing on anything
// that is not a clean hit.
func atVersion(t *testing.T, reg *MandateRegistry, asOf time.Time) uint64 {
	t.Helper()
	m, ok, err := reg.Mandate(context.Background(), "t1", "p1", asOf)
	if err != nil {
		t.Fatalf("resolution failed at %s: %v", asOf, err)
	}
	if !ok {
		t.Fatalf("no mandate in force at %s — the portfolio reads UNGOVERNED, which under "+
			"OMS_REQUIRE_MANDATE=false is admitted with no constraints", asOf)
	}
	return m.GetVersion()
}

// TestMandateVersionsDoNotAccumulate is the bound, MEASURED. #884: byKey's slice
// grew once per republish for the life of the process, and nothing dropped a
// version that could no longer be selected.
//
// A republish is an operator action, so this is a long-uptime concern rather than
// an attack surface — but the growth had no ceiling at all, and the estate-wide
// eviction guard cannot see it (it inspects map FIELDS; this is a collection
// inside a bounded key set).
func TestMandateVersionsDoNotAccumulate(t *testing.T) {
	reg := NewMandateRegistry()
	// Every version is already in force when it lands — the shape of an operator
	// correcting a mandate, which is what actually republishes.
	base := t0
	reg.now = func() time.Time { return base.Add(365 * 24 * time.Hour) }

	const republishes = 500
	for v := uint64(1); v <= republishes; v++ {
		mustPut(t, reg, versioned(v, base.Add(time.Duration(v)*time.Minute)))
	}

	if got := resident(reg, "t1", "p1"); got != 1 {
		t.Fatalf("after %d republishes the registry holds %d versions, want 1: only the latest one "+
			"in force at the retention instant can ever be selected again", republishes, got)
	}
	if got := atVersion(t, reg, reg.now()); got != republishes {
		t.Fatalf("pruning changed the answer: resolved v%d, want v%d", got, republishes)
	}
	// A zero asOf means "the latest", and it is a separate branch in Mandate.
	if got := atVersion(t, reg, time.Time{}); got != republishes {
		t.Fatalf("zero-asOf resolution changed: resolved v%d, want v%d", got, republishes)
	}
}

// TestMandateRegistry_ScheduledVersionsAreNeverPruned is THE SAFETY TEST, and it
// matters more than the memory one.
//
// A mandate published today with a date next month governs nothing until then —
// it is held, unselected, waiting. Dropping one because it is not the newest, or
// because it is not currently in force, would silently cancel a mandate change an
// operator scheduled and two people signed for. Nothing downstream would say so:
// the portfolio would keep resolving to the OLD mandate past the date the new one
// was meant to take effect.
func TestMandateRegistry_ScheduledVersionsAreNeverPruned(t *testing.T) {
	reg := NewMandateRegistry()
	retention := t0.Add(24 * time.Hour)
	reg.now = func() time.Time { return retention }

	// v1 superseded, v2 in force at the retention instant, v3..v5 scheduled.
	eff := map[uint64]time.Time{
		1: t0.Add(-48 * time.Hour),
		2: t0.Add(-1 * time.Hour),
		3: retention.Add(7 * 24 * time.Hour),
		4: retention.Add(30 * 24 * time.Hour),
		5: retention.Add(90 * 24 * time.Hour),
	}
	for v := uint64(1); v <= 5; v++ {
		mustPut(t, reg, versioned(v, eff[v]))
	}

	// One in force plus three scheduled. v1 is the only unreachable one.
	if got := resident(reg, "t1", "p1"); got != 4 {
		t.Fatalf("resident versions = %d, want 4 (v2 in force + v3,v4,v5 scheduled)", got)
	}
	for _, tc := range []struct {
		when time.Time
		want uint64
	}{
		{retention, 2},
		{eff[3], 3},
		{eff[4], 4},
		{eff[5], 5},
		{eff[5].Add(365 * 24 * time.Hour), 5},
	} {
		if got := atVersion(t, reg, tc.when); got != tc.want {
			t.Fatalf("at %s resolved v%d, want v%d — a SCHEDULED mandate change was lost",
				tc.when, got, tc.want)
		}
	}
}

// TestMandateRegistry_AllFutureDatedSurvive covers the boundary the loop above
// cannot reach: a portfolio whose EVERY published version is still in the future.
// Pruning to "the latest" there would leave nothing in force at all, and an
// UNGOVERNED portfolio is not refused under the default posture — it is admitted
// with no constraints.
func TestMandateRegistry_AllFutureDatedSurvive(t *testing.T) {
	reg := NewMandateRegistry()
	reg.now = func() time.Time { return t0 }
	mustPut(t, reg, versioned(1, t0.Add(time.Hour)))
	mustPut(t, reg, versioned(2, t0.Add(2*time.Hour)))

	if got := resident(reg, "t1", "p1"); got != 2 {
		t.Fatalf("resident versions = %d, want 2 — neither is in force yet, so neither is superseded", got)
	}
	if got := atVersion(t, reg, t0.Add(time.Hour)); got != 1 {
		t.Fatalf("the first scheduled version did not take force: resolved v%d, want v1", got)
	}
}

// TestMandateRegistry_ScheduledVersionCollapsesOnceItIsSuperseded proves the
// retained set DRAINS. A scheduled version is kept while it is still ahead of the
// clock and dropped once a later one has overtaken it, so "1 + scheduled" is a
// ceiling the estate walks back down rather than a high-water mark.
func TestMandateRegistry_ScheduledVersionCollapsesOnceItIsSuperseded(t *testing.T) {
	reg := NewMandateRegistry()
	clock := t0
	reg.now = func() time.Time { return clock }

	mustPut(t, reg, versioned(1, t0.Add(-time.Hour)))
	mustPut(t, reg, versioned(2, t0.Add(24*time.Hour)))
	if got := resident(reg, "t1", "p1"); got != 2 {
		t.Fatalf("resident versions = %d, want 2 while v2 is still scheduled", got)
	}

	// The clock passes v2's date and the operator republishes something else.
	clock = t0.Add(48 * time.Hour)
	mustPut(t, reg, versioned(3, clock.Add(-time.Minute)))
	if got := resident(reg, "t1", "p1"); got != 1 {
		t.Fatalf("resident versions = %d, want 1 — v1 and v2 are both superseded by v3", got)
	}
	if got := atVersion(t, reg, clock); got != 3 {
		t.Fatalf("resolved v%d, want v3", got)
	}
}

// TestMandateRegistry_RepublishedVersionKeepsTheSliceOrdered covers the idempotent
// -replace path, which used to write the new mandate over the old one IN PLACE and
// return without re-sorting.
//
// A republished version carries its own effective_at and an operator correcting a
// mandate can move it — effective_at is inside the digest the two-signature flow
// covers precisely because it is allowed to change. An in-place write left the
// slice out of the order Mandate's "the last match wins" scan depends on, so the
// registry could resolve to a version that was not the latest in force.
func TestMandateRegistry_RepublishedVersionKeepsTheSliceOrdered(t *testing.T) {
	reg := NewMandateRegistry()
	reg.now = func() time.Time { return t0 }
	mustPut(t, reg, versioned(1, t0.Add(24*time.Hour)))
	mustPut(t, reg, versioned(2, t0.Add(48*time.Hour)))

	// v1 is corrected to take force AFTER v2 — legal, and the reason the order has
	// to be re-established rather than assumed.
	mustPut(t, reg, versioned(1, t0.Add(72*time.Hour)))

	if got := atVersion(t, reg, t0.Add(96*time.Hour)); got != 1 {
		t.Fatalf("resolved v%d, want v1: it was republished with the LATER effective_at, so it is "+
			"the one in force", got)
	}
	if got := atVersion(t, reg, t0.Add(60*time.Hour)); got != 2 {
		t.Fatalf("resolved v%d, want v2 at a time only v2 had reached", got)
	}
}

// TestMandateRegistry_RetentionIsPerKey proves the prune does not reach across
// (tenant, portfolio). Two tenants naming a portfolio "p1" is the #243 case, and a
// retention step that walked the wrong bucket would drop one tenant's mandate on
// the other's republish — leaving it UNGOVERNED, which is worse than the leak.
func TestMandateRegistry_RetentionIsPerKey(t *testing.T) {
	reg := NewMandateRegistry()
	reg.now = func() time.Time { return t0.Add(24 * time.Hour) }
	other := func(v uint64, eff time.Time) *compliancepb.Mandate {
		return &compliancepb.Mandate{
			MandateId: "m2", TenantId: "t2", PortfolioId: "p1", Version: v,
			EffectiveAt: timestamppb.New(eff),
		}
	}
	mustPut(t, reg, other(1, t0))
	mustPut(t, reg, other(2, t0.Add(90*24*time.Hour))) // scheduled, must survive
	for v := uint64(1); v <= 20; v++ {
		mustPut(t, reg, versioned(v, t0.Add(time.Duration(v)*time.Minute)))
	}

	if got := resident(reg, "t1", "p1"); got != 1 {
		t.Fatalf("tenant t1 holds %d versions, want 1", got)
	}
	if got := resident(reg, "t2", "p1"); got != 2 {
		t.Fatalf("tenant t2 holds %d versions, want 2 (v1 in force + v2 scheduled) — t1's republishes "+
			"reached across the key", got)
	}
	m, ok, err := reg.Mandate(context.Background(), "t2", "p1", t0.Add(24*time.Hour))
	if err != nil || !ok || m.GetMandateId() != "m2" {
		t.Fatalf("t2's mandate did not survive t1's republishes: ok=%v m=%v err=%v", ok, m, err)
	}
}
