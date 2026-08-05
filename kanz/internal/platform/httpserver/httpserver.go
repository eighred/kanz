// Package httpserver builds the one http.Server every composition root in this
// estate listens on, and it cannot be built with a timeout missing.
//
// WHY IT EXISTS. Twenty-six composition roots each wrote the same partial struct
// literal — `Addr`, `Handler`, `ReadHeaderTimeout: 5 * time.Second`, nothing else
// (#235). ReadHeaderTimeout stops timing the moment the last header byte arrives,
// so past that point NOTHING bounded the connection: a caller that stopped reading
// the response, or an intermediary that wedged, pinned a goroutine and a file
// descriptor for as long as it liked, and with IdleTimeout AND ReadTimeout both
// unset net/http applies no idle bound at all, so every keep-alive connection ever
// opened is held until the peer closes it. Under a pooling proxy that is not a slow
// leak, it is the steady state. The gateway is the sole ingress for orders, so
// enough held descriptors anywhere on that path and no order can be submitted.
//
// WHY A CONSTRUCTOR RATHER THAN A DOCUMENTED CONVENTION. The partial literal was
// copied twenty-six times and was wrong twenty-six times; the fix has to be in the
// place the copies came from or it does not spread — the same lesson pkg/secret
// records, where fifteen roots kept an error-discarding copy for weeks while two
// had the repair. A struct literal cannot require a field. New can, and does: a
// zero in any of the four is refused at the first line of main, in every
// environment, on every run.
//
// THE ESCAPE HATCH FOR STREAMING, AND WHY IT IS NOT A NIL TIMEOUT. WriteTimeout
// bounds the WHOLE response, so it severs a Server-Sent-Events stream mid-flight —
// measured on go1.26.5: with WriteTimeout 300ms an SSE handler emitting a frame
// every 100ms delivered 3 of 10 frames and the client got an unexpected EOF, no
// status, nothing logged. The answer is NOT to leave WriteTimeout off the servers
// that stream, which would put the estate's one unbounded server back and hide it
// behind a legitimate reason. It is http.NewResponseController(w).SetWriteDeadline
// (time.Time{}), which lifts the bound for THAT ONE CONNECTION and leaves every
// other route on the same server bounded. Same measurement, with the deadline
// lifted: 10 of 10 frames. services/tv-sync/internal/brokerapi is the one handler
// in the estate that needs it, and test/arch/http_server_timeouts_test.go requires
// any future one to do the same.
//
// ReadTimeout does NOT need that treatment: it bounds the request read, and a
// handler that outlives it keeps a live request context (measured the same way —
// with ReadTimeout 300ms a handler still running at 1.5s was never cancelled, and
// an SSE stream under ReadTimeout alone delivered all 10 frames). Only WriteTimeout
// reaches the response.
package httpserver

import (
	"fmt"
	"net/http"
	"time"
)

// Timeouts is the complete set of connection bounds for one server. All four are
// required — see New.
//
// They are a keyed literal rather than four arguments so that
// test/arch/probe_deadline_nesting_test.go can read the gateway's overrides back
// out of the source by field name and order them against the handler budgets they
// have to clear. A positional signature would make that guard read arguments by
// index, which is how a guard silently starts asserting about the wrong number.
type Timeouts struct {
	// ReadHeader bounds the request line and headers. It is the slowloris bound,
	// and it is the ONLY one of the four this estate has ever set.
	ReadHeader time.Duration

	// Read bounds the whole request, headers plus body. It does not reach the
	// handler: a handler still running when it expires is not cancelled, so this
	// is a bound on a slow SENDER, nothing else.
	Read time.Duration

	// Write bounds the whole response, and its clock starts when the request is
	// read — so it bounds a handler that thinks for a long time and then writes
	// once, not merely one that writes slowly. It fires by severing the
	// connection: no status, no body, nothing naming what was slow. That makes it
	// the crude outermost backstop, and it must clear every deliberate per-call
	// budget a caller applies to this server, or it silently becomes the real
	// limit and takes the diagnosis away from the layer that has it.
	Write time.Duration

	// Idle bounds a keep-alive connection between requests. ABSENCE IS THE TRAP:
	// with Idle and Read both zero net/http applies NO idle bound. Too SHORT is
	// its own fault and not a safer one — a server that closes connections a
	// pooling client still believes it may reuse produces intermittent 502s under
	// no load at all, which is harder to read than the leak this closes.
	Idle time.Duration
}

// The estate default, for the twenty-five services that sit behind the gateway on
// the mesh. Each number is derived from a bound that already exists somewhere
// else; none is a round number chosen for looking reasonable.
const (
	// Unchanged from the value all twenty-six roots already ran in production, so
	// this change moves no bound that was previously enforced.
	standardReadHeaderTimeout = 5 * time.Second

	// The gateway abandons a proxied read at proxy.readForwardTimeout (30s), and a
	// NetworkPolicy makes the gateway the only caller these services have. A body
	// still arriving after that has no reader left waiting for it.
	standardReadTimeout = 30 * time.Second

	// MUST EXCEED THE LONGEST BUDGET ANY CALLER APPLIES TO THESE SERVICES, which is
	// proxy.copilotForwardTimeout (90s) on POST /v1/ask. Below it, the upstream
	// severs the connection first and the caller gets an anonymous EOF instead of
	// the gateway's own 502 naming the upstream that was slow — the same inversion
	// gatewayWriteTimeout is sized to avoid one layer further out. Guarded against
	// that const by test/arch/http_server_timeouts_test.go.
	standardWriteTimeout = 120 * time.Second

	// MUST EXCEED THE IDLE TIMEOUT OF THE CLIENT THAT POOLS CONNECTIONS HERE — the
	// gateway's proxy transport, pinned to proxyIdleConnTimeout (90s) in
	// cmd/api-gateway so this ordering is something a test can read rather than a
	// property of http.DefaultTransport's defaults. The gateway must be the side
	// that retires an idle connection; if this server retires it first, the
	// gateway sends on a socket that is already closing and the race surfaces as
	// intermittent 502s under no load.
	standardIdleTimeout = 120 * time.Second
)

// Standard returns the estate default bounds. It is the value every service
// behind the gateway passes; only the gateway itself overrides, because it is the
// only server with an ingress in front of it and handler budgets of its own.
func Standard() Timeouts {
	return Timeouts{
		ReadHeader: standardReadHeaderTimeout,
		Read:       standardReadTimeout,
		Write:      standardWriteTimeout,
		Idle:       standardIdleTimeout,
	}
}

// New builds the server. It PANICS if any of the four is zero.
//
// A panic rather than an error, deliberately. A zero here is not a configuration
// fault an operator can cause — Standard() cannot produce one and the environment
// cannot reach these fields — it is a wiring mistake, and it is reachable only by
// writing httpserver.Timeouts{} by hand. Returning an error would put twenty-six
// impossible `if err != nil { return 2 }` branches into the composition roots,
// which is how the branches that CAN fire stop standing out. This fires on the
// first line of main, before a listener exists, identically in every environment,
// so it cannot reach production as a server that came up looking healthy with a
// bound missing — which is the entire failure this package exists to end.
func New(addr string, h http.Handler, t Timeouts) *http.Server {
	for _, f := range []struct {
		name string
		d    time.Duration
		cost string
	}{
		{"ReadHeader", t.ReadHeader,
			"a client that opens a connection and dribbles headers holds it indefinitely"},
		{"Read", t.Read,
			"a client that sends a body one byte at a time holds the connection indefinitely, " +
				"and with Idle also zero net/http falls back to this one and applies no idle bound either"},
		{"Write", t.Write,
			"a client that stops reading the response — or a wedged intermediary — pins a " +
				"goroutine and a file descriptor until the peer closes, which under a pooling " +
				"proxy means until the process restarts"},
		{"Idle", t.Idle,
			"net/http falls back to Read, and if that is zero too there is NO idle bound at " +
				"all: every keep-alive connection ever opened is held until the peer closes it"},
	} {
		if f.d <= 0 {
			panic(fmt.Sprintf(
				"httpserver.New(%q): Timeouts.%s is not set.\n\n"+
					"Absence is not a looser bound, it is NO bound: %s.\n\n"+
					"Pass httpserver.Standard(), or override a field of it — do not build a "+
					"Timeouts value from scratch.", addr, f.name, f.cost))
		}
	}

	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: t.ReadHeader,
		ReadTimeout:       t.Read,
		WriteTimeout:      t.Write,
		IdleTimeout:       t.Idle,
	}
}
