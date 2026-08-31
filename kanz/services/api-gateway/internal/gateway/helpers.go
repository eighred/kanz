package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/eighred/kanz/services/api-gateway/internal/middleware"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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

// decodeJSON reads a size-limited JSON body and unmarshals it into the proto
// message via protojson (so the REST request shape matches the proto exactly).
// Unknown fields are rejected so a typo'd client field is a 400, not silently
// dropped.
func decodeJSON(r *http.Request, msg proto.Message) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxBodyBytes {
		return errBodyTooLarge
	}
	if len(body) == 0 {
		return nil // empty body ⇒ zero-value request (e.g. no shocks)
	}
	return protojson.UnmarshalOptions{}.Unmarshal(body, msg)
}

// writeError writes a uniform JSON error body. Matches the middleware error
// shape so clients see one error format across the gateway.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
