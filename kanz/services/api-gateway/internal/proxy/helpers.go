package proxy

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// maxBodyBytes is middleware.MaxRequestBody, not a fourth copy of 1 MiB (#887).
//
// This number was declared THREE times across the api-gateway — here,
// internal/orders and internal/proxy — each with its own comment justifying the
// same ceiling. Three spellings of one bound is how it gets raised in one place
// and not the others. The middleware package owns it because that layer applies
// it BEFORE authentication, and an arch guard ties it to the edge's own
// proxy-body-size so the two cannot drift.
const maxBodyBytes = middleware.MaxRequestBody

// ErrBackendUnavailable is returned by a Backend when the targeted upstream is
// not wired (the per-service client is absent at the composition root). The
// proxy maps it to 503, distinct from a 502 upstream fault.
var ErrBackendUnavailable = errors.New("proxy: upstream backend unavailable")

// writeForwardError maps a forwarding failure to a client status: an unwired
// upstream is 503; anything else is a 502 (upstream/transport fault).
func writeForwardError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrBackendUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "upstream service unavailable")
		return
	}
	writeError(w, http.StatusBadGateway, "failed to reach upstream service")
}

// writeUpstream writes the upstream response verbatim — the gateway is a
// transparent forwarder for these read surfaces, so the upstream owns the body
// shape and status.
func writeUpstream(w http.ResponseWriter, resp Response) {
	ct := resp.ContentType
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
