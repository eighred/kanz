package execution

import (
	"context"
	"testing"
	"time"
)

type placementContextKey struct{}

func TestPlacementRecoveryContextOutlivesTheAmbiguousPlacement(t *testing.T) {
	parent, stopPlacement := context.WithCancel(context.WithValue(
		context.Background(), placementContextKey{}, "trace-1"))
	stopPlacement()

	recovery, stopRecovery := NewPlacementRecoveryContext(parent)
	defer stopRecovery()

	if err := recovery.Err(); err != nil {
		t.Fatalf("recovery inherited the placement cancellation: %v", err)
	}
	if got := recovery.Value(placementContextKey{}); got != "trace-1" {
		t.Fatalf("recovery lost request values: got %v", got)
	}
	deadline, ok := recovery.Deadline()
	if !ok {
		t.Fatal("recovery has no deadline and can hold an execution worker forever")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > PlacementRecoveryTimeout {
		t.Fatalf("recovery deadline is outside its bound: remaining=%s bound=%s",
			remaining, PlacementRecoveryTimeout)
	}
}

func TestPlacementRecoveryContextCancelReleasesIt(t *testing.T) {
	recovery, cancel := NewPlacementRecoveryContext(context.Background())
	cancel()
	select {
	case <-recovery.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel did not release the bounded recovery context")
	}
}
