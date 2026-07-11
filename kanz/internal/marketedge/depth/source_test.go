package depth

import (
	"context"
	"testing"
	"time"
)

func TestSimSource_SnapshotThenChainedDeltas(t *testing.T) {
	s := NewSimSource(SimConfig{InstrumentID: "BTC-USD", Symbol: "BTCUSDT", Interval: time.Millisecond, Seed: 42})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	first, err := s.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv snapshot: %v", err)
	}
	if first.Snapshot == nil || first.Delta != nil {
		t.Fatal("first update must be a snapshot")
	}
	if first.Snapshot.GetMic() != "SIM" {
		t.Fatalf("mic = %q, want SIM (never presented as a real venue)", first.Snapshot.GetMic())
	}
	prev := first.Snapshot.GetLastUpdateSequence()

	// Subsequent updates are deltas that chain by sequence.
	for i := 0; i < 5; i++ {
		u, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv delta %d: %v", i, err)
		}
		if u.Delta == nil {
			t.Fatalf("update %d must be a delta", i)
		}
		if u.Delta.GetPrevUpdateSequence() != prev {
			t.Fatalf("delta prev_seq = %d, want %d (must chain)", u.Delta.GetPrevUpdateSequence(), prev)
		}
		prev = u.Delta.GetLastUpdateSequence()
	}
}

func TestSimSource_Deterministic(t *testing.T) {
	drain := func() uint64 {
		s := NewSimSource(SimConfig{InstrumentID: "X", Interval: time.Millisecond, Seed: 7})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var lastSeq uint64
		for i := 0; i < 6; i++ {
			u, err := s.Recv(ctx)
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			if u.Snapshot != nil {
				lastSeq = u.Snapshot.GetLastUpdateSequence()
			} else {
				lastSeq = u.Delta.GetLastUpdateSequence()
			}
		}
		return lastSeq
	}
	if a, b := drain(), drain(); a != b {
		t.Fatalf("sim not deterministic: %d != %d", a, b)
	}
}
