package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

// SectorHistoryHandler is mounted only on the private Accounting listener.
func SectorHistoryHandler(tenant string, history store.SectorHistory) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+refdata.SectorHistoryPath, func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireCallerTenantIs(w, r, tenant, notFoundBody) {
			return
		}
		if r.Header.Get(auth.HeaderPrincipalSubject) != "accounting" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		date := q.Get("effective_date")
		_, dateErr := time.Parse(time.DateOnly, date)
		asOf, timeErr := time.Parse(time.RFC3339Nano, q.Get("as_of"))
		ids := q["instrument_id"]
		if dateErr != nil || timeErr != nil || asOf.IsZero() || asOf.Location() != time.UTC ||
			asOf.Nanosecond()%1000 != 0 || asOf.After(time.Now()) || len(ids) == 0 || len(ids) > refdata.SectorHistoryLimit {
			http.Error(w, "invalid dated sector query", http.StatusBadRequest)
			return
		}
		for _, id := range ids {
			if id == "" || len(id) > 128 || strings.TrimSpace(id) != id {
				http.Error(w, "invalid instrument identity", http.StatusBadRequest)
				return
			}
		}
		rows, err := history.SectorsAt(r.Context(), date, asOf, ids)
		if err != nil {
			http.Error(w, "dated sector store unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})
	return mux
}
