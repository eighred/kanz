package main

// openLinkStore must never take the in-memory path silently. The link store owns
// the FILING HASH CHAIN HEAD, and the manifest's replicas: 2 is legal only
// because the database owns it: two in-process heads fork the audit chain, and
// the fork is undetectable because the forked hashes differ. At one replica the
// chain resets to Genesis on every restart. These tests pin the REFUSAL, the
// WARN, the kanz_regulatory_chain_durable gauge, and the "hash" signer posture
// that legitimately needs no DSN at all. #261.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/internal/audit/linkstore"
	"github.com/eighred/kanz/internal/regulatory"
	"github.com/eighred/kanz/services/regulatory/internal/config"
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

// TestOpenLinkStoreRefusesAnUnaskedForEphemeralChain is the defect itself, kept
// executable: an empty DSN used to return linkstore.NewMemoryStore() with no
// log, no metric and no error.
func TestOpenLinkStoreRefusesAnUnaskedForEphemeralChain(t *testing.T) {
	h := &storeLogCapture{}

	store, closeFn, err := openLinkStore(context.Background(), config.Config{}, slog.New(h))
	if err == nil {
		if closeFn != nil {
			closeFn()
		}
		t.Fatalf("openLinkStore returned %T with no error for an EMPTY REGULATORY_DATABASE_URL — the "+
			"filing hash chain would reset to Genesis on every restart, and the shipped replicas: 2 would "+
			"fork it", store)
		return
	}
	if store != nil {
		t.Errorf("openLinkStore returned a store alongside the refusal: %T", store)
	}
	for _, want := range []string{
		"REGULATORY_DATABASE_URL",
		"IN-MEMORY",
		"REGULATORY_ALLOW_EPHEMERAL_CHAIN",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got: %v", want, err)
		}
	}
	// The forked-chain consequence is the one a regulator would ask about, and it
	// is the reason this refuses rather than warns.
	for _, want := range []string{"GENESIS", "DIVERGENT"} {
		if !strings.Contains(strings.ToUpper(err.Error()), want) {
			t.Errorf("refusal does not name the %s consequence; got: %v", want, err)
		}
	}
	// The other legitimate DSN-free posture must be offered, not just forbidden.
	if !strings.Contains(err.Error(), "REGULATORY_SIGNER=hash") {
		t.Errorf("refusal does not point at the bare-digest signer, which needs no DSN; got: %v", err)
	}
}

func TestOpenLinkStoreWarnsAndReportsUndurableWhenEphemeralIsAcceptedOutLoud(t *testing.T) {
	h := &storeLogCapture{}

	store, closeFn, err := openLinkStore(context.Background(),
		config.Config{AllowEphemeralChain: true}, slog.New(h))
	if err != nil {
		t.Fatalf("openLinkStore with AllowEphemeralChain: %v", err)
	}
	defer closeFn()

	if _, ok := store.(*linkstore.MemoryStore); !ok {
		t.Fatalf("store = %T, want *linkstore.MemoryStore (DatabaseURL was empty)", store)
	}

	if !h.hasWarnContaining("IN-MEMORY", "GENESIS", "exactly ONE replica") {
		t.Fatalf("no WARN naming the in-process chain, the Genesis reset and the one-replica precondition; "+
			"got records: %+v", h.records)
	}

	if got := testutil.ToFloat64(chainDurable); got != 0 {
		t.Errorf("kanz_regulatory_chain_durable = %v, want 0 for the in-memory path", got)
	}
}

// TestHashSignerNeedsNoDatabase pins the posture the refusal must NOT break. A
// bare content digest makes no chain claim, so it builds no link store and never
// reaches openLinkStore — a DSN-free "hash" deployment stays legitimate.
//
// Without this, the obvious next edit — hoisting the DSN check into buildSigner
// or config.Load — would look correct and would break a supported deployment
// that no other test covers.
func TestHashSignerNeedsNoDatabase(t *testing.T) {
	h := &storeLogCapture{}

	sgnr, closeFn, err := buildSigner(context.Background(),
		config.Config{Signer: "hash"}, slog.New(h))
	if err != nil {
		t.Fatalf("buildSigner with Signer=hash and no DSN: %v — the bare-digest backend makes no chain "+
			"claim and must not require a database", err)
	}
	defer closeFn()

	if _, ok := sgnr.(regulatory.HashSigner); !ok {
		t.Fatalf("signer = %T, want regulatory.HashSigner", sgnr)
	}
}
