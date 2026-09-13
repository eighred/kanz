package optimization

import (
	"github.com/eighred/kanz/internal/dec"
	"testing"
	"time"
)

func TestExactRebalancePreservesFractionalQuantityWithoutApproval(t *testing.T) {
	p, err := RebalanceExact("test", map[string]dec.Exact{"x": "0"}, map[string]dec.Exact{"x": "1/3"}, "1", map[string]dec.Exact{"x": "3"}, "0", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Trades) != 1 || p.Trades[0].Notional != "1/3" || p.Trades[0].Quantity != "1/9" || p.Turnover != "1/6" || p.MandateStatus != MandateUnchecked {
		t.Fatal(p)
	}
}
