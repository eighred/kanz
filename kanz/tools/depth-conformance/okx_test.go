package main

import (
	"bytes"
	"encoding/json"
	"github.com/eighred/kanz/internal/dec"
	"testing"
)

func TestOKXCorpusUsesGoParserAndBook(t *testing.T) {
	var a, b bytes.Buffer
	if err := runOKX(&a); err != nil {
		t.Fatal(err)
	}
	if err := runOKX(&b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("OKX corpus nondeterministic")
	}
	var c corpus
	if err := json.Unmarshal(a.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) != 2 || len(c.Cases[0].Steps) != 3 {
		t.Fatal("unexpected corpus size")
	}
	first := decodeBook(t, c.Cases[0].Steps[0].BookWire)
	if first.Mic != "OKX" || first.LastUpdateSequence != 100 || dec.Str(dec.FromProto(first.Bids[0].Price)) != "10" || dec.Str(dec.FromProto(first.Bids[0].Size)) != "3" {
		t.Fatal("Go snapshot duplicate/sort mismatch")
	}
	updated := decodeBook(t, c.Cases[0].Steps[1].BookWire)
	if updated.LastUpdateSequence != 101 || len(updated.Bids) != 1 || dec.Str(dec.FromProto(updated.Bids[0].Size)) != "5" || dec.Str(dec.FromProto(updated.Asks[0].Size)) != "7" {
		t.Fatal("Go equivalentprice/deletion mismatch")
	}
	if len(decodeBook(t, c.Cases[0].Steps[2].BookWire).Asks) != 0 {
		t.Fatal("Go zero deletion failed")
	}
}
