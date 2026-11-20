package server

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

type historicalCustodyReader interface {
	HistoricalReceipt(context.Context, custody.Subject, string, string) (custody.DatedResult, error)
}

type datedCustodyReader interface {
	DatedResult(context.Context, custody.Subject) (custody.DatedResult, error)
}

func (s *Server) datedCustodyResult(reader datedCustodyReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		portfolio := r.PathValue("id")
		principal, ok := s.portfolioReader(w, r, portfolio)
		if !ok {
			return
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		ids, historical := query["run_id"]
		if err != nil || len(query) != 2 && !historical || historical && (len(query) != 3 || len(ids) != 1 || !custody.ValidReceiptID(ids[0])) || len(query["business_date"]) != 1 || len(query["custodian_id"]) != 1 {
			writeJSON(w, 400, map[string]string{"error": "one custodian and business date required"})
			return
		}
		date, err := time.Parse(time.DateOnly, query.Get("business_date"))
		if err != nil || date.IsZero() || !date.Before(custody.BusinessDay(time.Now())) {
			writeJSON(w, 400, map[string]string{"error": "a completed UTC business date is required"})
			return
		}
		custodian := query.Get("custodian_id")
		if !slices.Contains(s.custodyScope.Custodians(portfolio), custodian) {
			writeJSON(w, 404, map[string]string{"error": notFoundBody})
			return
		}
		subject := custody.Subject{PortfolioID: portfolio, CustodianID: custodian, BusinessDate: date}
		ctx := bus.WithTenantID(r.Context(), principal.Tenant)
		configured, _ := s.custodyScope.For(portfolio, custodian)
		result := custody.DatedResult{State: "unknown"}
		if historical {
			if prior, supported := reader.(historicalCustodyReader); supported && len(configured) == 1 {
				for account := range configured {
					result, err = prior.HistoricalReceipt(ctx, subject, account, ids[0])
				}
			}
		} else {
			result, err = reader.DatedResult(ctx, subject)
		}
		if err != nil {
			writeJSON(w, 503, map[string]string{"error": "dated comparison unavailable"})
			return
		}
		// An operator may have reassigned this custodian since the run. The
		// customer report must not describe that old account as today's basis.
		if result.State == "recorded" && (result.Run.Outcome == custody.OutcomeClean || result.Run.Outcome == custody.OutcomeBreaks) {
			basis := result.Run.Basis
			if basis == nil || basis.Mode != custody.BasisDeclared || len(basis.Accounts[custodian]) != 1 || len(configured) != 1 || !configured.Has(basis.Accounts[custodian][0]) {
				result = custody.DatedResult{State: "unknown"}
			}
		}
		response := map[string]any{"tenant_id": principal.Tenant, "state": result.State, "portfolio_id": portfolio, "custodian_id": custodian, "business_date": date.Format(time.DateOnly)}
		if result.Reason != "" {
			response["reason"] = result.Reason
		}
		if result.StatementID != "" {
			response["statement_id"] = result.StatementID
		}
		if result.State == "recorded" {
			response["run_id"] = result.Run.RunID
			response["outcome"] = result.Run.Outcome.String()
			response["completed_at"] = result.Run.CompletedAt.UTC().Format(time.RFC3339Nano)
			if result.Run.Outcome == custody.OutcomeClean || result.Run.Outcome == custody.OutcomeBreaks {
				response["transaction_pass"] = "unknown"
				if proof := result.Run.Basis.Transactions; proof != nil {
					response["transaction_pass"] = proof.Pass
					if proof.StatementReferences != nil && proof.BookReferences != nil && proof.UnreferencedBookTrades != nil {
						response["statement_reference_count"] = strconv.Itoa(*proof.StatementReferences)
						response["book_reference_count"] = strconv.Itoa(*proof.BookReferences)
						response["unreferenced_book_trades"] = strconv.Itoa(*proof.UnreferencedBookTrades)
					}
				}
			}
			if historical {
				response["signed_statement_sha256"] = result.SignedStatementSHA256
				rows := make([]map[string]string, 0, len(result.Run.Breaks))
				for _, b := range result.Run.Breaks {
					v := toBreakView(b, result.Run.CompletedAt)
					rows = append(rows, map[string]string{"break_id": v.BreakID, "kind": v.Kind, "key": v.Key,
						"values_state": v.ValuesState, "ibor": v.IBOR, "custodian": v.Custodian, "difference": v.Difference})
				}
				response["breaks"] = rows
			}
			if b := result.Run.Basis; b != nil {
				response["scope_mode"] = b.Mode
				response["journal_position"] = strconv.FormatInt(b.JournalPosition, 10)
			}
		}
		writeJSON(w, 200, response)
	}
}
