package revocation

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// validRequest is the request every ordering test uses. It is complete on
// purpose: these tests are about the SEQUENCE, and a request that failed
// validation would abort before any stage ran and make every one of them pass
// vacuously.
func validRequest() Request {
	return Request{Tenant: "acme", By: "operator:akif", Reason: "licence terminated"}
}

// recordingEstate captures the ORDER in which the three verbs were called, and
// can be told to fail at exactly one of them. It records the call BEFORE
// deciding whether to fail, so a failing stage still appears in the trace — the
// tests need to distinguish "was never attempted" from "was attempted and
// refused", and a double that only recorded successes could not.
type recordingEstate struct {
	calls   []Stage
	tenants []string
	failAt  Stage
	failErr error
}

func (r *recordingEstate) record(stage Stage, req Request) error {
	r.calls = append(r.calls, stage)
	r.tenants = append(r.tenants, req.Tenant)
	if r.failAt == stage {
		return r.failErr
	}
	return nil
}

func (r *recordingEstate) HaltTrading(_ context.Context, req Request) error {
	return r.record(StageHalt, req)
}

func (r *recordingEstate) ScaleToZero(_ context.Context, req Request) error {
	return r.record(StageScaleToZero, req)
}

func (r *recordingEstate) PurgeSecrets(_ context.Context, req Request) error {
	return r.record(StagePurgeSecrets, req)
}

func (r *recordingEstate) trace() string {
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.String())
	}
	if len(out) == 0 {
		return "(nothing was called)"
	}
	return strings.Join(out, " -> ")
}

// indexOf reports where a stage appears in the trace, or -1 if it never ran.
func (r *recordingEstate) indexOf(stage Stage) int {
	return slices.Index(r.calls, stage)
}

var _ Estate = (*recordingEstate)(nil)

// TestHaltStrictlyPrecedesScaleAndPurge is THE invariant this package exists
// for. Scaling a tenant's OMS to zero while orders are live at a venue abandons
// them: the exchange keeps the working orders and the process that knew about
// them is gone. The halt closes the gate first so that set stops growing.
func TestHaltStrictlyPrecedesScaleAndPurge(t *testing.T) {
	est := &recordingEstate{}

	res, err := Revoke(context.Background(), est, validRequest())
	if err != nil {
		t.Fatalf("Revoke over an estate that refuses nothing: %v", err)
	}

	halt, scale, purge := est.indexOf(StageHalt), est.indexOf(StageScaleToZero), est.indexOf(StagePurgeSecrets)
	if halt < 0 {
		t.Fatalf("the halt never ran: %s", est.trace())
	}
	// Asserted as three separate ordered pairs rather than one equality, so a
	// reordering says WHICH pair inverted. A revocation that scales before it
	// halts is the failure mode that costs money.
	if scale < 0 || halt >= scale {
		t.Errorf("halt does NOT strictly precede scale-to-zero (halt at %d, scale at %d): %s\n"+
			"Scaling a tenant to zero before trading is halted abandons its live orders at the "+
			"venue with no process left to reconcile them.", halt, scale, est.trace())
	}
	if purge < 0 || halt >= purge {
		t.Errorf("halt does NOT strictly precede purge-secrets (halt at %d, purge at %d): %s",
			halt, purge, est.trace())
	}
	if purge < scale {
		t.Errorf("purge-secrets ran before scale-to-zero (scale at %d, purge at %d): %s\n"+
			"Purging first crash-loops still-running pods against missing mounts.",
			scale, purge, est.trace())
	}

	if want := []Stage{StageHalt, StageScaleToZero, StagePurgeSecrets}; !slices.Equal(est.calls, want) {
		t.Errorf("call trace = %s, want halt -> scale-to-zero -> purge-secrets", est.trace())
	}
	if !res.Revoked || !res.BrakeEngaged || res.FailedAt != StageNone {
		t.Errorf("Result = %+v, want Revoked=true BrakeEngaged=true FailedAt=none", res)
	}
	if !slices.Equal(res.Done, []Stage{StageHalt, StageScaleToZero, StagePurgeSecrets}) {
		t.Errorf("Result.Done = %v, want all three stages in order", res.Done)
	}
	for i, got := range est.tenants {
		if got != "acme" {
			t.Errorf("stage %d acted on tenant %q, want %q", i, got, "acme")
		}
	}
}

// TestFailedHaltAbortsBeforeAnythingIsScaledOrPurged is the abort rule: a
// tenant that could not be stopped must not be dismantled around its own live
// orders.
func TestFailedHaltAbortsBeforeAnythingIsScaledOrPurged(t *testing.T) {
	boom := errors.New("bus unreachable")
	est := &recordingEstate{failAt: StageHalt, failErr: boom}

	res, err := Revoke(context.Background(), est, validRequest())
	if err == nil {
		t.Fatal("Revoke returned nil after the halt failed — a failed halt must never report a revocation")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not wrap the halt failure %v", err, boom)
	}
	if est.indexOf(StageScaleToZero) >= 0 {
		t.Errorf("scale-to-zero ran after a FAILED halt: %s\n"+
			"That is the sequence that abandons working orders at the venue.", est.trace())
	}
	if est.indexOf(StagePurgeSecrets) >= 0 {
		t.Errorf("purge-secrets ran after a FAILED halt: %s", est.trace())
	}
	if want := []Stage{StageHalt}; !slices.Equal(est.calls, want) {
		t.Errorf("call trace = %s, want the halt attempt and nothing else", est.trace())
	}
	if res.Revoked {
		t.Error("Result.Revoked is true after a failed halt")
	}
	if res.BrakeEngaged {
		t.Error("Result.BrakeEngaged is true although the halt FAILED — the tenant may still be trading")
	}
	if res.FailedAt != StageHalt {
		t.Errorf("Result.FailedAt = %s, want halt", res.FailedAt)
	}
	if len(res.Done) != 0 {
		t.Errorf("Result.Done = %v, want empty — nothing completed", res.Done)
	}
}

// TestFailedScaleLeavesTheBrakeEngagedAndPurgesNothing is the no-rollback rule
// at stage 2. The tenant is left halted, not resumed.
func TestFailedScaleLeavesTheBrakeEngagedAndPurgesNothing(t *testing.T) {
	boom := errors.New("apiserver rejected the scale")
	est := &recordingEstate{failAt: StageScaleToZero, failErr: boom}

	res, err := Revoke(context.Background(), est, validRequest())
	if err == nil {
		t.Fatal("Revoke returned nil although scale-to-zero failed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not wrap the scale failure %v", err, boom)
	}
	if !res.BrakeEngaged {
		t.Error("Result.BrakeEngaged is false after a successful halt — a partial revocation must " +
			"leave the brake ON, never back out into a half-revoked tenant that still trades")
	}
	if res.Revoked {
		t.Error("Result.Revoked is true although scale-to-zero failed")
	}
	if res.FailedAt != StageScaleToZero {
		t.Errorf("Result.FailedAt = %s, want scale-to-zero", res.FailedAt)
	}
	if est.indexOf(StagePurgeSecrets) >= 0 {
		t.Errorf("purge-secrets ran after a failed scale: %s — the purge is the one stage "+
			"re-running cannot undo", est.trace())
	}
	if want := []Stage{StageHalt, StageScaleToZero}; !slices.Equal(est.calls, want) {
		t.Errorf("call trace = %s, want halt -> scale-to-zero and no further stage", est.trace())
	}
	if !slices.Equal(res.Done, []Stage{StageHalt}) {
		t.Errorf("Result.Done = %v, want [halt]", res.Done)
	}
}

// TestFailedPurgeLeavesTheBrakeEngagedAndTheTenantScaledToZero is the same rule
// at stage 3: the credentials may still be mounted, and the answer is still not
// to resume the tenant.
func TestFailedPurgeLeavesTheBrakeEngagedAndTheTenantScaledToZero(t *testing.T) {
	boom := errors.New("vault refused the delete")
	est := &recordingEstate{failAt: StagePurgeSecrets, failErr: boom}

	res, err := Revoke(context.Background(), est, validRequest())
	if err == nil {
		t.Fatal("Revoke returned nil although purge-secrets failed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not wrap the purge failure %v", err, boom)
	}
	if !res.BrakeEngaged {
		t.Error("Result.BrakeEngaged is false — a failed purge must still leave the tenant halted")
	}
	if res.Revoked {
		t.Error("Result.Revoked is true although the secrets were not purged")
	}
	if !slices.Equal(res.Done, []Stage{StageHalt, StageScaleToZero}) {
		t.Errorf("Result.Done = %v, want [halt scale-to-zero]", res.Done)
	}
	if res.FailedAt != StagePurgeSecrets {
		t.Errorf("Result.FailedAt = %s, want purge-secrets", res.FailedAt)
	}
}

// TestRevokeNeverCallsAnythingTwice guards against a retry loop being added
// around a stage without an argument for it. Re-running a purge is not
// idempotent in the direction that matters.
func TestRevokeNeverCallsAnythingTwice(t *testing.T) {
	est := &recordingEstate{}
	if _, err := Revoke(context.Background(), est, validRequest()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	seen := map[Stage]int{}
	for _, c := range est.calls {
		seen[c]++
	}
	for stage, n := range seen {
		if n != 1 {
			t.Errorf("stage %s ran %d times, want exactly once: %s", stage, n, est.trace())
		}
	}
}

// TestEstateExposesNoUndoVerb is the structural half of the no-rollback rule.
// The safe end state of a failed revocation is HALTED; an interface that could
// resume a tenant is an invitation to "restore" one whose secrets are already
// gone and whose fitness to trade nobody has re-decided.
func TestEstateExposesNoUndoVerb(t *testing.T) {
	typ := reflect.TypeOf((*Estate)(nil)).Elem()

	if got := typ.NumMethod(); got != 3 {
		t.Errorf("Estate has %d methods, want exactly 3 (halt, scale, purge) — a fourth verb "+
			"needs an argument in the package doc, not just a compiler", got)
	}

	// Vocabulary, not an exact list: the point is to catch the SHAPE of an undo
	// being added, whatever it gets called.
	forbidden := []string{"Resume", "Rollback", "Undo", "Restore", "Unhalt", "Revert",
		"Compensate", "Reinstate", "Restart", "ScaleUp", "Recreate"}
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("Estate exposes %q, which is an undo verb (%q). A partial revocation "+
					"must be left HALTED — resuming would let a half-dismantled tenant trade "+
					"again under a record saying it was stopped.", name, bad)
			}
		}
	}

	want := []string{"HaltTrading", "PurgeSecrets", "ScaleToZero"}
	var got []string
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("Estate methods = %v, want %v", got, want)
	}
}

func TestRevokeRejectsAnUnderSpecifiedRequestBeforeTouchingTheEstate(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"no tenant", Request{By: "operator:akif", Reason: "licence terminated"}},
		{"blank tenant", Request{Tenant: "   ", By: "operator:akif", Reason: "licence terminated"}},
		{"no principal", Request{Tenant: "acme", Reason: "licence terminated"}},
		{"principal is not type:id", Request{Tenant: "acme", By: "akif", Reason: "licence terminated"}},
		{"no reason", Request{Tenant: "acme", By: "operator:akif"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			est := &recordingEstate{}
			res, err := Revoke(context.Background(), est, tc.req)
			if err == nil {
				t.Fatalf("Revoke(%+v) returned nil, want a refusal", tc.req)
			}
			if len(est.calls) != 0 {
				t.Errorf("an invalid request reached the estate: %s — validation must abort "+
					"BEFORE a tenant is halted for a reason nobody can attribute", est.trace())
			}
			if res.BrakeEngaged || res.Revoked {
				t.Errorf("Result = %+v, want the zero value", res)
			}
		})
	}
}

func TestRevokeRejectsANilEstate(t *testing.T) {
	res, err := Revoke(context.Background(), nil, validRequest())
	if err == nil {
		t.Fatal("Revoke with a nil estate returned nil — it would report a revocation that touched nothing")
	}
	if res.Revoked {
		t.Error("Result.Revoked is true with no estate at all")
	}
}

// TestCancelledContextStopsAtAStageBoundary — a deadline expiring mid-revocation
// must stop between stages, with the brake left on, rather than part-way through
// a cluster call.
func TestCancelledContextStopsAtAStageBoundary(t *testing.T) {
	t.Run("cancelled before the halt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		est := &recordingEstate{}
		res, err := Revoke(ctx, est, validRequest())
		if err == nil {
			t.Fatal("Revoke on a cancelled context returned nil")
		}
		if len(est.calls) != 0 {
			t.Errorf("stages ran on a cancelled context: %s", est.trace())
		}
		if res.BrakeEngaged {
			t.Error("Result.BrakeEngaged is true although the halt never ran")
		}
	})

	t.Run("cancelled after the halt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		est := &cancelAfterHalt{cancel: cancel}
		res, err := Revoke(ctx, est, validRequest())
		if err == nil {
			t.Fatal("Revoke returned nil although the context was cancelled after the halt")
		}
		if est.scaled {
			t.Error("scale-to-zero ran after the context was cancelled")
		}
		if est.purged {
			t.Error("purge-secrets ran after the context was cancelled")
		}
		if !res.BrakeEngaged {
			t.Error("Result.BrakeEngaged is false — a cancelled revocation must still leave the " +
				"brake on; nothing here takes it back off")
		}
		if res.FailedAt != StageScaleToZero {
			t.Errorf("Result.FailedAt = %s, want scale-to-zero", res.FailedAt)
		}
	})
}

// cancelAfterHalt cancels the revocation's own context from inside the halt —
// the shape of a deadline expiring while the brake is being applied.
type cancelAfterHalt struct {
	cancel context.CancelFunc
	scaled bool
	purged bool
}

func (c *cancelAfterHalt) HaltTrading(context.Context, Request) error { c.cancel(); return nil }
func (c *cancelAfterHalt) ScaleToZero(context.Context, Request) error { c.scaled = true; return nil }
func (c *cancelAfterHalt) PurgeSecrets(context.Context, Request) error {
	c.purged = true
	return nil
}

func TestResultStatesTheEndState(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  Result
		want []string
	}{
		{
			"complete",
			Result{Done: []Stage{StageHalt, StageScaleToZero, StagePurgeSecrets}, BrakeEngaged: true, Revoked: true},
			[]string{"REVOKED", "brake ENGAGED"},
		},
		{
			"partial",
			Result{Done: []Stage{StageHalt}, FailedAt: StageScaleToZero, BrakeEngaged: true},
			[]string{"PARTIALLY REVOKED", "brake ENGAGED", "still deployed", "Do NOT resume"},
		},
		{
			"halt failed",
			Result{FailedAt: StageHalt},
			[]string{"NOT REVOKED", "brake NOT engaged", "may still"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.res.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("Result.String() = %q, missing %q — the summary is what the next "+
						"operator acts on", got, want)
				}
			}
		})
	}
}

func TestStageStringsAreDistinct(t *testing.T) {
	seen := map[string]Stage{}
	for _, s := range []Stage{StageNone, StageHalt, StageScaleToZero, StagePurgeSecrets} {
		name := s.String()
		if prev, dup := seen[name]; dup {
			t.Errorf("Stage(%d) and Stage(%d) both render as %q", int(prev), int(s), name)
		}
		seen[name] = s
	}
	if got := Stage(99).String(); !strings.Contains(got, "99") {
		t.Errorf("Stage(99).String() = %q, want it to name the unknown value", got)
	}
}

// TestNotImplementedStubsRefuseLoudly. A stub that returned nil would take
// Revoke all the way to Revoked=true over a tenant that is still trading, still
// deployed and still holding its exchange credentials.
func TestNotImplementedStubsRefuseLoudly(t *testing.T) {
	req := validRequest()
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"halt", func() error { return NotImplementedHalter{}.HaltTrading(context.Background(), req) }},
		{"scale", func() error { return NotImplementedScaler{}.ScaleToZero(context.Background(), req) }},
		{"purge", func() error { return NotImplementedPurger{}.PurgeSecrets(context.Background(), req) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s stub returned nil — 'nothing configured' and 'checked, and fine' "+
					"must never look the same", tc.name)
			}
			if !errors.Is(err, ErrNotImplemented) {
				t.Errorf("%s stub error %v does not wrap ErrNotImplemented", tc.name, err)
			}
			if !strings.Contains(err.Error(), req.Tenant) {
				t.Errorf("%s stub error %q does not name the tenant it refused", tc.name, err)
			}
			// A refusal an operator cannot act on is only marginally better than
			// a silent success: it must say what is missing, not just that
			// something is.
			if len(err.Error()) < len("not implemented on this estate")+40 {
				t.Errorf("%s stub error %q says too little about what is missing", tc.name, err)
			}
		})
	}
}

// TestRevokeOverTheUnbuiltEstateReportsNoRevocation is the honest end-to-end of
// this package as it ships: wired to the only implementation that exists, it
// fails at the first stage and says the tenant was NOT revoked.
func TestRevokeOverTheUnbuiltEstateReportsNoRevocation(t *testing.T) {
	res, err := Revoke(context.Background(), NotImplementedEstate{}, validRequest())
	if err == nil {
		t.Fatal("Revoke over NotImplementedEstate returned nil — this platform cannot revoke a tenant yet")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("error %v does not wrap ErrNotImplemented", err)
	}
	if res.Revoked {
		t.Error("Result.Revoked is true over an estate where nothing is implemented")
	}
	if res.BrakeEngaged {
		t.Error("Result.BrakeEngaged is true although the halt stub refused")
	}
	if res.FailedAt != StageHalt {
		t.Errorf("Result.FailedAt = %s, want halt", res.FailedAt)
	}
	if summary := res.String(); !strings.Contains(summary, "NOT REVOKED") {
		t.Errorf("Result.String() = %q, want it to state that nothing was revoked", summary)
	}
}
