package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// maxBodyBytes bounds the scenario request body — a generous ceiling for a
// shock list, small enough to refuse an abusive payload.
const maxBodyBytes = 1 << 20 // 1 MiB

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
