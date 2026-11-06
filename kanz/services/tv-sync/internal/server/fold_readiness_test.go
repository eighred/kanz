package server

import (
	"context"
	"fmt"
	"testing"
)

func TestFoldReadinessRequiresMatchingRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewReadiness(ctx)
	r.Set(true)
	r.FoldResult("a", "one", true)
	r.FoldResult("a", "two", true)
	r.Set(true)
	r.FoldResult("b", "one", false)
	r.FoldResult("a", "other", false)
	if r.Ready() {
		t.Fatal("unrelated completion/catch-up cleared failed fold")
	}
	r.FoldResult("a", "one", false)
	if r.Ready() {
		t.Fatal("one completion cleared another failed fold")
	}
	r.FoldResult("a", "two", false)
	if !r.Ready() {
		t.Fatal("matched retries did not restore readiness")
	}
	cancel()
	r.Set(true)
	if r.Ready() {
		t.Fatal("cancelled process reopened")
	}
}
func TestFoldReadinessOverflowAndMissingIdentityStayClosed(t *testing.T) {
	for _, missing := range []bool{false, true} {
		r := NewReadiness(context.Background())
		r.Set(true)
		if missing {
			r.FoldResult("", "", true)
		} else {
			for i := 0; i <= maxUnresolvedFolds; i++ {
				r.FoldResult("a", fmt.Sprint(i), true)
			}
		}
		for i := 0; i <= maxUnresolvedFolds; i++ {
			r.FoldResult("a", fmt.Sprint(i), false)
		}
		r.Set(true)
		if r.Ready() {
			t.Fatal("overflow/unknown identity reopened")
		}
		if len(r.foldFailures.pending) > maxUnresolvedFolds {
			t.Fatal("unbounded failure map")
		}
	}
}
