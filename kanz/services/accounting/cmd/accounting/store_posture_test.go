package main

// openStore must never take the in-memory path silently. That path holds the
// fund's BOOK OF RECORD in RAM and discards it on the next restart, while the
// process starts clean, reports /readyz 200 and materializes NAV from a journal
// that looks right. These tests pin the three things that make it visible: the
// REFUSAL when nobody asked for it, the WARN naming the consequence when
// somebody did, and the kanz_accounting_ledger_durable gauge that makes the
// degraded posture alertable instead of a log line that scrolled off at boot.
// #261.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// storeLogCapture records every emitted record so a test can assert on level +
// message without parsing the JSON output. Named for this file rather than
// `capturingHandler` because cash_publisher_test.go shares the package.
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

// TestOpenStoreRefusesAnUnaskedForEphemeralLedger is the defect itself, kept
// executable: an empty DSN used to return ledger.NewMemoryStore() with no log,
// no metric and no error.
func TestOpenStoreRefusesAnUnaskedForEphemeralLedger(t *testing.T) {
	h := &storeLogCapture{}

	store, _, closeFn, err := openStore(context.Background(), config.Config{}, slog.New(h))
	if err == nil {
		if closeFn != nil {
			closeFn()
		}
		t.Fatalf("openStore returned %T with no error for an EMPTY ACCOUNTING_DATABASE_URL — the fund's "+
			"book of record would be held in RAM and discarded on the next restart, and the process would "+
			"report a clean start", store)
		return
	}
	if store != nil {
		t.Errorf("openStore returned a store alongside the refusal: %T", store)
	}
	// The message is the whole fix: it has to tell an operator what was lost and
	// what to do, not just that something is missing.
	for _, want := range []string{
		"ACCOUNTING_DATABASE_URL",
		"IN-MEMORY",
		"DISCARDED",
		"ACCOUNTING_ALLOW_EPHEMERAL_LEDGER",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got: %v", want, err)
		}
	}
}

// TestOpenStoreWarnsAndReportsUndurableWhenEphemeralIsAcceptedOutLoud: the
// escape hatch is deployable, but it is not quiet. An operator who opted in
// still gets the consequence in the log and a 0 on the dashboard.
func TestOpenStoreWarnsAndReportsUndurableWhenEphemeralIsAcceptedOutLoud(t *testing.T) {
	h := &storeLogCapture{}

	store, _, closeFn, err := openStore(context.Background(),
		config.Config{AllowEphemeralLedger: true}, slog.New(h))
	if err != nil {
		t.Fatalf("openStore with AllowEphemeralLedger: %v", err)
	}
	defer closeFn()

	if _, ok := store.(*ledger.MemoryStore); !ok {
		t.Fatalf("store = %T, want *ledger.MemoryStore (DatabaseURL was empty)", store)
	}

	if !h.hasWarnContaining("IN-MEMORY", "DISCARDED", "exactly ONE replica") {
		t.Fatalf("no WARN naming the in-memory ledger, its loss on restart and the one-replica "+
			"precondition; got records: %+v", h.records)
	}

	if got := testutil.ToFloat64(ledgerDurable); got != 0 {
		t.Errorf("kanz_accounting_ledger_durable = %v, want 0 for the in-memory path", got)
	}
}
