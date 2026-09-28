package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdministrativeBodyDeliversCloseResponsesBeforeAnyMutation(t *testing.T) {
	var mutations atomic.Int64
	srv := httptest.NewServer(administrativeBody(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			writeErr(w, http.StatusForbidden, "refused")
			return
		}
		if !emptyAdministrativeBody(w, r) {
			return
		}
		mutations.Add(1)
		writeJSON(w, http.StatusOK, map[string]bool{"changed": true})
	}))
	defer srv.Close()
	for range 30 {
		for _, tc := range []struct {
			body       string
			authorized bool
			status     int
		}{
			{"", true, 200}, {"{}", true, 200}, {"{}", false, 403},
			{`{"unexpected":true}`, true, 400}, {"null", true, 400},
		} {
			req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Close = true
			if tc.authorized {
				req.Header.Set("Authorization", "test-authority")
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("response reset for %q: %v", tc.body, err)
			}
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d read=%v", resp.StatusCode, tc.status, err)
			}
		}
	}
	if mutations.Load() != 60 {
		t.Fatalf("refused bodies mutated state: %d", mutations.Load())
	}
}

func TestAdministrativeBodyBoundsDeclaredAndChunkedStreams(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(administrativeBody(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer srv.Close()
	for _, chunked := range []bool{false, true} {
		conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err = conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		body := strings.Repeat("x", maxBody+1)
		header := fmt.Sprintf("Content-Length: %d\r\n", maxBody*256)
		if chunked {
			header = "Transfer-Encoding: chunked\r\n"
			body = fmt.Sprintf("%x\r\n%s\r\n", len(body), body)
		}
		_, err = fmt.Fprintf(conn, "PUT / HTTP/1.1\r\nHost: identity.test\r\nConnection: close\r\n%s\r\n%s", header, body)
		if err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
		// Never finish the advertised body: a drain-until-EOF implementation hangs.
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
		if resp.StatusCode != 413 {
			t.Errorf("oversize status=%d", resp.StatusCode)
		}
		_ = conn.Close()
		_ = resp.Body.Close()
	}
	if calls.Load() != 0 {
		t.Fatal("oversized stream reached mutation")
	}
}

func TestAdministrativeBodyDeadlineRefusesSlowAndCancelledCommands(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(administrativeBody(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(administrativeBodyTimeout + 3*time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprint(conn, "PUT / HTTP/1.1\r\nHost: identity.test\r\nConnection: close\r\nContent-Length: 10\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 408 {
		t.Errorf("slow body status=%d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	cancelled, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprint(cancelled, "PUT / HTTP/1.1\r\nHost: identity.test\r\nContent-Length: 10\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	_ = cancelled.Close()
	srv.CloseClientConnections()
	if calls.Load() != 0 {
		t.Fatal("partial body reached mutation")
	}
}
