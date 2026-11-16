package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
)

func decodeBook(t *testing.T, ints []int) *marketpb.OrderBookSnapshot {
	t.Helper()
	wire := make([]byte, len(ints))
	for i, n := range ints {
		if n < 0 || n > 255 {
			t.Fatal("invalid wire byte")
		}
		wire[i] = byte(n)
	}
	s := new(marketpb.OrderBookSnapshot)
	if err := proto.Unmarshal(wire, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCorpusDeterministicAndBookSemantics(t *testing.T) {
	var first, second bytes.Buffer
	if err := run(&first); err != nil {
		t.Fatal(err)
	}
	if err := run(&second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("corpus depends on wall clock or map ordering")
	}
	var c corpus
	if err := json.Unmarshal(first.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) != 4 {
		t.Fatalf("case count %d", len(c.Cases))
	}
	for _, f := range c.Cases {
		for _, step := range f.Steps {
			s := decodeBook(t, step.BookWire)
			// Reference predicate mirrors ingest.topOfBook, not a second production
			// implementation. Eligibility is independently specified in fixtures.
			eligible := len(s.Bids) > 0 && len(s.Asks) > 0 && dec.IsPositive(s.Bids[0].Price) && dec.IsPositive(s.Asks[0].Price) && dec.Cmp(s.Asks[0].Price, s.Bids[0].Price) > 0
			if eligible != step.QuoteEligible {
				t.Fatalf("%s quote eligibility", f.Name)
			}
		}
	}
	initial := decodeBook(t, c.Cases[0].Steps[0].BookWire)
	if len(initial.Bids) != 3 || dec.Str(dec.FromProto(initial.Bids[0].Price)) != "10" || dec.Str(dec.FromProto(initial.Bids[0].Size)) != "3" {
		t.Fatal("numeric ordering / last positive duplicate changed")
	}
	// Snapshot zero duplicates are skipped; delta zero duplicates delete.
	if dec.Str(dec.FromProto(initial.Bids[2].Size)) != "1" {
		t.Fatal("snapshot zero changed")
	}
	updated := decodeBook(t, c.Cases[0].Steps[1].BookWire)
	if len(updated.Bids) != 1 || dec.Str(dec.FromProto(updated.Bids[0].Price)) != "2" || dec.Str(dec.FromProto(updated.Bids[0].Size)) != "5" {
		t.Fatal("equivalent price replacement or deletion changed")
	}
	if dec.Str(dec.FromProto(updated.Asks[0].Size)) != "7" {
		t.Fatal("last delta duplicate did not win")
	}
	sequence := c.Cases[2].Steps
	for i, want := range []uint64{100, 102, 110, 111} {
		if got := decodeBook(t, sequence[i].BookWire).LastUpdateSequence; got != want {
			t.Fatalf("sequence[%d]=%d, want%d", i, got, want)
		}
	}
}
