package compute

import (
	decutil "github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"math/big"
)

// ExcludedExposure accumulates exact gross money without rounding an incomplete
// amount into an apparently known total. Its zero value is ready for one query.
type ExcludedExposure struct {
	exact   big.Rat
	amount  *commonpb.Decimal
	unknown bool
}

func (e *ExcludedExposure) Add(value *commonpb.Decimal) {
	amount, valid := decutil.FromProtoChecked(value)
	if !valid {
		e.unknown = true
		return
	}
	e.exact.Add(&e.exact, amount.Abs(amount))
	absolute, fits := decutil.Abs(value)
	if e.amount == nil {
		e.amount = &commonpb.Decimal{}
	}
	if fits {
		e.amount, fits = decutil.Add(e.amount, absolute)
	}
	e.unknown = e.unknown || !fits
}
func (e *ExcludedExposure) Result(cov *Coverage) *commonpb.Decimal {
	if e.amount == nil {
		e.amount = &commonpb.Decimal{}
	}
	if !e.unknown && decutil.FromProto(e.amount).Cmp(&e.exact) == 0 {
		return e.amount
	}
	cov.ExcludeWhole("excluded_exposure_unrepresentable")
	return nil
}
