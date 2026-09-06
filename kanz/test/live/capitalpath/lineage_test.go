package main

import (
	"sync"
	"testing"
)

func TestLineageRecordsEveryFactOnceRegardlessOfArrivalOrder(t *testing.T) {
	var got lineage
	for _, kind := range []factKind{factAccounted, factFilled, factRouted, factAccepted} {
		if duplicate := got.observe(kind); duplicate {
			t.Fatalf("first observation of %v reported duplicate", kind)
		}
	}
	if err := got.complete(); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !got.observe(factFilled) {
		t.Fatal("second fill FACT was not reported as a duplicate")
	}
}

func TestLineageConcurrentDuplicateObservationIsDeterministic(t *testing.T) {
	var got lineage
	const writers = 128
	var wg sync.WaitGroup
	duplicates := make(chan bool, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			duplicates <- got.observe(factAccepted)
		}()
	}
	wg.Wait()
	close(duplicates)
	var n int
	for duplicate := range duplicates {
		if duplicate {
			n++
		}
	}
	if n != writers-1 {
		t.Fatalf("duplicates=%d want %d", n, writers-1)
	}
}

func TestLineageObservationAllocatesNothing(t *testing.T) {
	var got lineage
	if n := testing.AllocsPerRun(1000, func() {
		got.reset()
		_ = got.observe(factAccepted)
		_ = got.observe(factRouted)
		_ = got.observe(factFilled)
		_ = got.observe(factAccounted)
	}); n != 0 {
		t.Fatalf("allocations/run=%v want 0", n)
	}
}

func TestLineageRefusesAnIncompleteCertification(t *testing.T) {
	var got lineage
	got.observe(factAccepted)
	got.observe(factRouted)
	got.observe(factFilled)
	if err := got.complete(); err != errMissingAccounted {
		t.Fatalf("complete err=%v want errMissingAccounted", err)
	}
	got.reset()
	got.observe(factAccepted)
	got.observe(factRouted)
	if err := got.complete(); err != errMissingFill {
		t.Fatalf("complete err=%v want errMissingFill", err)
	}
}
