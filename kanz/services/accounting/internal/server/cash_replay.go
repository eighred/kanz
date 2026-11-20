package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type cashHistoryReader interface {
	ReadCashCommits(context.Context, string, string, int64, int64, int) (ledger.CashCommitPage, error)
}

func (s *Server) cashCommitHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.cashPrincipal(w, r); !ok {
		return
	}
	reader, ok := s.store.(cashHistoryReader)
	if !ok {
		http.Error(w, "durable cash history unavailable", http.StatusServiceUnavailable)
		return
	}
	query := r.URL.Query()
	// Ambiguous cursor parameters must not select different pages in a proxy
	// and backend. All bounds are explicit; zero through freezes the source head.
	for _, name := range []string{"currency", "after", "through", "limit"} {
		if len(query[name]) != 1 || query.Get(name) == "" {
			http.Error(w, "explicit cash history bounds required", http.StatusBadRequest)
			return
		}
	}
	after, e1 := strconv.ParseInt(query.Get("after"), 10, 64)
	through, e2 := strconv.ParseInt(query.Get("through"), 10, 64)
	limit, e3 := strconv.Atoi(query.Get("limit"))
	if e1 != nil || e2 != nil || e3 != nil {
		http.Error(w, "invalid cash history bounds", http.StatusBadRequest)
		return
	}
	page, err := reader.ReadCashCommits(r.Context(), r.PathValue("id"), query.Get("currency"), after, through, limit)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, ledger.ErrCashReplayBounds) {
			status = http.StatusBadRequest
		} else if errors.Is(err, ledger.ErrNoCashHistory) {
			status = http.StatusNotFound
		}
		http.Error(w, "cash history unavailable for the requested bounds", status)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
