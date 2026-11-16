package positionfixture

import (
	"errors"
	"testing"
	"time"
)

type joinedReceiptReader struct {
	stopping chan struct{}
	waiting  chan struct{}
	closed   chan struct{}
}

func (r *joinedReceiptReader) Stop() { close(r.stopping) }
func (r *joinedReceiptReader) Closed() <-chan struct{} {
	close(r.waiting)
	return r.closed
}

func TestPositionReceiptCleanupWaitsForReaderAndClearsAliases(t *testing.T) {
	nested := map[string]bool{"aggregate": true, "venue": true}
	outer := map[string]map[string]bool{"input": nested}
	keys := map[string]bool{"aggregate": true, "venue": true}
	wantErr := errors.New("receipt failed before cleanup")
	r := &PositionReceipts{seen: outer, keys: keys, err: wantErr}
	reader := &joinedReceiptReader{stopping: make(chan struct{}), waiting: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- r.close(reader) }()
	select {
	case <-reader.stopping:
	case <-time.After(5 * time.Second):
		close(reader.closed)
		t.Fatal("cleanup did not request reader stop")
	}
	select {
	case <-reader.waiting:
	case <-time.After(5 * time.Second):
		close(reader.closed)
		t.Fatal("cleanup did not observe the reader's join channel")
	}
	select {
	case <-done:
		close(reader.closed)
		t.Fatal("cleanup returned before the reader joined")
	case <-time.After(50 * time.Millisecond):
	}
	r.mu.Lock()
	beforeJoin := len(nested) == 2 && len(outer) == 1 && len(keys) == 2
	r.mu.Unlock()
	close(reader.closed)
	if !beforeJoin {
		t.Error("cleanup discarded receipts before the reader joined")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish after the reader joined")
	}
	if len(nested) != 0 || len(outer) != 0 || len(keys) != 0 {
		t.Fatal("cleanup retained nested receipts or ownership keys")
	}
	if !errors.Is(r.err, wantErr) {
		t.Fatal("cleanup erased an earlier receipt failure")
	}
	if err := r.close(nil); err != nil {
		t.Fatal("repeated cleanup without a reader refused")
	}
}

func TestPositionReceiptCleanupWithoutReader(t *testing.T) {
	nested := map[string]bool{"aggregate": true}
	r := &PositionReceipts{seen: map[string]map[string]bool{"input": nested}, keys: map[string]bool{"aggregate": true}}
	if err := r.close(nil); err != nil {
		t.Fatal(err)
	}
	if len(nested) != 0 || len(r.seen) != 0 || len(r.keys) != 0 {
		t.Fatal("inactive fixture retained its collections")
	}
}
