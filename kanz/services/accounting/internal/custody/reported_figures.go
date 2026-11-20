package custody

import (
	"slices"
	"strings"
)

// ReportedExecution contains source observations only. The signed currency describes
// the cash leg. The source contract does not identify the price's quote currency.
type ReportedExecution struct {
	ExternalRef  string `json:"external_ref"`
	InstrumentID string `json:"instrument_id"`
	Price        string `json:"reported_price,omitempty"`
	Cash         string `json:"reported_cash,omitempty"`
	CashCurrency string `json:"cash_currency"`
}

func (r DatedResult) ReportedExecutionFigures() ([]ReportedExecution, bool) {
	if r.reportedTransactions == nil || len(r.reportedTransactions) > historicalBreakLimit {
		return nil, false
	}
	rows := make([]ReportedExecution, 0, len(r.reportedTransactions))
	for _, tx := range r.reportedTransactions {
		if tx.ExternalRef == "" || !batchID.MatchString(tx.InstrumentID) || !batchID.MatchString(tx.CurrencyCode) {
			return nil, false
		}
		row := ReportedExecution{ExternalRef: tx.ExternalRef, InstrumentID: tx.InstrumentID, CashCurrency: tx.CurrencyCode}
		var ok bool
		if tx.Price != nil {
			if row.Price, ok = ExactDecimalText(tx.Price); !ok {
				return nil, false
			}
		}
		if tx.Cash != nil {
			if row.Cash, ok = ExactDecimalText(tx.Cash); !ok {
				return nil, false
			}
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b ReportedExecution) int { return strings.Compare(a.ExternalRef, b.ExternalRef) })
	return rows, true
}
