package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"google.golang.org/protobuf/proto"
)

func boundedFixture(n int) []Observation {
	out := make([]Observation, 0, n*4)
	for i := 0; i < n; i++ {
		for _, kind := range []PriceKind{PriceKindClose, PriceKindOpen} {
			out = append(out, obs("BOUND", i, dec(int64(i+100), 0), kind, i))
			out = append(out, obs("BOUND", i, dec(int64(i+200), 0), kind, i+2))
		}
	}
	return out
}

func checkBoundedParity(t *testing.T, s Store) {
	t.Helper()
	for _, kind := range []PriceKind{PriceKindUnspecified, PriceKindClose, PriceKindOpen} {
		for _, horizon := range []int{1, 40, 5000, 20000} {
			for _, limit := range []int{1, 2, 7, 251, MaxHistoryLimit} {
				q := Query{InstrumentID: "BOUND", Kind: kind, Start: day(2), End: day(9998), AsOf: day(horizon)}
				all, err := s.History(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				q.Limit = limit
				got, err := s.History(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				if len(all) > limit {
					all = all[len(all)-limit:]
				}
				if len(got) != len(all) || cap(got) > limit {
					t.Fatalf("kind=%d horizon=%d limit=%d lengths %d/%d capacity %d", kind, horizon, limit, len(got), len(all), cap(got))
				}
				for i := range all {
					a, b := all[i], got[i]
					if !a.ObservationTime.Equal(b.ObservationTime) || !a.KnowledgeTime.Equal(b.KnowledgeTime) || a.Kind != b.Kind || a.CurrencyCode != b.CurrencyCode || !proto.Equal(a.Price, b.Price) {
						t.Fatalf("q=%+v row=%d got=%+v want=%+v", q, i, b, a)
					}
				}
			}
		}
	}
	for _, limit := range []int{-1, MaxHistoryLimit + 1, math.MaxInt} {
		if _, err := s.History(context.Background(), Query{InstrumentID: "BOUND", Limit: limit}); !errors.Is(err, ErrHistoryLimit) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.History(ctx, Query{InstrumentID: "BOUND", Limit: 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.History(context.Background(), Query{Limit: 2}); err == nil {
		t.Fatal("empty instrument accepted")
	}
}

func TestBoundedMemory(t *testing.T) {
	s := NewMemory()
	mustPut(t, s, boundedFixture(300)...)
	checkBoundedParity(t, s)
	q := Query{InstrumentID: "BOUND", Limit: 2}
	a, err := s.History(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	original := a[0].Price.Coefficient
	a[0].Price.Coefficient = -1
	b, err := s.History(context.Background(), q)
	if err != nil || b[0].Price.Coefficient != original {
		t.Fatal("bounded read aliases store")
	}
}

func BenchmarkBoundedMemory(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := NewMemory()
			if err := s.Put(context.Background(), boundedFixture(n)); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := s.History(context.Background(), Query{InstrumentID: "BOUND", Kind: PriceKindClose, Limit: 251}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
