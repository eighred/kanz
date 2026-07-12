package server

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// Readiness is the adapter's readiness authority. It answers a harder question
// than "is the process up": can this adapter DO ITS JOB?
//
// An exchange adapter has TWO outputs, and both must work:
//
//   - SYNCHRONOUS — venue.v1 gRPC (Execute, CancelOrder). If the process is
//     listening, this works.
//   - ASYNCHRONOUS — the fill FACTs it publishes to the bus when a resting order
//     fills on the user-data websocket, and the StateHealed FACTs its reconciler
//     emits. MOST FILLS ARRIVE THIS WAY.
//
// An adapter whose publishes all fail still answers gRPC perfectly. It will accept
// an order, work it at the exchange, and then LOSE THE FILL: the fund's money moved
// and the OMS never heard. It looks healthy the whole time — which is exactly how
// market-ingest hid the identical bug, folding its book while emitting nothing with
// /readyz at 200.
//
// So publish health IS readiness here. When publishing fails, this adapter drops
// out of its Service, the OMS's dial to it fails, and the router hard-errors on
// that MIC instead of routing orders into a venue that cannot report what it did
// with them. Refusing to trade beats trading blind.
type Readiness struct {
	ready  atomic.Bool
	health atomic.Pointer[bus.HealthPublisher]
}

// Set marks process-level startup complete (or not): credentials loaded, order
// view open, bus connected, connector started, venue.v1 listening.
func (r *Readiness) Set(v bool) { r.ready.Store(v) }

// Ready reports the process-level flag only. Status is the full picture.
func (r *Readiness) Ready() bool { return r.ready.Load() }

// TrackPublisher attaches the publish-health tracker once the producer exists.
//
// The probe server starts BEFORE the bus is dialed — deliberately, so a slow
// exchange handshake does not read as a crash — so this is attached afterwards
// rather than passed in. An atomic pointer, because the handlers are already
// serving by then.
func (r *Readiness) TrackPublisher(h *bus.HealthPublisher) { r.health.Store(h) }

// Status reports whether the adapter is ready, and if not, WHY — so the endpoint
// tells an operator what it already knows rather than sending them to the logs.
func (r *Readiness) Status() (bool, string) {
	if !r.ready.Load() {
		return false, "not ready: starting up"
	}
	h := r.health.Load()
	if h == nil {
		return true, "ready"
	}
	if ok, consecutive, lastErr := h.Status(); !ok {
		return false, fmt.Sprintf(
			"not ready: %d consecutive publish failures — fills and healing FACTs are NOT reaching the bus, "+
				"so an order worked here would be executed and then LOST. last error: %v",
			consecutive, lastErr)
	}
	return true, "ready"
}

// Probes returns the /healthz + /readyz (+ optional /metrics) surface. The gRPC
// venue.v1 endpoint is separate and on its own port — this one is for the
// kubelet, and it is deliberately not the port that can submit orders.
func Probes(readiness *Readiness, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	// Liveness: the process is up. It says nothing about the exchange or the bus,
	// and it must not — restarting a pod does not fix a missing stream or a
	// malformed envelope, and a crash-loop would only bury the reason.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	// Readiness: this adapter can work an order AND report what it did with it.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		ok, reason := readiness.Status()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, reason)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, reason)
	})
	if metrics != nil {
		mux.Handle("/metrics", metrics)
	}
	return mux
}
