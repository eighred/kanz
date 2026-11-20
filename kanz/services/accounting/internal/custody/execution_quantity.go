package custody

import (
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func quantityEvidence(q *recon.QuantityAssessment) (*accountingpb.ExecutionQuantityComparison, error) {
	if q == nil {
		return nil, nil
	}
	// A signed batch is bounded before parsing. A comparison cannot measure
	// more matched rows than the source contains.
	if q.Compared+q.Unassessed > BatchLimit {
		return nil, ErrUnverifiedPrecision
	}
	out := &accountingpb.ExecutionQuantityComparison{Compared: uint32(q.Compared), Unassessed: uint32(q.Unassessed)}
	for _, d := range q.Differences {
		book, iborOK := exactProto(d.Book)
		signed, signedOK := exactProto(d.Statement)
		diff, diffOK := exactProto(d.Difference)
		if !iborOK || !signedOK || !diffOK {
			return nil, ErrUnverifiedPrecision
		}
		out.Differences = append(out.Differences, &accountingpb.ExecutionQuantityDifference{ExternalRef: d.ExternalRef, InstrumentId: d.InstrumentID, Ibor: book, Custodian: signed, Difference: diff})
	}
	return out, nil
}

// Historical evidence remains absent on legacy completions. Present figures
// must restate the exact signed row and preserve the original book-minus-source sign.
func quantityEvidenceMatches(run Run, stmt Statement, basis *TransactionComparison) bool {
	q := run.ExecutionQuantities
	if q == nil {
		return true
	}
	measured := uint64(q.Compared) + uint64(q.Unassessed)
	if stmt.Grain != recon.GrainTransactions || basis == nil || basis.Pass != recon.LegRan.String() || basis.BookReferences == nil || *basis.BookReferences < 0 || measured > uint64(*basis.BookReferences) || uint64(len(q.Differences)) > uint64(q.Compared) {
		return false
	}
	missing := make(map[string]bool)
	for _, b := range run.Breaks {
		if b.Kind == recon.BreakExecutionMissingInIBOR {
			missing[b.Key] = true
		}
	}
	if len(missing) > len(stmt.Transactions) || measured != uint64(len(stmt.Transactions)-len(missing)) {
		return false
	}
	attested := make(map[string]recon.Transaction)
	asserted := false
	for _, row := range stmt.Transactions {
		asserted = asserted || row.QuantityInInstrumentUnits
		if row.QuantityInInstrumentUnits && !missing[row.ExternalRef] && row.Quantity != nil && row.InstrumentID != "" {
			attested[row.ExternalRef] = row
		}
	}
	if !asserted || uint64(q.Compared) > uint64(len(attested)) {
		return false
	}
	prior := ""
	for _, d := range q.Differences {
		row, ok := attested[d.GetExternalRef()]
		if !ok || d.GetExternalRef() <= prior || d.GetInstrumentId() == "" || d.GetInstrumentId() != row.InstrumentID || row.Quantity == nil || d.GetCustodian() == nil || d.GetIbor() == nil || d.GetDifference() == nil {
			return false
		}
		book, bok := dec.FromProtoChecked(d.Ibor)
		signed, sok := dec.FromProtoChecked(d.Custodian)
		diff, dok := dec.FromProtoChecked(d.Difference)
		if !bok || !sok || !dok || signed.Cmp(row.Quantity) != 0 || diff.Sign() == 0 || new(big.Rat).Sub(book, signed).Cmp(diff) != 0 {
			return false
		}
		prior = d.ExternalRef
	}
	return true
}
