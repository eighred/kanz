package monitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/platform/subject"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

type lifecycleSink struct {
	mu      sync.Mutex
	events  []bus.Event
	records []comp.DecisionRecord
	publish func(context.Context, bus.Event) error
}

func (s *lifecycleSink) Publish(ctx context.Context, e bus.Event) error {
	if s.publish != nil {
		if err := s.publish(ctx, e); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}
func (s *lifecycleSink) Record(_ context.Context, r comp.DecisionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	return nil
}
func (s *lifecycleSink) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events), len(s.records)
}

type lifecycleMandate struct {
	comp.MandateSource
	used             atomic.Bool
	entered, release chan struct{}
}

func (p *lifecycleMandate) Mandate(ctx context.Context, tenant, portfolio string, at time.Time) (*compliancepb.Mandate, comp.Governance, error) {
	if p.used.CompareAndSwap(false, true) {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, comp.NeverMandated, ctx.Err()
		}
	}
	return p.MandateSource.Mandate(ctx, tenant, portfolio, at)
}

func lifecycleBook(t *testing.T) (levered, *lifecycleSink) {
	t.Helper()
	l := leveredBook(t, 3, 0, 100, -50_000)
	s := &lifecycleSink{}
	l.mon.emitter, l.mon.recorder = NewEmitter(s), s
	l.hold(t, 90_000) // marked gross100k / equity50k =2, below the3x cap
	return l, s
}

func lifecyclePosition(t *testing.T, l levered, quantity int64, at time.Time) {
	t.Helper()
	if err := l.mon.Handle(t.Context(), &envelopepb.Envelope{TenantId: "t1"}, positionEvent(t, "AAPL", quantity, quantity*90, at)); err != nil {
		t.Fatal(err)
	}
}

func lifecycleEvict(t *testing.T, l levered) {
	t.Helper()
	l.mon.now = clockAt(t0.Add(bookIdleRetention + 2*time.Minute))
	l.mon.mu.Lock()
	l.mon.gcBooksLocked()
	_, exists := l.mon.books[evictionKey]
	l.mon.mu.Unlock()
	l.mon.now = clockAt(t0)
	if exists {
		t.Fatal("fixture did not evict its flat book")
	}
}

func lifecycleStatus(m *Monitor) (compliancepb.ComplianceStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.books[evictionKey]
	if b == nil {
		return compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED, false
	}
	return b.lastStatus, true
}

func lifecyclePause(t *testing.T, m *Monitor) (func(), <-chan error) {
	t.Helper()
	p := &lifecycleMandate{MandateSource: m.mandates, entered: make(chan struct{}), release: make(chan struct{})}
	m.mandates = p
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan error, 1)
	go func() { defer close(done); done <- m.Reevaluate(ctx, "t1", "p1") }()
	var once sync.Once
	release := func() { once.Do(func() { close(p.release) }) }
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("paused evaluation did not join")
		}
	})
	select {
	case <-p.entered:
	case <-ctx.Done():
		t.Fatal("held capture never reached the real mandate lookup")
	}
	return release, done
}

func lifecycleJoin(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("evaluation error=%v want %v", err, want)
		}
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	case <-time.After(5 * time.Second):
		t.Fatal("evaluation did not complete")
	}
}

func lifecycleCounts(t *testing.T, s *lifecycleSink, wantEvents, wantRecords int) {
	t.Helper()
	if events, records := s.counts(); events != wantEvents || records != wantRecords {
		t.Fatalf("FACTs/decisions=%d/%d want%d/%d", events, records, wantEvents, wantRecords)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		br := e.Payload.(*compliancepb.ComplianceBreach)
		if br.GetPortfolioId() != "p1" || br.GetMandateId() != "m1" || br.GetMandateVersion() != 1 || br.GetResult().GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH || !br.GetDetectedAt().AsTime().Equal(e.EventTime) || !br.GetResult().GetEvaluatedAt().AsTime().Equal(e.EventTime) {
			t.Fatal("breach lost captured attribution", br)
		}
	}
	for _, r := range s.records {
		if r.TenantID != "t1" || r.Phase != comp.PhasePostTrade || r.Result.GetPortfolioId() != "p1" || r.Allowed {
			t.Fatal("decision lost owner/phase", r)
		}
	}
}

func TestLifecycleLateHeldResultCannotReplaceReopeningState(t *testing.T) {
	for _, state := range []string{"retained-empty", "evicted-empty", "retained-healthy", "retained-breach", "evicted-healthy", "evicted-breach"} {
		for _, old := range []string{"pass", "breach"} {
			t.Run(state+"-"+old, func(t *testing.T) {
				l, s := lifecycleBook(t)
				if old == "breach" {
					foldCash(t, l.cash, "p1", -70_000, t0)
				}
				release, done := lifecyclePause(t, l.mon)
				lifecyclePosition(t, l, 0, t0.Add(time.Second))
				if strings.HasPrefix(state, "evicted") {
					lifecycleEvict(t, l)
				}
				foldCash(t, l.cash, "p1", -70_000, t0)
				want := compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED
				count := 0
				if strings.HasSuffix(state, "healthy") {
					lifecyclePosition(t, l, 2_000, t0.Add(2*time.Second)) //200k/130k=20/13
					want = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS
				} else if strings.HasSuffix(state, "breach") {
					lifecyclePosition(t, l, 1_000, t0.Add(2*time.Second)) //100k/30k=10/3
					want = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
					count++
				}
				release()
				lifecycleJoin(t, done, nil)
				if old == "breach" {
					count++
				}
				lifecycleCounts(t, s, count, count)
				if got, _ := lifecycleStatus(l.mon); got != want {
					t.Fatalf("late%s replaced current%s with%s", old, want, got)
				}
				if state == "evicted-empty" {
					if _, exists := lifecycleStatus(l.mon); exists {
						t.Fatal("historical result recreated an evicted book")
					}
				}
				if !strings.HasSuffix(state, "breach") {
					lifecyclePosition(t, l, 1_000, t0.Add(3*time.Second))
					count++
				}
				if err := l.mon.ReevaluateAll(t.Context()); err != nil {
					t.Fatal(err)
				}
				lifecycleCounts(t, s, count, count)
			})
		}
	}
}

func TestLifecycleConcurrentCaptureCompletionKeepsNewestStatus(t *testing.T) {
	for _, pair := range []string{"pass-breach", "breach-pass", "breach-breach"} {
		t.Run(pair, func(t *testing.T) {
			l, s := lifecycleBook(t)
			if strings.HasPrefix(pair, "breach") {
				foldCash(t, l.cash, "p1", -70_000, t0)
			}
			release, done := lifecyclePause(t, l.mon)
			want := compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
			if strings.HasSuffix(pair, "pass") {
				foldCash(t, l.cash, "p1", -50_000, t0)
				want = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS
			} else {
				foldCash(t, l.cash, "p1", -70_000, t0)
			}
			if err := l.mon.Reevaluate(t.Context(), "t1", "p1"); err != nil {
				t.Fatal(err)
			}
			release()
			lifecycleJoin(t, done, nil)
			if got, _ := lifecycleStatus(l.mon); got != want {
				t.Fatal("older capture replaced newer result", got, want)
			}
			lifecycleCounts(t, s, 1, 1)
			if want == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
				foldCash(t, l.cash, "p1", -70_000, t0)
				if err := l.mon.Reevaluate(t.Context(), "t1", "p1"); err != nil {
					t.Fatal(err)
				}
				lifecycleCounts(t, s, 2, 2)
			} else {
				if err := l.mon.Reevaluate(t.Context(), "t1", "p1"); err != nil {
					t.Fatal(err)
				}
				lifecycleCounts(t, s, 1, 1)
			}
		})
	}
}

func TestLifecycleUnknownMarkerStillRefusesLateMeasuredResult(t *testing.T) {
	for _, kind := range []string{"canonical", "legacy"} {
		for _, old := range []string{"pass", "breach"} {
			t.Run(kind+"-"+old, func(t *testing.T) {
				l, s := lifecycleBook(t)
				if old == "breach" {
					foldCash(t, l.cash, "p1", -70_000, t0)
				}
				release, done := lifecyclePause(t, l.mon)
				key := "p1"
				if kind == "canonical" {
					key = subject.PositionFor("t1", "p1", "AAPL")
				}
				if err := positionIdentityFeed(t, l.mon, "t1", "p1", "", 1, 100, key); !errors.Is(err, errPositionIdentity) {
					t.Fatal(err)
				}
				release()
				lifecycleJoin(t, done, errPositionIdentity)
				lifecycleCounts(t, s, 0, 0)
				if got, _ := lifecycleStatus(l.mon); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED {
					t.Fatal("late measurement restored UNKNOWN status", got)
				}
				foldCash(t, l.cash, "p1", -70_000, t0)
				err := l.mon.Handle(t.Context(), &envelopepb.Envelope{TenantId: "t1", PartitionKey: subject.PositionFor("t1", "p1", "AAPL")}, positionEvent(t, "AAPL", 0, 0, t0))
				if kind == "legacy" {
					if !errors.Is(err, errPositionIdentity) {
						t.Fatal("individual holding repaired anonymous legacy slot", err)
					}
					lifecycleCounts(t, s, 0, 0)
				} else {
					if err != nil {
						t.Fatal(err)
					}
					lifecyclePosition(t, l, 1_000, t0.Add(time.Second))
					lifecycleCounts(t, s, 1, 1)
				}
			})
		}
	}
}

func TestLifecycleEmitFailureOnlyRollsBackItsReservation(t *testing.T) {
	for _, state := range []string{"current-retry", "standing-current-retry", "flat", "evicted", "healthy-reopen", "breaching-reopen", "newer-pass", "newer-breach", "position-revision-breach"} {
		t.Run(state, func(t *testing.T) {
			l, s := lifecycleBook(t)
			foldCash(t, l.cash, "p1", -70_000, t0)
			entered, release := make(chan struct{}), make(chan struct{})
			failure := errors.New("owned fixture publish failure")
			var used atomic.Bool
			s.publish = func(ctx context.Context, _ bus.Event) error {
				if !used.CompareAndSwap(false, true) {
					return nil
				}
				close(entered)
				select {
				case <-release:
					return failure
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { defer close(done); done <- l.mon.Reevaluate(ctx, "t1", "p1") }()
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(func() {
				unblock()
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("failed publication did not join")
				}
			})
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			want := compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED
			count := 0
			switch state {
			case "standing-current-retry":
				if err := l.mon.Reevaluate(t.Context(), "t1", "p1"); err != nil {
					t.Fatal(err)
				}
			case "flat", "evicted", "healthy-reopen", "breaching-reopen":
				lifecyclePosition(t, l, 0, t0.Add(time.Second))
				if state == "evicted" {
					lifecycleEvict(t, l)
				}
				if state == "healthy-reopen" {
					lifecyclePosition(t, l, 2_000, t0.Add(2*time.Second))
					want = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS
				}
				if state == "breaching-reopen" {
					lifecyclePosition(t, l, 1_000, t0.Add(2*time.Second))
					want = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
					count++
				}
			case "newer-pass", "newer-breach":
				foldCash(t, l.cash, "p1", -50_000, t0)
				if err := l.mon.Reevaluate(t.Context(), "t1", "p1"); err != nil {
					t.Fatal(err)
				}
				want = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS
				if state == "newer-breach" {
					foldCash(t, l.cash, "p1", -70_000, t0)
					if err := l.mon.Reevaluate(t.Context(), "t1", "p1"); err != nil {
						t.Fatal(err)
					}
					want = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
					count++
				}
			case "position-revision-breach":
				lifecyclePosition(t, l, 900, t0.Add(time.Second))
				want = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
				count++
			}
			unblock()
			lifecycleJoin(t, done, failure)
			if got, _ := lifecycleStatus(l.mon); got != want {
				t.Fatal("old failure reset a later status or failed to release its own reservation", got, want)
			}
			if state == "evicted" {
				if _, exists := lifecycleStatus(l.mon); exists {
					t.Fatal("old failure recreated evicted book")
				}
			}
			if want != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
				foldCash(t, l.cash, "p1", -70_000, t0)
				if state == "flat" || state == "evicted" || state == "healthy-reopen" {
					lifecyclePosition(t, l, 1_000, t0.Add(3*time.Second))
				} else if err := l.mon.Reevaluate(t.Context(), "t1", "p1"); err != nil {
					t.Fatal(err)
				}
				count++
			}
			if err := l.mon.ReevaluateAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			lifecycleCounts(t, s, count, count+1) // failed attempt already kept its decision
		})
	}
	t.Run("old-success-new-reservation-failure", func(t *testing.T) {
		l, s := lifecycleBook(t)
		foldCash(t, l.cash, "p1", -70_000, t0)
		entered := []chan struct{}{make(chan struct{}), make(chan struct{})}
		release := []chan struct{}{make(chan struct{}), make(chan struct{})}
		failure := errors.New("new reservation failed")
		var calls atomic.Int32
		s.publish = func(ctx context.Context, _ bus.Event) error {
			n := int(calls.Add(1)) - 1
			if n >= 2 {
				return nil
			}
			close(entered[n])
			select {
			case <-release[n]:
				if n == 1 {
					return failure
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		oldDone, newDone := make(chan error, 1), make(chan error, 1)
		go func() { defer close(oldDone); oldDone <- l.mon.Reevaluate(ctx, "t1", "p1") }()
		var releaseOnce [2]sync.Once
		unblockOne := func(n int) { releaseOnce[n].Do(func() { close(release[n]) }) }
		unblock := func() { unblockOne(0); unblockOne(1) }
		t.Cleanup(func() {
			unblock()
			cancel()
			for _, done := range []chan error{oldDone, newDone} {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("owned publication did not join")
				}
			}
		})
		select {
		case <-entered[0]:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		lifecyclePosition(t, l, 0, t0.Add(time.Second))
		go func() {
			defer close(newDone)
			newDone <- l.mon.Handle(ctx, &envelopepb.Envelope{TenantId: "t1"}, positionEvent(t, "AAPL", 1_000, 90_000, t0.Add(2*time.Second)))
		}()
		select {
		case <-entered[1]:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		unblockOne(0)
		lifecycleJoin(t, oldDone, nil)
		unblockOne(1)
		lifecycleJoin(t, newDone, failure)
		if got, _ := lifecycleStatus(l.mon); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_UNSPECIFIED {
			t.Fatal("old success stole newer reservation ownership", got)
		}
		if err := l.mon.Reevaluate(ctx, "t1", "p1"); err != nil {
			t.Fatal(err)
		}
		lifecycleCounts(t, s, 2, 3)
	})
}
