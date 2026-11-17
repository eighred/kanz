package domain

import (
	"fmt"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestObservedPositionCountIncludesFlatsWithoutMaterializingRows(t *testing.T) {
	p := NewPortfolio("P", "USD")
	for i := range 1025 {
		p.SetPosition(Position{InstrumentID: InstrumentID(fmt.Sprintf("I%04d", i)), Quantity: &commonpb.Decimal{}})
	}
	if p.PositionCount() != 0 {
		t.Fatal("fixture did not preserve the source-reported non-flat count")
	}
	if p.sortedCache != nil {
		t.Fatal("fixture already materialized the observed rows")
	}
	if p.ObservedPositionCount() != 1025 {
		t.Fatal("actual observed cardinality dropped retained flat rows")
	}
	if p.sortedCache != nil {
		t.Fatal("cardinality lookup materialized/sorted the observed rows")
	}
}
