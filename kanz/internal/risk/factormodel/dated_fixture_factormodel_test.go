package factormodel

import (
	"context"
	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/returns/returnstest"
	"time"
)

func (p matrixReturns) DatedReturns(ctx context.Context, id string, asOf time.Time, window int) (returns.Series, error) {
	values, err := p.Returns(ctx, id, asOf, window)
	return returnstest.Series(id, asOf, values), err
}
func (p identityReturns) DatedReturns(ctx context.Context, id string, asOf time.Time, window int) (returns.Series, error) {
	values, err := p.Returns(ctx, id, asOf, window)
	return returnstest.Series(id, asOf, values), err
}
