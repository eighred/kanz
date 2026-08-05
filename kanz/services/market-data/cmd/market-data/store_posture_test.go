package main

// openStore must never take the in-memory path silently. That path holds the
// price history the risk engine marks positions against, discards it on the next
// restart, and — because every pod shares one durable consumer group — gives each
// replica a DISJOINT slice of it, so a query is answered by whichever pod the
// Service picked. The process reports a clean start and /readyz 200 throughout.
//
// These tests pin the two things that make it visible: the WARN naming the
// consequence AND the single-replica precondition, and the
// kanz_market_data_price_history_durable gauge that makes the degraded posture
// alertable after the boot log has scrolled off. #261.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/internal/marketdata/store"
	"github.com/eighred/kanz/services/market-data/internal/config"
)

// storeLogCapture is a minimal slog.Handler that records every emitted record so
// a test can assert on level + message without parsing the JSON output. Named
// for this file rather than `capturingHandler` because feed_producer_test.go
// shares the package.
type storeLogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *storeLogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *storeLogCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *storeLogCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *storeLogCapture) WithGroup(string) slog.Handler      { return h }

func (h *storeLogCapture) hasWarnContaining(substrs ...string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		match := true
		for _, s := range substrs {
			if !strings.Contains(r.Message, s) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestOpenStoreInMemoryWarnsAndReportsUndurable is the defect itself, kept
// executable: an empty DSN used to return store.NewMemory() with no log, no
// metric and no error.
func TestOpenStoreInMemoryWarnsAndReportsUndurable(t *testing.T) {
	h := &storeLogCapture{}

	st, closeFn, err := openStore(context.Background(), config.Config{}, slog.New(h))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer closeFn()

	if _, ok := st.(*store.Memory); !ok {
		t.Fatalf("store = %T, want *store.Memory (DatabaseURL was empty)", st)
	}

	// The single-replica precondition is the half that is easy to drop, and it is
	// the half that matters: "history is lost on restart" describes a dev rig,
	// while the shipped manifest runs two pods and KEDA scales it to sixteen.
	if !h.hasWarnContaining("IN-MEMORY", "DISCARDED", "exactly ONE replica", "DISJOINT") {
		t.Fatalf("no WARN naming the in-memory price history, its loss on restart AND the one-replica "+
			"precondition; got records: %+v", h.records)
	}
	if !h.hasWarnContaining("MARKET_DATA_DATABASE_URL") {
		t.Errorf("the WARN does not name the variable that fixes it; got records: %+v", h.records)
	}

	if got := testutil.ToFloat64(priceHistoryDurable); got != 0 {
		t.Errorf("kanz_market_data_price_history_durable = %v, want 0 for the in-memory path", got)
	}
}
