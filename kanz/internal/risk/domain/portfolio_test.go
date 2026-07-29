package domain_test

import (
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/domain"
)

// These lock the LATENCY-01c Positions() memoization contract: the cache must
// be invalidated by every mutation, so a read never returns stale state. The
// engine relies on this — it reads, then the same goroutine may apply more
// state to a snapshot before reading again (e.g. bootstrap replay).

func mv(coef int64) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: coef}, CurrencyCode: "USD"}
}

func pos(instrument string, coef int64) domain.Position {
	return domain.Position{InstrumentID: domain.InstrumentID(instrument), MarketValue: mv(coef), AsOf: time.Unix(0, 0)}
}

func keys(positions []domain.Position) []string {
	out := make([]string, len(positions))
	for i, p := range positions {
		out[i] = string(p.InstrumentID)
	}
	return out
}

func TestPositions_SortedOrder(t *testing.T) {
	p := domain.NewPortfolio("P", "USD")
	p.SetPosition(pos("C", 1))
	p.SetPosition(pos("A", 1))
	p.SetPosition(pos("B", 1))
	got := keys(p.Positions())
	want := []string{"A", "B", "C"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("order=%v want %v", got, want)
	}
}

func TestPositions_SetInvalidatesAfterRead(t *testing.T) {
	p := domain.NewPortfolio("P", "USD")
	p.SetPosition(pos("A", 1))
	_ = p.Positions() // warm the cache
	p.SetPosition(pos("B", 1))
	if got := len(p.Positions()); got != 2 {
		t.Errorf("after add post-read: len=%d want 2 (stale cache)", got)
	}
}

func TestPositions_UpdateInPlaceReflected(t *testing.T) {
	p := domain.NewPortfolio("P", "USD")
	p.SetPosition(pos("A", 100))
	_ = p.Positions()
	p.SetPosition(pos("A", 250)) // same instrument, new mark
	got := p.Positions()
	if len(got) != 1 || got[0].MarketValue.Amount.Coefficient != 250 {
		t.Errorf("updated mark not reflected: %+v", got)
	}
}

func TestPositions_ForgetInvalidates(t *testing.T) {
	p := domain.NewPortfolio("P", "USD")
	p.SetPosition(pos("A", 1))
	p.SetPosition(pos("B", 1))
	_ = p.Positions()
	p.Forget("A")
	got := keys(p.Positions())
	if len(got) != 1 || got[0] != "B" {
		t.Errorf("after Forget: %v want [B]", got)
	}
}

func TestPositions_ClearInvalidates(t *testing.T) {
	p := domain.NewPortfolio("P", "USD")
	p.SetPosition(pos("A", 1))
	_ = p.Positions()
	p.ClearPositions()
	if got := len(p.Positions()); got != 0 {
		t.Errorf("after ClearPositions: len=%d want 0", got)
	}
}
