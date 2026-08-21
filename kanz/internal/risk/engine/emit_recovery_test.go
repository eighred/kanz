package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/pkg/bus"
)

// flakyClient fails the first failures publishes of any subject containing
// match, then succeeds. It counts every attempt, including the failed ones,
// because "was it retried" is the question and a client that only counted
// successes could not answer it.
type flakyClient struct {
	mu       sync.Mutex
	match    string
	failures int
	attempts map[string]int
}

func newFlakyClient(match string, failures int) *flakyClient {
	return &flakyClient{match: match, failures: failures, attempts: map[string]int{}}
}

func (f *flakyClient) Publish(_ context.Context, msg bus.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[msg.Subject]++
	if strings.Contains(msg.Subject, f.match) && f.failures > 0 {
		f.failures--
		return context.DeadlineExceeded // any transport failure will do
	}
	return nil
}
func (f *flakyClient) Subscribe(context.Context, string, string, bus.Handler) error { return nil }
func (f *flakyClient) Close() error                                                 { return nil }

func (f *flakyClient) attemptsOn(match string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for subj, c := range f.attempts {
		if strings.Contains(subj, match) {
			n += c
		}
	}
	return n
}

// A FAILED EMIT ON A QUIET PORTFOLIO MUST RECOVER BY ITSELF (#620).
//
// This is the issue's own mutation, executed: fail EmitExposure once for a
// portfolio, then send NO further applies for it, and assert the platform does
// not go on serving the pre-failure exposure as current.
//
// THE "NO FURTHER APPLIES" HALF IS THE WHOLE TEST. The argument in recompute's
// doc was "publish failures are logged, not retried here — the bus.Producer owns
// delivery durability, and the next apply re-triggers a recompute anyway". That
// is true of a BUSY book and false of a quiet one: a portfolio that fails an
// emit and then stops trading has no next apply, so the last successfully
// published exposure stands as its live risk indefinitely and every consumer
// downstream treats it as current. A test that triggered twice would exercise
// the case that already worked.
func TestRecomputer_QuietPortfolioRecoversFromAFailedEmit(t *testing.T) {
	cc := newFlakyClient("exposure", 1)
	store := state.NewStore()
	cache := risk.NewCache()

	r := engine.NewRecomputer(
		context.Background(), store, compute.DefaultRegistry(), cache,
		newPublisher(t, cc), 10*time.Millisecond, discard(),
		engine.WithEmitRetryInterval(25*time.Millisecond),
	)
	defer r.Close()

	applyPosition(t, store, "pf-quiet", "AAPL", 1_000, time.Now())

	// ONE trigger. Nothing else happens to this portfolio for the rest of the test.
	r.Trigger("pf-quiet")

	waitFor(t, "the failed exposure emit to be retried without a second apply", func() bool {
		return cc.attemptsOn("exposure") >= 2
	})
}

// AND THE RETRY STOPS ONCE IT SUCCEEDS. A retry loop that never settles is its
// own defect — it would hold the portfolio dirty forever and keep the worker
// waking on a book that is fine.
func TestRecomputer_RetryStopsOnceTheEmitSucceeds(t *testing.T) {
	cc := newFlakyClient("exposure", 1)
	store := state.NewStore()
	cache := risk.NewCache()

	r := engine.NewRecomputer(
		context.Background(), store, compute.DefaultRegistry(), cache,
		newPublisher(t, cc), 10*time.Millisecond, discard(),
		engine.WithEmitRetryInterval(20*time.Millisecond),
	)
	defer r.Close()

	applyPosition(t, store, "pf-settles", "AAPL", 1_000, time.Now())
	r.Trigger("pf-settles")

	waitFor(t, "the retry to succeed", func() bool { return cc.attemptsOn("exposure") >= 2 })

	// Let several more retry windows elapse. If the portfolio were still being
	// re-armed after a SUCCESSFUL emit, the count would keep climbing.
	settled := cc.attemptsOn("exposure")
	time.Sleep(120 * time.Millisecond)
	if got := cc.attemptsOn("exposure"); got != settled {
		t.Fatalf("exposure emits kept climbing after success: %d -> %d over six retry windows.\n\n"+
			"A recompute that succeeded must clear the portfolio from the retry set; leaving it "+
			"armed holds the worker awake on a book whose published risk is already correct.",
			settled, got)
	}
}
