package main

// openStore must never take the in-memory path silently. That path holds the
// platform's tamper-evidence record in RAM and discards it on the next restart,
// while the process starts clean, reports /readyz 200 and answers queries — and
// nothing downstream of audit consumes the audit log, so there is no second
// observer to notice. These tests pin the three things that make it visible:
// the REFUSAL when nobody asked for it, the WARN naming the consequence when
// somebody did, and the kanz_audit_log_durable gauge that makes the degraded
// posture alertable instead of a log line that scrolled off at boot.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/services/audit/internal/audit"
	"github.com/eighred/kanz/services/audit/internal/config"
)

// capturingHandler records every emitted record so a test can assert on level +
// message without parsing the JSON output.
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

// TestOpenStoreRefusesAnUnaskedForEphemeralAuditLog is the defect itself, kept
// executable: an empty DSN used to return audit.NewMemory() with no log, no
// metric and no gauge.
func TestOpenStoreRefusesAnUnaskedForEphemeralAuditLog(t *testing.T) {
	h := &capturingHandler{}

	store, closeFn, err := openStore(context.Background(), config.Config{}, slog.New(h))
	if err == nil {
		if closeFn != nil {
			closeFn()
		}
		t.Fatalf("openStore returned %T with no error for an EMPTY AUDIT_DATABASE_URL — the compliance "+
			"record would be held in RAM and discarded on the next restart, and the process would "+
			"report a clean start", store)
		return
	}
	if store != nil {
		t.Errorf("openStore returned a store alongside the refusal: %T", store)
	}
	// The message is the whole fix: it has to tell an operator what was lost and
	// what to do, not just that something is missing.
	for _, want := range []string{"AUDIT_DATABASE_URL", "IN-MEMORY", "DISCARDED", "AUDIT_ALLOW_EPHEMERAL_LOG"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got: %v", want, err)
		}
	}
}

// TestOpenStoreWarnsAndReportsUndurableWhenEphemeralIsAcceptedOutLoud: the
// escape hatch is deployable, but it is not quiet. An operator who opted in
// still gets the consequence in the log and a 0 on the dashboard.
func TestOpenStoreWarnsAndReportsUndurableWhenEphemeralIsAcceptedOutLoud(t *testing.T) {
	h := &capturingHandler{}
	logger := slog.New(h)

	store, closeFn, err := openStore(context.Background(), config.Config{AllowEphemeralLog: true}, logger)
	if err != nil {
		t.Fatalf("openStore with AllowEphemeralLog: %v", err)
	}
	defer closeFn()

	if _, ok := store.(*audit.Memory); !ok {
		t.Fatalf("store = %T, want *audit.Memory (DatabaseURL was empty)", store)
	}

	if !h.hasWarnContaining("IN-MEMORY", "DISCARDED", "not a system of record") {
		t.Fatalf("no WARN naming the in-memory audit log and its operational consequence; got records: %+v", h.records)
	}

	if got := testutil.ToFloat64(auditLogDurable); got != 0 {
		t.Errorf("kanz_audit_log_durable = %v, want 0 for the in-memory path", got)
	}
}
