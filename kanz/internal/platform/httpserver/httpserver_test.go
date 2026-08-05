package httpserver_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/platform/httpserver"
)

// TestNewRefusesAMissingTimeout is the whole point of the package: the partial
// struct literal that shipped twenty-six times must not be expressible.
//
// One subtest per field, because a loop that checked "some field missing" would
// pass if New only ever noticed the first one.
func TestNewRefusesAMissingTimeout(t *testing.T) {
	for _, tc := range []struct {
		field string
		mut   func(*httpserver.Timeouts)
	}{
		{"ReadHeader", func(x *httpserver.Timeouts) { x.ReadHeader = 0 }},
		{"Read", func(x *httpserver.Timeouts) { x.Read = 0 }},
		{"Write", func(x *httpserver.Timeouts) { x.Write = 0 }},
		{"Idle", func(x *httpserver.Timeouts) { x.Idle = 0 }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			to := httpserver.Standard()
			tc.mut(&to)

			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("New built a server with %s unset. Absence is not a looser "+
						"bound, it is no bound — that is the leak this package closes.", tc.field)
				}
				// The message has to NAME the field, or an operator reading a crash
				// loop learns only that something is wrong with the timeouts.
				if msg := fmt.Sprint(r); !strings.Contains(msg, tc.field) {
					t.Errorf("panic message does not name %s: %s", tc.field, msg)
				}
			}()

			_ = httpserver.New(":0", http.NewServeMux(), to)
		})
	}
}

// TestStandardBuildsAFullyBoundedServer pins that every field reaches the server.
// A constructor that validated its input and then dropped a field on the floor
// would satisfy every other test here.
func TestStandardBuildsAFullyBoundedServer(t *testing.T) {
	to := httpserver.Standard()
	srv := httpserver.New("127.0.0.1:0", http.NewServeMux(), to)

	for _, f := range []struct {
		name      string
		got, want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, to.ReadHeader},
		{"ReadTimeout", srv.ReadTimeout, to.Read},
		{"WriteTimeout", srv.WriteTimeout, to.Write},
		{"IdleTimeout", srv.IdleTimeout, to.Idle},
	} {
		if f.got != f.want {
			t.Errorf("http.Server.%s = %v, want %v", f.name, f.got, f.want)
		}
	}
	if srv.Addr != "127.0.0.1:0" {
		t.Errorf("Addr = %q", srv.Addr)
	}
}

// TestWriteTimeoutSeversAStreamAndTheResponseControllerLiftsIt is the measurement
// the package comment cites, kept executable.
//
// It is here rather than in a comment because the escape hatch for tv-sync's SSE
// endpoint rests on it: if a future Go release stopped honouring
// SetWriteDeadline(zero) on the response, the estate's one streaming endpoint would
// start dying at WriteTimeout with no status and nothing logged, and the only
// evidence would be a sentence in a doc comment. Short durations so the test is
// fast; the mechanism is what is under test, not the numbers.
func TestWriteTimeoutSeversAStreamAndTheResponseControllerLiftsIt(t *testing.T) {
	const (
		writeTimeout = 300 * time.Millisecond
		frameGap     = 100 * time.Millisecond
		frames       = 10
	)

	run := func(t *testing.T, lift bool) (int, error) {
		t.Helper()

		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			if lift {
				if err := rc.SetWriteDeadline(time.Time{}); err != nil {
					t.Errorf("SetWriteDeadline: %v", err)
					return
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for i := range frames {
				if _, err := fmt.Fprintf(w, "event: tick\ndata: %d\n\n", i); err != nil {
					return
				}
				if err := rc.Flush(); err != nil {
					return
				}
				time.Sleep(frameGap)
			}
		}))
		srv.Config.WriteTimeout = writeTimeout
		srv.Start()
		t.Cleanup(srv.Close)

		resp, err := srv.Client().Get(srv.URL)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(resp.Body)
		return strings.Count(string(body), "event: tick"), readErr
	}

	t.Run("bounded", func(t *testing.T) {
		got, err := run(t, false)
		if got >= frames {
			t.Fatalf("all %d frames arrived under a %v WriteTimeout — WriteTimeout no longer "+
				"bounds a streaming response, so the escape hatch below is guarding nothing "+
				"and tv-sync's SSE endpoint no longer needs it", frames, writeTimeout)
		}
		t.Logf("severed after %d/%d frames, err %v — this is the failure the hatch exists for",
			got, frames, err)
	})

	t.Run("lifted", func(t *testing.T) {
		got, err := run(t, true)
		if got != frames {
			t.Fatalf("only %d/%d frames arrived (err %v) even though the handler cleared its "+
				"write deadline with http.NewResponseController(w).SetWriteDeadline(time.Time{}).\n\n"+
				"That is the ONLY thing keeping tv-sync's Server-Sent-Events endpoint alive "+
				"under the estate's WriteTimeout. If it has stopped working, that stream is now "+
				"being severed mid-flight with no status and nothing logged, and the fix is not "+
				"to drop WriteTimeout from that server.", got, frames, err)
		}
	})
}

// TestReadTimeoutDoesNotCancelARunningHandler pins the other half of the
// measurement — the half that says the escape hatch needs to clear the WRITE
// deadline only.
//
// If this ever fails, every handler that outlives Timeouts.Read is being cancelled
// mid-flight across the whole estate, which would make the standard 30s Read a
// silent 30s cap on the copilot's 90s /v1/ask budget. It is exactly the class of
// second, invisible bound that probe_deadline_nesting_test.go exists to forbid.
func TestReadTimeoutDoesNotCancelARunningHandler(t *testing.T) {
	const readTimeout = 200 * time.Millisecond

	outcome := make(chan string, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			outcome <- "cancelled: " + r.Context().Err().Error()
		case <-time.After(5 * readTimeout):
			outcome <- "survived"
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ReadTimeout = readTimeout
	srv.Start()
	defer srv.Close()

	go func() {
		resp, err := srv.Client().Get(srv.URL)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	if got := <-outcome; got != "survived" {
		t.Fatalf("a handler still running past ReadTimeout was %s.\n\n"+
			"Timeouts.Read would then be a hidden bound on every handler in the estate, "+
			"under the per-call budgets that are supposed to be the only ones — including "+
			"the copilot's 90s /v1/ask. Either Read must be raised above the longest handler "+
			"budget, or streaming/long handlers must clear the read deadline too.", got)
	}
}
