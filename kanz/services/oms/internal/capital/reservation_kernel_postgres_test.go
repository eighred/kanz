package capital

import (
	"context"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/jackc/pgx/v5"
)

func TestPostgresReservationExactMatchesGo(t *testing.T) {
	pool := database(t)
	bound := new(big.Int).Lsh(big.NewInt(1), 512)
	maxComponent := new(big.Int).Sub(new(big.Int).Set(bound), big.NewInt(1)).String()
	// dec.Exact validates the reduced rational. Large unreduced inputs,
	// old decimal strings, and leading-zero fraction numerators are retained inputs.
	values := []string{"", "-0", "0/1", "-0.00", "1/3", "9/21", "1/01", "1/0", "1/-2",
		"008", "008.50", "0010/3", "-0010/3", "000/3", "008/3", ".5", "1.", "1e3", "1\n", " 1", "NaN", "∞",
		maxComponent, "-" + maxComponent, bound.String(), "1/" + maxComponent, "1/" + bound.String(),
		bound.String() + "/" + bound.String(), strings.Repeat("0", 400), strings.Repeat("0", 401),
		"0." + strings.Repeat("0", 152) + "1", "0." + strings.Repeat("0", 154) + "1",
	}
	for _, input := range values {
		var num, den *string
		err := pool.QueryRow(t.Context(), `SELECT parts[1]::text, parts[2]::text FROM (SELECT capital_exact_parts($1) AS parts) p`, input).Scan(&num, &den)
		if err != nil {
			t.Fatal(err)
		}
		want, err := dec.Exact(input).Rat()
		if err != nil {
			if num != nil || den != nil {
				t.Errorf("SQL accepted %q", input)
			}
			continue
		}
		if num == nil || den == nil || *num != want.Num().String() || *den != want.Denom().String() {
			t.Errorf("SQL rational disagrees with Go for %q, want %s", input, want.RatString())
		}
	}
}

func TestPostgresReserveKernelRetainedAmountsAndRollback(t *testing.T) {
	p := database(t)
	ctx := t.Context()
	opening := snapshot(1, "20")
	if err := Apply(ctx, p, opening); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE capital_balances SET total='0020.000',reserved='0.2500'`); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var verdict string
	if err = tx.QueryRow(ctx, `SELECT capital_reserve('fund','USD','test','0010/24',$1,$2,$3)`,
		opening.ObservedAt.Unix(), int32(opening.ObservedAt.Nanosecond()), int64(time.Minute)).Scan(&verdict); err != nil || verdict != "RESERVED" {
		t.Fatalf("direct reservation=%q err=%v", verdict, err)
	}
	for _, tc := range []struct {
		id, debit string
		want      error
	}{
		{"test", "1/3", ErrConflict}, {"too-large", "20", ErrInsufficient}, {"bad", "1e1", ErrInvalid},
	} {
		if err = Reserve(ctx, tx, "fund", "USD", tc.id, dec.Exact(tc.debit), opening.ObservedAt, time.Minute); !errors.Is(err, tc.want) {
			t.Fatalf("reserve %s: %v", tc.id, err)
		}
	}
	var debit, admission, reserved string
	if err = tx.QueryRow(ctx, `SELECT required_debit,admission_debit,reserved FROM capital_commitments JOIN capital_balances USING(tenant_id,portfolio_id,currency)`).Scan(&debit, &admission, &reserved); err != nil || debit != "1/3" || admission != "1/3" || reserved != "7/12" {
		t.Fatalf("retained debit=%s admission=%s reserved=%s err=%v", debit, admission, reserved, err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var commitments, members int
	if err = p.QueryRow(ctx, `SELECT reserved,(SELECT count(*) FROM capital_commitments),(SELECT count(*) FROM capital_members) FROM capital_balances`).Scan(&reserved, &commitments, &members); err != nil || reserved != "0.2500" || commitments != 0 || members != 0 {
		t.Fatalf("rollback retained %s/%d/%d: %v", reserved, commitments, members, err)
	}
}

func TestPostgresReserveKernelFreshnessAndReducedSumBounds(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	event := snapshot(1, "5")
	if err := Apply(ctx, p, event); err != nil {
		t.Fatal(err)
	}
	reserve := func(id, amount string, now time.Time, age time.Duration) error {
		return transaction(p, func(tx pgx.Tx) error { return Reserve(ctx, tx, "fund", "USD", id, dec.Exact(amount), now, age) })
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		age  time.Duration
		want error
	}{
		{"future", event.ObservedAt.Add(-1), time.Minute, ErrUnknown},
		{"edge", event.ObservedAt.Add(time.Minute), time.Minute, nil},
		{"expired", event.ObservedAt.Add(time.Minute + 1), time.Minute, ErrUnknown},
		{"far-future", event.ObservedAt.AddDate(500, 0, 0), time.Minute, ErrUnknown},
		{"saturated-go-duration", event.ObservedAt.AddDate(500, 0, 0), time.Duration(math.MaxInt64), nil},
	} {
		if err := reserve(tc.name, "0", tc.now, tc.age); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	largest := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 512), big.NewInt(1))
	// Both operands fit. Adding coprime denominators exceeds 512 bits.
	first := "1/" + largest.String()
	other := "1/" + new(big.Int).Sub(largest, big.NewInt(1)).String()
	if _, err := p.Exec(ctx, `UPDATE capital_balances SET reserved=$1`, first); err != nil {
		t.Fatal(err)
	}
	if err := reserve("normalized-overflow", other, event.ObservedAt, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE capital_balances SET total='0'`); err != nil {
		t.Fatal(err)
	}
	if err := reserve("insufficient-first", other, event.ObservedAt, time.Minute); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("insufficient precedence: %v", err)
	}
}
