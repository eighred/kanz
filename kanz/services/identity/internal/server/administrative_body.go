package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

const administrativeBodyTimeout = 5 * time.Second

// administrativeBody consumes a bounded request before authorization can return.
// With Connection: close, unread bytes can reset the TCP connection and erase
// even a successful response. Never drain an unbounded stream or commit while
// waiting for the rest of a command. Handlers validate the buffered payload
// after authorization and before mutation.
func administrativeBody(next http.HandlerFunc) http.HandlerFunc {
	return boundedAdministrativeBody(next, maxBody)
}

func boundedAdministrativeBody(next http.HandlerFunc, limit int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(time.Now().Add(administrativeBodyTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeErr(w, http.StatusServiceUnavailable, "request body deadline unavailable")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			w.Header().Set("Connection", "close")
			var oversized *http.MaxBytesError
			var timedOut net.Error
			switch {
			case errors.As(err, &oversized):
				writeErr(w, http.StatusRequestEntityTooLarge, "request body exceeds the identity limit")
			case errors.As(err, &timedOut) && timedOut.Timeout():
				writeErr(w, http.StatusRequestTimeout, "request body deadline exceeded")
			default:
				writeErr(w, http.StatusBadRequest, "request body could not be read")
			}
			return
		}
		_ = r.Body.Close()
		_ = controller.SetReadDeadline(time.Time{})
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next(w, r)
	}
}

// emptyAdministrativeBody runs after authorization, preserving opaque refusals
// while enforcing the status-only command contract before any mutation.
func emptyAdministrativeBody(w http.ResponseWriter, r *http.Request) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	var object map[string]json.RawMessage
	err := decoder.Decode(&object)
	if errors.Is(err, io.EOF) {
		return true
	}
	if err == nil && object != nil && len(object) == 0 {
		var extra any
		if errors.Is(decoder.Decode(&extra), io.EOF) {
			return true
		}
	}
	writeErr(w, http.StatusBadRequest, "this command accepts no body or an empty JSON object")
	return false
}
