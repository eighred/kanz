package server

import (
	"net/http"
	"sync/atomic"
)

// Readiness is the liveness/readiness flag the probe endpoints report.
//
// It is set true only once the adapter can actually work an order: credentials
// present, order view open, bus connected, exchange connector started, venue.v1
// listening. A venue adapter that reports ready before it can trade is worse than
// one that reports unready — Kubernetes would send it traffic and the OMS would
// route live orders into a process that cannot fill them.
type Readiness struct{ ready atomic.Bool }

// Set marks the service ready (or not).
func (r *Readiness) Set(v bool) { r.ready.Store(v) }

// Ready reports the current state.
func (r *Readiness) Ready() bool { return r.ready.Load() }

// Probes returns the /healthz + /readyz (+ optional /metrics) surface. The gRPC
// venue.v1 endpoint is separate and on its own port — this one is for the
// kubelet, and it is deliberately not the port that can submit orders.
func Probes(readiness *Readiness, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	// Liveness: the process is up. It says nothing about the exchange.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Readiness: the adapter can work an order.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !readiness.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	if metrics != nil {
		mux.Handle("/metrics", metrics)
	}
	return mux
}
