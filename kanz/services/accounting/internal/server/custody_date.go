package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"google.golang.org/protobuf/encoding/protojson"
)

type historicalCustodyReader interface {
	HistoricalReceipt(context.Context, custody.Subject, string, string) (custody.DatedResult, error)
}

type previousCustodyReader interface {
	PreviousComparison(context.Context, custody.Subject, string, string) (custody.DatedResult, error)
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
		delete(query, "run_id")
		includes, requested := query["include"]
		delete(query, "include")
		include := ""
		if len(includes) == 1 {
			include = includes[0]
		}
		validInclude := !requested || historical && (include == "reported_execution_figures" || include == "previous_comparison")
		validID := !historical || len(ids) == 1 && custody.ValidReceiptID(ids[0])
		if err != nil || !validID || !validInclude || len(query) != 2 || len(query["business_date"]) != 1 || len(query["custodian_id"]) != 1 {
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
		if include == "previous_comparison" {
			if prior, supported := reader.(previousCustodyReader); supported && len(configured) == 1 {
				for account := range configured {
					result, err = prior.PreviousComparison(ctx, subject, account, ids[0])
				}
			}
		} else if historical {
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
		if result.HistoricalQuantityCorrectionRunID != "" || (result.State == "recorded" && (result.Run.Outcome == custody.OutcomeHistoricalQuantityCorrection || result.Run.Outcome == custody.OutcomeClean || result.Run.Outcome == custody.OutcomeBreaks)) {
			basis := result.Run.Basis
			if basis == nil || basis.Mode != custody.BasisDeclared || len(basis.Accounts[custodian]) != 1 || len(configured) != 1 || !configured.Has(basis.Accounts[custodian][0]) {
				if result.State == "pending" {
					result.HistoricalQuantityCorrectionRunID = ""
				} else {
					result = custody.DatedResult{State: "unknown"}
				}
			}
		}
		response := map[string]any{"tenant_id": principal.Tenant, "state": result.State, "portfolio_id": portfolio, "custodian_id": custodian, "business_date": date.Format(time.DateOnly)}
		if result.HistoricalQuantityCorrectionRunID != "" && result.State == "pending" {
			response["historical_quantity_correction_run_id"] = result.HistoricalQuantityCorrectionRunID
		}
		if result.Reason != "" {
			response["reason"] = result.Reason
		}
		if result.StatementID != "" {
			response["statement_id"] = result.StatementID
		}
		if result.State == "recorded" {
			if result.AccountAssignmentVerified && (result.Run.Outcome == custody.OutcomeHistoricalQuantityCorrection || result.Run.Outcome == custody.OutcomeClean || result.Run.Outcome == custody.OutcomeBreaks) {
				response["account_assignment_verified"] = true
			}
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
				if q := result.Run.ExecutionQuantities; q != nil {
					// Protobuf JSON preserves the signed 64-bit coefficients as decimal strings.
					figures, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(q)
					if err != nil {
						writeJSON(w, 503, map[string]string{"error": "dated comparison unavailable"})
						return
					}
					response["execution_quantities"] = json.RawMessage(figures)
				}
				response["signed_statement_sha256"] = result.SignedStatementSHA256
				rows := make([]map[string]string, 0, len(result.Run.Breaks))
				for _, b := range result.Run.Breaks {
					v := toBreakView(b, result.Run.CompletedAt)
					rows = append(rows, map[string]string{"break_id": v.BreakID, "kind": v.Kind, "key": v.Key,
						"values_state": v.ValuesState, "ibor": v.IBOR, "custodian": v.Custodian, "difference": v.Difference})
				}
				if result.Run.Outcome == custody.OutcomeHistoricalQuantityCorrection {
					var links []json.RawMessage
					for _, link := range result.Run.HistoricalQuantityResolutions {
						raw, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(link)
						if err != nil {
							writeJSON(w, 503, map[string]string{"error": "dated comparison unavailable"})
							return
						}
						links = append(links, raw)
					}
					response["historical_quantity_resolutions"] = links
				} else {
					response["breaks"] = rows
				}
			}
			if b := result.Run.Basis; b != nil {
				response["scope_mode"] = b.Mode
				response["journal_position"] = strconv.FormatInt(b.JournalPosition, 10)
			}
		}
		if include == "previous_comparison" && result.State == "recorded" {
			response["previous_of_run_id"] = ids[0]
			response["previous_of_statement_id"] = result.PreviousOfStatementID
		}
		if include == "reported_execution_figures" && result.State == "recorded" {
			rows, ok := result.ReportedExecutionFigures()
			if !ok {
				writeJSON(w, 422, map[string]string{"error": "signed execution figures unavailable for this receipt"})
				return
			}
			response["reported_execution_figures"] = map[string]any{
				"basis": "signed_custodian_statement", "signed_statement_sha256": result.SignedStatementSHA256, "rows": rows,
			}
			encoded, err := json.Marshal(response)
			if err != nil || len(encoded)+1 > custody.BatchLimit {
				writeJSON(w, 422, map[string]string{"error": "signed execution figures exceed receipt limit"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(append(encoded, '\n'))
			return
		}
		writeJSON(w, 200, response)
	}
}
