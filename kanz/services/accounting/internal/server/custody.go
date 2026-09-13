package server

// The custody reconciliation break queue — the operator surface for the
// lifecycle #962 added.
//
// WHY THIS EXISTS AT ALL, AND WHY SHIPPING WITHOUT IT WOULD HAVE BEEN A DEFECT.
// custody.Break.Assign/Explain are the half of the lifecycle a PERSON
// drives; the automatic half (detect ⇒ OPEN, agreement ⇒ RESOLVED) runs on every
// scheduled run and needs nobody. Without a route, the manual half would be
// complete, unit-tested, architecturally sound and CALLED BY NOTHING — the "dark
// capability" pattern this codebase has nine instances of, and the one that makes
// a control look finished while an operator cannot actually work the queue.
//
// A GET AND TWO TRANSITIONS, NOT A CRUD SURFACE — and there is deliberately no
// resolve among them. A break is DETECTED by comparing the book against a
// custodian statement and is RESOLVED by those two agreeing again, which only a
// run can establish; Store.UpsertBreaks does it automatically and is the single
// path into the terminal state. An operator may record what they know about a
// break (assign it, explain it) and may not invent, erase or close one — which is
// why ApplyAction refuses unknown breaks and custody.Break has no Resolve method.
// See the note in custody/lifecycle.go for why a "resolve if the sides agree"
// route cannot be made safe without re-running the comparison.

import (
	"context"
	"net/http"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/custody"
)

// BreakStore is the read/write surface the operator routes need. It is the
// subset of custody.Store this package uses, declared here so the server depends
// on the two operations it calls rather than on the whole store.
type BreakStore interface {
	OutstandingBreaks(ctx context.Context) ([]custody.Break, error)
	ApplyAction(ctx context.Context, action custody.Action) (custody.ActionEvidence, error)
	ActionsEnabled() bool
}

// WithBreakStore mounts the custody break queue. Without it the routes are not
// registered at all.
//
// UNREGISTERED IS A 404, AND THAT IS THE HONEST ANSWER. A route mounted over a
// nil store would answer 500 to every caller, which reads as "the queue is
// broken" when the truth is "this deployment has no custody reconciliation". The
// same stance the cash-movement route takes one file over.
func WithBreakStore(store BreakStore) Option {
	return func(s *Server) { s.breaks = store }
}

func (s *Server) custodyRoutes() {
	if s.breaks == nil {
		return
	}
	s.mux.HandleFunc("GET /v1/custody/breaks", s.handleListBreaks)
	s.mux.HandleFunc("POST /v1/custody/breaks/{id}/assign", s.handleAssignBreak)
	s.mux.HandleFunc("POST /v1/custody/breaks/{id}/explain", s.handleExplainBreak)
	s.mux.HandleFunc("POST /v1/custody/breaks/{id}/actions", s.handleCustodyAction)
}

// breakView is one break as an operator sees it. Figures are rendered as exact
// decimal TEXT, never as JSON numbers: encoding/json emits a number as float64,
// and a quantity beyond float's exact range would be shown to the person deciding
// what to do about it as a different number from the one the book holds.
type breakView struct {
	BreakID         string `json:"break_id"`
	Kind            string `json:"kind"`
	Key             string `json:"key"`
	IBOR            string `json:"ibor"`
	Custodian       string `json:"custodian"`
	Difference      string `json:"difference"`
	Status          string `json:"status"`
	Assignee        string `json:"assignee,omitempty"`
	Explanation     string `json:"explanation,omitempty"`
	FirstSeenAt     string `json:"first_seen_at"`
	LastSeenAt      string `json:"last_seen_at"`
	StatusChangedAt string `json:"status_changed_at"`
	AgeSeconds      int64  `json:"age_seconds"`
	Revision        int64  `json:"revision,string"`
	ValuesState     string `json:"values_state"`
}

func toBreakView(b custody.Break, now time.Time) breakView {
	ibor, iok := custody.ExactDecimalText(b.IBOR)
	custodian, cok := custody.ExactDecimalText(b.Custodian)
	difference, dok := custody.ExactDecimalText(b.Diff)
	state := "exact"
	if !b.ValuesVerified {
		state = "legacy_unverified"
	} else if !iok || !cok || !dok {
		state = "unavailable"
	}
	if state != "exact" {
		ibor, custodian, difference = "", "", ""
	}
	return breakView{
		ValuesState: state,
		BreakID:     b.BreakID,
		Revision:    b.Revision,
		Kind:        b.Kind.String(),
		Key:         b.Key,
		IBOR:        ibor,
		Custodian:   custodian,
		Difference:  difference,
		Status:      b.Status.String(),
		Assignee:    b.Assignee,
		Explanation: b.Explanation,
		FirstSeenAt: b.FirstSeenAt.UTC().Format(time.RFC3339),
		LastSeenAt:  b.LastSeenAt.UTC().Format(time.RFC3339),
		// AGE IS SERVED RATHER THAN LEFT TO THE CALLER. It is measured from
		// FirstSeenAt, which is never advanced by a redetection, and it is the
		// number the queue is triaged by — a client computing it from the
		// timestamps would be a second implementation of the one figure that
		// decides which break gets worked first.
		StatusChangedAt: b.StatusChangedAt.UTC().Format(time.RFC3339),
		AgeSeconds:      int64(b.Age(now).Seconds()),
	}
}

func (s *Server) handleListBreaks(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	breaks, err := s.breaks.OutstandingBreaks(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "custody values could not be verified"})
		return
	}
	now := time.Now().UTC()
	w.Header().Set("Cache-Control", "no-store")
	out := make([]breakView, 0, len(breaks))
	for _, b := range breaks {
		out = append(out, toBreakView(b, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"breaks": out, "count": len(out), "actions_enabled": s.breaks.ActionsEnabled()})
}
