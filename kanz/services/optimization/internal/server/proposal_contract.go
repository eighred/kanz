package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/eighred/kanz/pkg/auth"
)

func (s *Server) handleLegacyPropose(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.PrincipalFromHeaders(r.Header); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authenticated principal required"})
		return
	}
	writeJSON(w, http.StatusGone, map[string]string{"error": "float financial proposals retired; use /v2/propose with exact string financial values, explicit currency and threshold"})
}

func decodeProposalRequest(w http.ResponseWriter, r *http.Request, out *proposeRequest) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	if err := uniqueProposalKeys(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ambiguous proposal JSON"})
		return false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid exact proposal request"})
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected one proposal request"})
		return false
	}
	return true
}

// Case aliases are ambiguous to encoding/json's struct matching. Reject them
// along with duplicate map keys rather than silently choosing a financial value.
func uniqueProposalKeys(d *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("proposal nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil || token == "" {
		return errors.New("unknown proposal value")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid key")
			}
			name = strings.ToLower(name)
			if keys[name] {
				return errors.New("duplicate key")
			}
			keys[name] = true
		}
		if err := uniqueProposalKeys(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
