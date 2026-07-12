package depth

import (
	"context"
	"testing"
	"time"
)

// recvOK is the shared assertion helper for the live venue depth sources: one
// bounded Recv that must not error.
func recvOK(t *testing.T, s DepthSource) Update {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := s.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	return u
}
