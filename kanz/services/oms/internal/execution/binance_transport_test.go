//go:build binance

package execution

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestCachedDialer_ResolvesOncePerTTL(t *testing.T) {
	var mu sync.Mutex
	resolves := 0
	clock := time.Unix(1_700_000_000, 0)

	d := &cachedDialer{
		resolve: func(_ context.Context, host string) ([]string, error) {
			mu.Lock()
			resolves++
			mu.Unlock()
			return []string{"93.184.216.34"}, nil
		},
		// Stub dial: assert the host was replaced by the cached IP, return a
		// no-op conn (a closed pipe end).
		dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			if host, _, _ := net.SplitHostPort(addr); host != "93.184.216.34" {
				t.Errorf("dialed %q, want the cached IP", addr)
			}
			c, _ := net.Pipe()
			return c, nil
		},
		ttl:   time.Minute,
		now:   func() time.Time { return clock },
		cache: map[string]dnsEntry{},
	}

	for i := 0; i < 3; i++ {
		conn, err := d.DialContext(context.Background(), "tcp", "api.binance.com:443")
		if err != nil {
			t.Fatalf("DialContext: %v", err)
		}
		_ = conn.Close()
	}
	// Three live dials, one resolution — the hot path never re-resolves.
	if resolves != 1 {
		t.Fatalf("resolved %d times across 3 dials, want 1 (no repeated blocking DNS)", resolves)
	}

	// Past the TTL, one more resolution refreshes the entry.
	clock = clock.Add(2 * time.Minute)
	conn, _ := d.DialContext(context.Background(), "tcp", "api.binance.com:443")
	_ = conn.Close()
	if resolves != 2 {
		t.Fatalf("resolved %d times after TTL, want 2", resolves)
	}
}

func TestCachedDialer_IPLiteralSkipsResolution(t *testing.T) {
	resolved := false
	d := &cachedDialer{
		resolve: func(context.Context, string) ([]string, error) { resolved = true; return nil, nil },
		dial:    func(context.Context, string, string) (net.Conn, error) { c, _ := net.Pipe(); return c, nil },
		ttl:     time.Minute, now: time.Now, cache: map[string]dnsEntry{},
	}
	conn, err := d.DialContext(context.Background(), "tcp", "1.2.3.4:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if resolved {
		t.Fatal("an IP literal must not trigger a DNS lookup")
	}
}
