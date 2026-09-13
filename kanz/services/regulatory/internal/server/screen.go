package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/sustainability"
)

// THE ESG EXCLUSION SCREEN, AND THE CALLER IT NEVER HAD (#751 item 4).
//
// internal/sustainability.Screen has been complete since it was written: it
// takes a book, a classifier and an exclusion policy, and runs them through the
// SHARED COMP-01 engine so an ESG exclusion and a mandate restriction can never
// disagree. It was dark for one reason — nothing called it. This service
// imported the package for FileTCFD and FileSFDR only, mounted no screening
// route, and no other service imported it at all, so an ESG exclusion policy had
// nowhere to be submitted.
//
// # The caller supplies the book, and that is this service's shape rather than a
// shortcut
//
// Every other route here is a stateless calculator over request-supplied inputs:
// the FRTB, Form PF, AIFMD, TCFD and SFDR filings all arrive with their numbers
// in the body. Screen's own doc points the same way — "a caller already holding
// a portfolio snapshot passes it straight through". This service holds no
// portfolio store and reads no principal header, so fetching a book would mean
// inventing both a store and a tenant boundary for it.
//
// NOTE WHAT THIS IS NOT. #751 item 3 criticises services/performance for taking
// already-BUCKETED sector data, which makes the client the classifier. This is
// the opposite: the caller supplies raw holdings and the SERVER classifies them,
// through the reference-data cache the composition root wires. The judgement
// stays here.

// screeningPosition is one holding, with its numbers as decimal STRINGS.
//
// Strings rather than JSON numbers because these are money and quantity, and
// CLAUDE.md is explicit that neither is ever a float. ParseProtoExact preserves
// every digit or refuses before classification; rounding could erase a small
// excluded holding and wrapping could fabricate a large holding's value.
type screeningPosition struct {
	InstrumentID string `json:"instrument_id"`
	Quantity     string `json:"quantity"`
	// MarketValue may be omitted, and omitting it is NOT the same as zero. The
	// engine refuses a book it cannot price rather than treating the holding as
	// worth nothing (#760), so a caller that leaves this out gets a named refusal
	// instead of a clean PASS over a position nobody valued.
	MarketValue string `json:"market_value"`
	Currency    string `json:"currency"`
}

// screeningRequest is an ESG screen: the book to screen and the policy to screen
// it against.
type screeningRequest struct {
	AsOf         *time.Time          `json:"as_of"`
	PortfolioID  string              `json:"portfolio_id"`
	BaseCurrency string              `json:"base_currency"`
	Positions    []screeningPosition `json:"positions"`
	// ExcludedSectors / ExcludedIssuers carry explicit json tags because
	// sustainability.ExclusionPolicy has none, and encoding/json would not match
	// "excluded_sectors" to its ExcludedSectors field — the policy would arrive
	// EMPTY and the screen would pass every book. A silent empty policy is the
	// worst possible failure for a control whose whole output is "you hold
	// something you may not".
	ExcludedSectors []string `json:"excluded_sectors"`
	ExcludedIssuers []string `json:"excluded_issuers"`
}

func (r screeningRequest) policy() sustainability.ExclusionPolicy {
	return sustainability.ExclusionPolicy{
		ExcludedSectors: r.ExcludedSectors,
		ExcludedIssuers: r.ExcludedIssuers,
	}
}

// book converts the request into the COMP-01 shape, refusing anything it cannot
// read exactly.
//
// AN UNPARSEABLE DECIMAL IS AN ERROR, NEVER A ZERO. The alternative — skip it,
// or substitute 0 — is the confident zero this estate has filed six issues
// about, and here it would silently shrink the book a compliance screen runs
// over. An omitted market value is passed through as ABSENT and refused
// downstream by the engine (#760), which is a different thing from a value the
// caller sent and this code failed to read.
func (r screeningRequest) book() (*compliance.Book, error) {
	b := &compliance.Book{PortfolioID: r.PortfolioID, BaseCurrency: r.BaseCurrency}
	for i, p := range r.Positions {
		if p.InstrumentID == "" {
			return nil, fmt.Errorf("positions[%d]: instrument_id is required", i)
		}
		pos := compliance.Position{InstrumentID: p.InstrumentID}
		if p.Quantity != "" {
			q, ok := dec.ParseProtoExact(p.Quantity)
			if !ok {
				return nil, fmt.Errorf("positions[%d].quantity: exact representable decimal required", i)
			}
			pos.Quantity = q
		}
		if p.MarketValue != "" {
			mv, ok := dec.ParseProtoExact(p.MarketValue)
			if !ok {
				return nil, fmt.Errorf("positions[%d].market_value: exact representable decimal required", i)
			}
			ccy := p.Currency
			if ccy == "" {
				ccy = r.BaseCurrency
			}
			pos.MarketValue = &commonpb.Money{Amount: mv, CurrencyCode: ccy}
		}
		b.Positions = append(b.Positions, pos)
	}
	return b, nil
}

// handleESGScreen evaluates a book against an ESG exclusion policy.
//
// IT RETURNS THE COMPLIANCE RESULT AS-IS, including a refusal. The engine
// distinguishes "you hold an excluded sector" from "this could not be checked" —
// an unresolvable dimension when no classifier is wired (#640), or a holding the
// book could not price (#760) — and collapsing those into a boolean here would
// throw away the only part an operator can act on.
func (s *Server) handleESGScreen(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req screeningRequest
	if !decode(w, r, &req) {
		return
	}
	if req.PortfolioID == "" {
		badRequest(w, fmt.Errorf("portfolio_id is required"))
		return
	}
	versioned := r.Pattern == "POST /v2/screening/esg"
	if versioned && (req.AsOf == nil || req.AsOf.IsZero() || req.BaseCurrency == "" || req.policy().Empty()) {
		badRequest(w, fmt.Errorf("as_of, base_currency and a nonempty exclusion policy are required"))
		return
	}
	if len(req.Positions) > 4096 || len(req.ExcludedSectors) > 256 || len(req.ExcludedIssuers) > 256 {
		badRequest(w, fmt.Errorf("screening input exceeds supported bounds"))
		return
	}
	for _, list := range [][]string{req.ExcludedSectors, req.ExcludedIssuers} {
		for _, value := range list {
			if strings.TrimSpace(value) == "" || len(value) > 256 {
				badRequest(w, fmt.Errorf("exclusion identifiers must be nonempty and at most 256 bytes"))
				return
			}
		}
	}
	book, err := req.book()
	if err != nil {
		badRequest(w, err)
		return
	}
	result := sustainability.Screen(r.Context(), book, s.classifier, req.policy(), asOf(req.AsOf))
	if versioned {
		// A separate route refuses old deployments. Proto JSON preserves int64
		// fields as strings and timestamps as RFC3339 for browser consumers.
		data, err := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(result)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "screening result is unavailable"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
