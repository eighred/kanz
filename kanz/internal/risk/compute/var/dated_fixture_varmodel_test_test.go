package varmodel_test

import (
	"context"
	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/returns/returnstest"
	"time"
)

func (p fixedProvider) DatedReturns(ctx context.Context, id string, asOf time.Time, window int) (returns.Series, error) {
	values, err := p.Returns(ctx, id, asOf, window)
	return returnstest.Series(id, asOf, values), err
}
func (p mapProvider) DatedReturns(ctx context.Context, id string, asOf time.Time, window int) (returns.Series, error) {
	values, err := p.Returns(ctx, id, asOf, window)
	return returnstest.Series(id, asOf, values), err
}
func (p noReturns) DatedReturns(ctx context.Context, id string, asOf time.Time, window int) (returns.Series, error) {
	values, err := p.Returns(ctx, id, asOf, window)
	return returnstest.Series(id, asOf, values), err
}
func (p oneKnownInstrument) DatedReturns(ctx context.Context, id string, asOf time.Time, window int) (returns.Series, error) {
	values, err := p.Returns(ctx, id, asOf, window)
	return returnstest.Series(id, asOf, values), err
}
func (p *sliceProvider) DatedReturns(ctx context.Context, id string, asOf time.Time, window int) (returns.Series, error) {
	values, err := p.Returns(ctx, id, asOf, window)
	return returnstest.Series(id, asOf, values), err
}
