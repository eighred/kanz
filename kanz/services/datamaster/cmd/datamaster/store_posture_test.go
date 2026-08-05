package main

// openStores must never take the in-memory path silently. That path holds the
// pricing-oversight EXCEPTION QUEUE — a named human's signed decision to accept
// a price the system flagged — in a map, and it does not even arrive on the bus,
// so nothing can rebuild it. It also returns a NIL cycle lock, so every replica
// refreshes against a vendor API billed per call. These tests pin the REFUSAL,
// the WARN, the gauge, and the nil lock that makes the replica count matter.
// #261.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/services/datamaster/internal/config"
	"github.com/eighred/kanz/services/datamaster/internal/store"
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

// TestOpenStoresRefusesAnUnaskedForEphemeralMaster is the defect itself, kept
// executable: an empty DSN used to return the in-memory pair with no log, no
// metric and no error.
func TestOpenStoresRefusesAnUnaskedForEphemeralMaster(t *testing.T) {
	h := &storeLogCapture{}

	golden, exceptions, lock, closeFn, err := openStores(context.Background(),
		config.Config{}, slog.New(h))
	if err == nil {
		if closeFn != nil {
			closeFn()
		}
		t.Fatalf("openStores returned (%T, %T) with no error for an EMPTY DATAMASTER_DATABASE_URL — every "+
			"operator override would be written to a map and deleted by the next rolling update",
			golden, exceptions)
		return
	}
	if golden != nil || exceptions != nil || lock != nil {
		t.Errorf("openStores returned stores alongside the refusal: golden=%T exceptions=%T lock=%T",
			golden, exceptions, lock)
	}
	for _, want := range []string{
		"DATAMASTER_DATABASE_URL",
		"IN-MEMORY",
		"DISCARDED",
		"DATAMASTER_ALLOW_EPHEMERAL_MASTER",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got: %v", want, err)
		}
	}
	// The metered-vendor consequence is the half that is invisible from the store
	// seam, and the half a reader would otherwise never connect to a missing DSN.
	if !strings.Contains(err.Error(), "cycle lock") {
		t.Errorf("refusal does not mention the absent cycle lock, so nothing warns that every replica "+
			"would refresh against a metered vendor API; got: %v", err)
	}
}

func TestOpenStoresWarnsAndReportsUndurableWhenEphemeralIsAcceptedOutLoud(t *testing.T) {
	h := &storeLogCapture{}

	golden, exceptions, lock, closeFn, err := openStores(context.Background(),
		config.Config{AllowEphemeralMaster: true}, slog.New(h))
	if err != nil {
		t.Fatalf("openStores with AllowEphemeralMaster: %v", err)
	}
	defer closeFn()

	if _, ok := golden.(*store.MemoryGoldenStore); !ok {
		t.Fatalf("golden = %T, want *store.MemoryGoldenStore (DatabaseURL was empty)", golden)
	}
	if exceptions == nil {
		t.Error("openStores returned a nil exception queue on the opted-in path")
	}
	// Pinned deliberately: run() only applies projector.WithCycleLock when this is
	// non-nil, so the nil IS the "every replica hits the vendor" behaviour the
	// warning describes. If a future change starts returning a real lock here, the
	// warning becomes wrong and this test says so.
	if lock != nil {
		t.Errorf("cycle lock = %T on the in-memory path, want nil — the WARN claims there is none", lock)
	}

	if !h.hasWarnContaining("IN-MEMORY", "DISCARDED", "exactly ONE replica") {
		t.Fatalf("no WARN naming the in-memory master, the discarded overrides and the one-replica "+
			"precondition; got records: %+v", h.records)
	}
	if !h.hasWarnContaining("cycle lock is ABSENT") {
		t.Errorf("the WARN does not say the cycle lock is absent; got records: %+v", h.records)
	}

	if got := testutil.ToFloat64(masterDurable); got != 0 {
		t.Errorf("kanz_datamaster_master_durable = %v, want 0 for the in-memory path", got)
	}
}
