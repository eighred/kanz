package execution

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCloseBatchDoesNotStarveUnobservedIntents(t *testing.T) {
	registry := NewCloseRegistry()
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 201; i++ {
		if err := registry.Track(ctx, CloseIntent{OrderID: fmt.Sprintf("o%03d", i), InstrumentID: "BTC-USD", RequestedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for attempt := range 3 {
		due, err := registry.DueCloses(ctx, now.Add(time.Duration(attempt)*time.Minute), 0)
		if err != nil || len(due) > 100 {
			t.Fatalf("unbounded batch: %d %v", len(due), err)
		}
		for _, ci := range due {
			seen[ci.OrderID] = true
		}
	}
	if len(seen) != 201 {
		t.Fatalf("unobserved intents starved by retries: saw %d of 201", len(seen))
	}
}
