package recon

import (
	"errors"
	"math/big"
	"sort"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// QuantityAssessment covers matched references only. A missing ledger quantity,
// source unit assertion, or common instrument leaves that reference unassessed.
type QuantityAssessment struct {
	Compared, Unassessed int
	Differences          []ExecutionQuantityDifference
	// Measured includes explicit equalities. An omitted reference never closes a working discrepancy.
	Measured []ExecutionQuantityDifference
}

type ExecutionQuantityDifference struct {
	ExternalRef, InstrumentID   string
	AccountID, EntryID          string
	Book, Statement, Difference *big.Rat
}

// AssessExecutionQuantities compares exact signed quantities in one canonical
// instrument's natural units. Membership, cash, fees and price have other scopes.
func AssessExecutionQuantities(executions []ledger.Execution, stmt Statement) (*QuantityAssessment, error) {
	if stmt.Grain != GrainTransactions || stmt.BusinessDate.IsZero() {
		return nil, nil
	}
	requested := false
	for _, line := range stmt.Transactions {
		if line.QuantityInInstrumentUnits {
			requested = true
			break
		}
	}
	if !requested {
		return nil, nil
	}
	book, err := datedExecutions(executions, businessDay(stmt.BusinessDate))
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(stmt.Transactions))
	result := new(QuantityAssessment)
	for _, line := range stmt.Transactions {
		if line.ExternalRef == "" || seen[line.ExternalRef] {
			return nil, errors.New("recon: blank or repeated source execution reference")
		}
		seen[line.ExternalRef] = true
		entry, matched := book[line.ExternalRef]
		if !matched {
			continue
		}
		if !line.QuantityInInstrumentUnits || line.Quantity == nil || entry.Quantity == nil || line.InstrumentID == "" || line.InstrumentID != entry.InstrumentID {
			result.Unassessed++
			continue
		}
		result.Compared++
		measurement := ExecutionQuantityDifference{ExternalRef: line.ExternalRef, InstrumentID: line.InstrumentID,
			AccountID: entry.VenueAccountID, EntryID: entry.EntryID, Book: new(big.Rat).Set(entry.Quantity),
			Statement: new(big.Rat).Set(line.Quantity), Difference: new(big.Rat).Sub(entry.Quantity, line.Quantity)}
		result.Measured = append(result.Measured, measurement)
		if measurement.Difference.Sign() != 0 {
			result.Differences = append(result.Differences, measurement)
		}
	}
	sort.Slice(result.Differences, func(i, j int) bool { return result.Differences[i].ExternalRef < result.Differences[j].ExternalRef })
	return result, nil
}
