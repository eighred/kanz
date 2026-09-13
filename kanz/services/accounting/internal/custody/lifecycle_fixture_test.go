package custody

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
)

// Existing reconciliation tests need a previously investigated break. Durable
// fixtures now go through real audited actions; volatile fixtures only exercise
// reconciliation carry-forward, never claim to prove durable human evidence.
func recordFixtureLifecycle(t *testing.T, st Store, ctx context.Context, desired Break) error {
	t.Helper()
	if memory, ok := st.(*MemoryStore); ok {
		memory.mu.Lock()
		defer memory.mu.Unlock()
		if _, ok := memory.breaks[desired.BreakID]; !ok {
			return ErrNoBreak
		}
		memory.breaks[desired.BreakID] = desired
		return nil
	}
	current, err := st.LoadBreak(ctx, desired.BreakID)
	if err != nil {
		return err
	}
	actor := desired.Assignee
	if actor == "" {
		actor = "custody-test-operator"
	}
	apply := func(kind, explanation string) error {
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%d", t.Name(), desired.BreakID, kind, current.Revision))))
		e, err := st.ApplyAction(ctx, Action{Tenant: "__system__", Actor: actor, RequestID: key, BreakID: desired.BreakID, Kind: kind, Explanation: explanation, ExpectedRevision: current.Revision})
		if err == nil {
			current.Revision = e.After.Revision
		}
		return err
	}
	if desired.Assignee != current.Assignee {
		if err := apply("claim", ""); err != nil {
			return err
		}
	}
	if desired.Status == BreakExplained {
		return apply("explain", desired.Explanation)
	}
	return nil
}
