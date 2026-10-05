package audit

import (
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

// A real TCP partition between the worker and the bootstrapped broker. The
// projector's independent connection stays up, as it would on another host.
func authorityNetwork(t *testing.T, address string) (string, func()) {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	upstream := u.Host
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u.Host = l.Addr().String()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var once sync.Once
	connections := map[net.Conn]bool{}
	closed := false
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			down, e := l.Accept()
			if e != nil {
				return
			}
			up, e := net.DialTimeout("tcp", upstream, time.Second)
			if e != nil {
				_ = down.Close()
				continue
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = down.Close()
				_ = up.Close()
				return
			}
			connections[down] = true
			connections[up] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(up, down); _ = up.Close(); _ = down.Close(); close(done) }()
				_, _ = io.Copy(down, up)
				_ = up.Close()
				_ = down.Close()
				<-done
				mu.Lock()
				delete(connections, up)
				delete(connections, down)
				mu.Unlock()
			}()
		}
	}()
	cut := func() {
		once.Do(func() {
			mu.Lock()
			closed = true
			_ = l.Close()
			for c := range connections {
				_ = c.Close()
			}
			mu.Unlock()
			wg.Wait()
		})
	}
	t.Cleanup(cut)
	return u.String(), cut
}
