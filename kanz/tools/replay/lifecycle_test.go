package replay

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func requireClosed(t *testing.T, r *Reader) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not join reader lifecycle")
	}
}

func TestReaderCloseBeforeNext(t *testing.T) {
	r, err := NewReader(Config{Brokers: []string{"127.0.0.1:1"}, Topic: "fixture", Range: Range{EndOffset: ptrInt64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	requireClosed(t, r)
	requireClosed(t, r)
	if _, err := r.Next(context.Background()); err == nil {
		t.Fatalf("Next after Close: %v", err)
	}
}

func TestReaderCloseAfterDiscoveryFailure(t *testing.T) {
	r, err := NewReader(Config{Brokers: []string{"127.0.0.1:1"}, Topic: "fixture", Range: Range{EndOffset: ptrInt64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Next(ctx); err == nil {
		t.Fatal("canceled discovery accepted")
	}
	requireClosed(t, r)
}

// The peer accepts a TCP connection but never answers Kafka metadata. Close
// must interrupt the established connection, not wait for a dial timeout.
func TestReaderCloseInterruptsMetadata(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	}()
	r, err := NewReader(Config{Brokers: []string{listener.Addr().String()}, Topic: "fixture", Range: Range{EndOffset: ptrInt64(1)}, PartitionDialTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	next := make(chan error, 1)
	go func() { _, err := r.Next(context.Background()); next <- err }()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata connection not attempted")
	}
	requireClosed(t, r)
	select {
	case err := <-next:
		if err == nil {
			t.Fatal("interrupted metadata accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not finish")
	}
	select {
	case <-peerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata connection retained after Close")
	}
}
