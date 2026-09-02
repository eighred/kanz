package server

// The custody reconciliation break queue — the operator surface for the
// lifecycle #962 added.
//
// WHY THIS EXISTS AT ALL, AND WHY SHIPPING WITHOUT IT WOULD HAVE BEEN A DEFECT.
// custody.Break.Assign/Explain/Resolve are the half of the lifecycle a PERSON
// drives; the automatic half (detect ⇒ OPEN, agreement ⇒ RESOLVED) runs on every
// scheduled run and needs nobody. Without a route, the manual half would be
// complete, unit-tested, architecturally sound and CALLED BY NOTHING — the "dark
// capability" pattern this codebase has nine instances of, and the one that makes
// a control look finished while an operator cannot actually work the queue.
//
// A GET AND THREE TRANSITIONS, NOT A CRUD SURFACE. There is deliberately no
// create and no delete: a break is DETECTED by comparing the book against a
// custodian statement and is resolved by those two agreeing again. An operator
// may record what they know about one and may not invent or erase one, which is
// why SaveBreak refuses to insert and why Resolve refuses while the difference is
// still there.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

// BreakStore is the read/write surface the operator routes need. It is the
// subset of custody.Store this package uses, declared here so the server depends
// on the two operations it calls rather than on the whole store.
type BreakStore interface {
	OutstandingBreaks(ctx context.Context) ([]custody.Break, error)
	LoadBreak(ctx context.Context, breakID string) (custody.Break, error)
	SaveBreak(ctx context.Context, b custody.Break) error
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
	s.mux.HandleFunc("POST /v1/custody/breaks/{id}/resolve", s.handleResolveBreak)
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
}

func toBreakView(b custody.Break, now time.Time) breakView {
	return breakView{
		BreakID:     b.BreakID,
		Kind:        b.Kind.String(),
		Key:         b.Key,
		IBOR:        dec.Str(b.IBOR),
		Custodian:   dec.Str(b.Custodian),
		Difference:  dec.Str(b.Diff),
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	out := make([]breakView, 0, len(breaks))
	for _, b := range breaks {
		out = append(out, toBreakView(b, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"breaks": out, "count": len(out)})
}

type breakTransitionRequest struct {
	Assignee    string `json:"assignee"`
	Explanation string `json:"explanation"`
}

// transition loads a break, applies a lifecycle move, and persists it.
//
// THE STORE IS RE-READ RATHER THAN TRUSTED FROM THE REQUEST. An operator's client
// may be showing a break as it was some minutes ago, and the transitions are
// state-dependent — Resolve refuses while the difference is still detected. A
// move applied to a stale copy would be a decision taken against a book that has
// since moved.
func (s *Server) transition(w http.ResponseWriter, r *http.Request, apply func(*custody.Break, time.Time) error) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	id := r.PathValue("id")
	b, err := s.breaks.LoadBreak(r.Context(), id)
	if errors.Is(err, custody.ErrNoBreak) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such break"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := apply(&b, time.Now().UTC()); err != nil {
		// AN ILLEGAL TRANSITION IS A 409, NOT A 400. The request is well-formed
		// and the caller is entitled to make it; the break is simply not in a
		// state that admits it — usually because somebody else moved it first, or
		// because the difference the caller believes is gone is still there. 400
		// would tell them to fix their request, which is the wrong instruction.
		status := http.StatusConflict
		if errors.Is(err, errBadTransitionInput) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	if err := s.breaks.SaveBreak(r.Context(), b); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toBreakView(b, time.Now().UTC()))
}

// errBadTransitionInput marks a refusal caused by the REQUEST rather than by the
// break's state, so the handler can answer 400 instead of 409.
var errBadTransitionInput = errors.New("accounting: invalid break transition request")

func decodeTransition(w http.ResponseWriter, r *http.Request) (breakTransitionRequest, bool) {
	var req breakTransitionRequest
	if r.ContentLength == 0 {
		return req, true
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return req, false
	}
	return req, true
}

func (s *Server) handleAssignBreak(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeTransition(w, r)
	if !ok {
		return
	}
	s.transition(w, r, func(b *custody.Break, now time.Time) error {
		if req.Assignee == "" {
			return errBadTransitionInput
		}
		return b.Assign(req.Assignee, now)
	})
}

func (s *Server) handleExplainBreak(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeTransition(w, r)
	if !ok {
		return
	}
	s.transition(w, r, func(b *custody.Break, now time.Time) error {
		if req.Explanation == "" {
			return errBadTransitionInput
		}
		return b.Explain(req.Explanation, now)
	})
}

// handleResolveBreak resolves a break ONLY if the latest stored state no longer
// shows it outstanding-and-detected.
//
// THE stillDetected ARGUMENT IS READ FROM THE STORE, NEVER FROM THE REQUEST. A
// caller asserting "this is fixed" is the exact thing the check exists to
// prevent: a break resolved while the book and the custodian still disagree is
// the control being silenced by hand, which leaves the number wrong and the queue
// looking clean. The legitimate path is that the cause is corrected, the next run
// stops detecting it, and it is resolved then — which the scheduled run already
// does automatically.
func (s *Server) handleResolveBreak(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, func(b *custody.Break, now time.Time) error {
		outstanding, err := s.breaks.OutstandingBreaks(r.Context())
		if err != nil {
			return err
		}
		stillDetected := false
		for _, o := range outstanding {
			// LastSeenAt equal to the newest run's completion is what "the latest
			// run still finds it" means; the store only keeps outstanding breaks
			// in this set, so presence here is the answer.
			if o.BreakID == b.BreakID && o.LastSeenAt.After(o.StatusChangedAt) {
				stillDetected = true
				break
			}
		}
		return b.Resolve(stillDetected, now)
	})
}
