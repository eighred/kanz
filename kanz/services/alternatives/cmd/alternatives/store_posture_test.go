package main

// openStore must never take the in-memory path silently. That path holds the
// append-only commitment journal — every capital call, distribution and NAV mark
// the IRR/TVPI series is computed from — in RAM, and discards it on the next
// restart while the process reports a clean start. These tests pin the REFUSAL
// when nobody asked for it, the WARN naming the consequence when somebody did,
// and the kanz_alternatives_journal_durable gauge. #261.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/services/alternatives/internal/config"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
)

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

// TestOpenStoreRefusesAnUnaskedForEphemeralJournal is the defect itself, kept
// executable: an empty DSN used to return fund.NewMemoryStore() with no log, no
// metric and no error.
func TestOpenStoreRefusesAnUnaskedForEphemeralJournal(t *testing.T) {
	h := &storeLogCapture{}

	store, closeFn, err := openStore(context.Background(), config.Config{}, slog.New(h))
	if err == nil {
		if closeFn != nil {
			closeFn()
		}
		t.Fatalf("openStore returned %T with no error for an EMPTY ALTERNATIVES_DATABASE_URL — every "+
			"capital call and distribution would be held in RAM and discarded on the next restart, and "+
			"IRR/TVPI would then be reported over what was left", store)
		return
	}
	if store != nil {
		t.Errorf("openStore returned a store alongside the refusal: %T", store)
	}
	for _, want := range []string{
		"ALTERNATIVES_DATABASE_URL",
		"IN-MEMORY",
		"DISCARDED",
		"ALTERNATIVES_ALLOW_EPHEMERAL_JOURNAL",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got: %v", want, err)
		}
	}
}

func TestOpenStoreWarnsAndReportsUndurableWhenEphemeralIsAcceptedOutLoud(t *testing.T) {
	h := &storeLogCapture{}

	store, closeFn, err := openStore(context.Background(),
		config.Config{AllowEphemeralJournal: true}, slog.New(h))
	if err != nil {
		t.Fatalf("openStore with AllowEphemeralJournal: %v", err)
	}
	defer closeFn()

	if _, ok := store.(*fund.MemoryStore); !ok {
		t.Fatalf("store = %T, want *fund.MemoryStore (DatabaseURL was empty)", store)
	}

	if !h.hasWarnContaining("IN-MEMORY", "DISCARDED", "exactly ONE replica") {
		t.Fatalf("no WARN naming the in-memory journal, its loss on restart and the one-replica "+
			"precondition; got records: %+v", h.records)
	}

	if got := testutil.ToFloat64(journalDurable); got != 0 {
		t.Errorf("kanz_alternatives_journal_durable = %v, want 0 for the in-memory path", got)
	}
}
