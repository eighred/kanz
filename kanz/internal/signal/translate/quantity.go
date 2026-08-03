package translate

import "math/big"

// Qty is a RESOLVED base-asset quantity — units of the instrument itself, after
// SizeType has been applied. It is a distinct type rather than a *big.Rat because
// the defect it closes (#240) was a comparison that type-checked and was still
// nonsense.
//
// Intent.Size is a bare number whose UNIT is not decided until resolveQuantity
// applies SizeType: ABSOLUTE_QTY makes it a quantity, QUOTE_NOTIONAL makes it
// quote currency, PCT_OF_EQUITY makes it a percentage. webhook-ingest bounded that
// bare number with a single `max_size` scalar BEFORE the unit was decided, so a
// cap of "10" meaning "10 BTC" admitted {"size":"9","size_type":"pct_of_equity"} —
// 1,800 BTC at a $1e9 NAV and a $50k mark, 180x the cap, submitted as live orders.
//
// So a bound is a Qty, the resolved quantity is a Qty, and the only way to obtain
// one from an Intent is to resolve it. The comparison that shipped —
// max.Cmp(in.Size) — no longer compiles. That is the guard; there is no arch test
// for it because the compiler is a stronger one.
//
// The zero Qty is UNSET, which is how "no bound configured" is spelled. Unset is
// not zero-quantity: Qty{}.Cmp is never consulted, IsSet is.
type Qty struct{ r *big.Rat }

// NewQty wraps an exact rational as a resolved base-asset quantity. A nil
// rational yields the unset Qty, so an absent bound stays absent rather than
// becoming a bound of zero (which would refuse every order).
func NewQty(r *big.Rat) Qty {
	if r == nil {
		return Qty{}
	}
	return Qty{r: new(big.Rat).Set(r)}
}

// IsSet reports whether this Qty carries a value at all.
func (q Qty) IsSet() bool { return q.r != nil }

// Rat returns a COPY of the underlying rational, or nil when unset. A copy
// because big.Rat methods mutate their receiver, and a shared bound that a caller
// scaled in place would silently re-bound every later signal.
func (q Qty) Rat() *big.Rat {
	if q.r == nil {
		return nil
	}
	return new(big.Rat).Set(q.r)
}

// Sign reports the sign of the quantity; the unset Qty is 0.
func (q Qty) Sign() int {
	if q.r == nil {
		return 0
	}
	return q.r.Sign()
}

// Cmp compares two quantities. Both must be set; comparing against an unset Qty
// is a caller error and answers 0, which is why every call site tests IsSet first.
func (q Qty) Cmp(o Qty) int {
	if q.r == nil || o.r == nil {
		return 0
	}
	return q.r.Cmp(o.r)
}

// Scale multiplies by a DIMENSIONLESS factor — a venue allocation weight. The
// result is still a quantity, which is why this returns a Qty and takes a bare
// rational: a weight has no unit to lose.
func (q Qty) Scale(factor *big.Rat) Qty {
	if q.r == nil || factor == nil {
		return Qty{}
	}
	return Qty{r: new(big.Rat).Mul(q.r, factor)}
}

// RatString renders the exact value for an error message; unset renders "unset".
func (q Qty) RatString() string {
	if q.r == nil {
		return "unset"
	}
	return q.r.RatString()
}
