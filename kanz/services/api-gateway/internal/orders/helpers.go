package orders

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// errWritesDisabled is returned when no publisher is wired (read-only gateway).
var errWritesDisabled = errors.New("orders: write surface disabled (no bus producer configured)")

// writePublishError maps a publish failure to a client status: a forged/missing
// issuer is a 403 (the caller may not issue under that identity); writes
// disabled is 503; anything else is a 502 (upstream bus fault).
func writePublishError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errWritesDisabled):
		writeError(w, http.StatusServiceUnavailable, "order writes are disabled")
	case errors.Is(err, auth.ErrForgedIssuer), errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, bus.ErrMissingCommandIssuer):
		writeError(w, http.StatusForbidden, "not authorized to issue this command")
	default:
		writeError(w, http.StatusBadGateway, "failed to publish order command")
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
