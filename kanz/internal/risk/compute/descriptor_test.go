package compute

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/returns/returnstest"
)

type descriptorReturnFixture struct {
	values []float64
	method returns.ReturnMethod
}

func (p descriptorReturnFixture) Returns(context.Context, string, time.Time, int) ([]float64, error) {
	return p.values, nil
}
func (p descriptorReturnFixture) DatedReturns(_ context.Context, id string, at time.Time, _ int) (returns.Series, error) {
	s := returnstest.Series(id, at, p.values)
	s.Method = p.method
	return s, nil
}

func TestDerivedDescriptorsDoNotInventUnavailableMomentum(t *testing.T) {
	for _, method := range []returns.ReturnMethod{returns.ReturnSimple, returns.ReturnLog} {
		rp := descriptorReturnFixture{[]float64{.1, .2, .3}, method}
		p := NewStoreCharacteristicProvider(rp, nil, CharacteristicConfig{Window: 3, MomentumSkip: 3})
		c, ok := p.Characteristics(context.Background(), "A", liveAsOf)
		if !ok || c.SourceDigest == "" || !c.AsOf.Equal(liveAsOf) {
			t.Fatal("missing source identity")
		}
		if _, present := c.Style[StyleMomentum]; present {
			t.Fatal("empty momentum window became observed zero")
		}
		p = NewStoreCharacteristicProvider(rp, nil, CharacteristicConfig{Window: 3, MomentumSkip: 1})
		c, ok = p.Characteristics(context.Background(), "A", liveAsOf)
		if !ok {
			t.Fatal("valid panel rejected")
		}
		want := .32
		if method == returns.ReturnLog {
			want = math.Expm1(.3)
		}
		if math.Abs(c.Style[StyleMomentum]-want) > 1e-14 {
			t.Fatalf("momentum method %v: %v", method, c.Style)
		}
	}
}
