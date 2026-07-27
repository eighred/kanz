package main

// openView must never take the in-memory path silently: that path loses
// exactly the state the healing seam needs on every restart, while the
// process keeps serving orders to a live exchange. These tests pin the WARN
// (naming the operational consequence) and the kanz_venue_orderview_durable
// gauge that let an operator see the degraded posture without reading logs.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kanz-eng/kanz/internal/venueadapter/orderview"
	"github.com/kanz-eng/kanz/services/venue-okx/internal/config"
)

// capturingHandler is a minimal slog.Handler that records every emitted
// record so a test can assert on level + message without parsing text/JSON
// output.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) hasWarnContaining(substrs ...string) bool {
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

func TestOpenViewInMemoryWarnsAndReportsUndurable(t *testing.T) {
	h := &capturingHandler{}
	logger := slog.New(h)

	store, closeFn, err := openView(context.Background(), config.Config{}, logger)
	if err != nil {
		t.Fatalf("openView: %v", err)
	}
	defer closeFn()

	if _, ok := store.(*orderview.Memory); !ok {
		t.Fatalf("store = %T, want *orderview.Memory (DatabaseURL was empty)", store)
	}

	if !h.hasWarnContaining("IN-MEMORY", "healing watchdog goes blind", "restart") {
		t.Fatalf("no WARN log naming the in-memory order view and its operational consequence; got records: %+v", h.records)
	}

	if got := testutil.ToFloat64(orderViewDurable); got != 0 {
		t.Errorf("kanz_venue_orderview_durable = %v, want 0 for the in-memory path", got)
	}
}
