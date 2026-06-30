package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
)

// maxBodyBytes bounds a forwarded request body (a copilot question or a small
// read body) — generous enough for any read surface, small enough to refuse an
// abusive payload.
const maxBodyBytes = 1 << 20 // 1 MiB

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
