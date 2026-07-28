package liquidity

import (
	"context"

	"github.com/eighred/kanz/internal/risk/domain"
)

// Liquidity-adjusted VaR (LIQ-01c). LVaR adds the cost of orderly liquidation to
// the price-risk VaR: a position is not worth its mark if realizing it moves the
// market and crosses a spread. The exogenous-spread add-on (Bangia et al.),
// widened here by the liquidation horizon (LiquidationCost), is the standard
// closed-form adjustment:
//
//	LVaR = VaR + Σ |MarketValue_i| × costFraction_i
//
// Because the cost is non-negative, LVaR ≥ VaR always — the LIQ-01e invariant.
// VaR itself stays the RISK-07 / MODEL-01d measure; LVaR only ever adds to it.

// LiquidityAdjustedVaR widens varValue by p's liquidation cost. varValue is the
// unadjusted VaR (a money loss in the base currency); the return is ≥ varValue.
func (m Model) LiquidityAdjustedVaR(ctx context.Context, varValue float64, p *domain.Portfolio, provider Provider) float64 {
	return varValue + m.LiquidationCost(ctx, p, provider)
}
