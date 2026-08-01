package gateway

import (
	"encoding/json"
	"strings"
)

// detailOf pulls the gateway's own "error" field out of a failure body, falling
// back to the trimmed raw payload when there is none.
//
// Separate from the transport so that Do reads as the request it performs, with
// the error-shape parsing out of the way of it.
func detailOf(payload []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(payload, &body)
	if d := strings.TrimSpace(body.Error); d != "" {
		return d
	}
	return strings.TrimSpace(string(payload))
}
