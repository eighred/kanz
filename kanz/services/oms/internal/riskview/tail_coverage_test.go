package riskview

import (
	"testing"
	"time"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"github.com/eighred/kanz/test/contract/compatibility"
	"google.golang.org/protobuf/proto"
)

func TestUnpricedTailHoldingMakesPreviouslyKnownVaRUnavailable(t *testing.T) {
	unpriced, err := proto.Marshal(compatibility.UnpricedTailVaR())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	view := New(WithClock(func() time.Time { return t0 }))
	covered := &domainpb.InputCoverage{Contributed: 1}
	for _, tc := range []struct {
		label   string
		payload []byte
		known   bool
		usd     int64
	}{
		{"priced", measuresWithCoverage(t, "P1", t0, "VaR99", 500, covered), true, 500},
		{"unpriced", unpriced, false, 0},
		{"priced at zero", measuresWithCoverage(t, "P1", t0, "VaR99", 0, covered), true, 0},
	} {
		if err := view.Handle(ctx, nil, tc.payload); err != nil {
			t.Fatal(err)
		}
		got, ok := view.Measure("P1", "VaR99")
		if ok != tc.known || (ok && (!got.IsInt() || got.Num().Int64() != tc.usd)) {
			t.Errorf("%s: OMS VaR=%v known=%t", tc.label, got, ok)
		}
	}
}
