package main

// openStore must never take the in-memory path silently. That path loses every
// household on restart and then SERVES THE EMPTY BOOK as an authoritative
// exposure view, because the durable consumer group resumes at its last ack and
// never re-reads the valuations it already folded. These tests pin the REFUSAL
// when nobody asked for it, the WARN naming the consequence when somebody did,
// and the kanz_wealth_book_durable gauge. #261.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/config"
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

// TestOpenStoreRefusesAnUnaskedForEphemeralBook is the defect itself, kept
// executable: an empty DSN used to return book.NewMemoryStore() with no log, no
// metric and no error.
func TestOpenStoreRefusesAnUnaskedForEphemeralBook(t *testing.T) {
	h := &storeLogCapture{}

	store, closeFn, err := openStore(context.Background(), config.Config{}, slog.New(h))
	if err == nil {
		if closeFn != nil {
			closeFn()
		}
		t.Fatalf("openStore returned %T with no error for an EMPTY WEALTH_DATABASE_URL — every household "+
			"would be held in RAM, discarded on the next restart, and the pod would then serve an EMPTY "+
			"exposure view as though it were the answer", store)
		return
	}
	if store != nil {
		t.Errorf("openStore returned a store alongside the refusal: %T", store)
	}
	for _, want := range []string{
		"WEALTH_DATABASE_URL",
		"IN-MEMORY",
		"DISCARDED",
		"WEALTH_ALLOW_EPHEMERAL_BOOK",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got: %v", want, err)
		}
	}
	// The refusal must name the reason nothing refills the book, not merely that
	// it is lost — that is what distinguishes this from a cache and is why the
	// compacted WEALTH stream does not rescue it.
	if !strings.Contains(err.Error(), "last ack") {
		t.Errorf("refusal does not say why the book cannot be rebuilt (the consumer group resumes at its "+
			"last ack); got: %v", err)
	}
}

func TestOpenStoreWarnsAndReportsUndurableWhenEphemeralIsAcceptedOutLoud(t *testing.T) {
	h := &storeLogCapture{}

	store, closeFn, err := openStore(context.Background(),
		config.Config{AllowEphemeralBook: true}, slog.New(h))
	if err != nil {
		t.Fatalf("openStore with AllowEphemeralBook: %v", err)
	}
	defer closeFn()

	if _, ok := store.(*book.MemoryStore); !ok {
		t.Fatalf("store = %T, want *book.MemoryStore (DatabaseURL was empty)", store)
	}

	if !h.hasWarnContaining("IN-MEMORY", "DISCARDED", "exactly ONE replica") {
		t.Fatalf("no WARN naming the in-memory book, its loss on restart and the one-replica "+
			"precondition; got records: %+v", h.records)
	}

	if got := testutil.ToFloat64(bookDurable); got != 0 {
		t.Errorf("kanz_wealth_book_durable = %v, want 0 for the in-memory path", got)
	}
}
