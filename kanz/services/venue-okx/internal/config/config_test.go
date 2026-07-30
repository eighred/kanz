package config

// secret() resolves a <K>_FILE / <K> pair (SEC-01d). Setting <K>_FILE is the
// deployment's declaration that a durable secret mount was intended — see
// venue-okx-deploy.yaml's VENUE_OKX_DATABASE_URL_FILE. Before this test
// existed, a declared-but-unreadable file (bad mount, wrong permissions, typo'd
// path) fell straight through to the plaintext env and then to "", which for
// DatabaseURL is indistinguishable from "no durable store was ever configured"
// — the adapter would boot on an in-memory order view with nothing logged and
// readyz green. These tests pin the fail-fast behaviour that closes that gap.

import (
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setVenueEndpoints supplies the two settings that have no default (#147) so a
// test about something else can reach the code it is actually testing. It
// deliberately uses a non-live host: a fixture that names www.okx.com would be
// copied into a manifest sooner or later.
func setVenueEndpoints(t *testing.T) {
	t.Helper()
	t.Setenv("OKX_BASE_URL", "https://okx.invalid")
	t.Setenv("OKX_TRADING_MODE", "demo")
	t.Setenv("OKX_WS_BASE", "wss://okx.invalid")
}

func TestUnreadableDatabaseURLFileFailsFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")
	t.Setenv("VENUE_OKX_DATABASE_URL_FILE", path)

	_, err := Load()
	if err == nil {
		t.Fatalf("Load() with an unreadable VENUE_OKX_DATABASE_URL_FILE = nil error, want an error — " +
			"a mounted-but-unreadable secret must not silently fall through to \"\" (indistinguishable from " +
			"never having been configured)")
	}
	if !strings.Contains(err.Error(), "VENUE_OKX_DATABASE_URL_FILE") {
		t.Fatalf("error = %q, want it to name VENUE_OKX_DATABASE_URL_FILE", err.Error())
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %q, want it to name the path %q", err.Error(), path)
	}
}

func TestDatabaseURLFromSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database-url")
	// Trailing newline is what a Vault/CSI file mount actually writes.
	if err := os.WriteFile(path, []byte("postgres://u:p@db:5432/venue\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("VENUE_OKX_DATABASE_URL_FILE", path)
	setVenueEndpoints(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DatabaseURL != "postgres://u:p@db:5432/venue" {
		t.Fatalf("DSN from file = %q, want the trimmed DSN", cfg.DatabaseURL)
	}
}

func TestDatabaseURLFileWinsOverPlaintextEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database-url")
	if err := os.WriteFile(path, []byte("postgres://from-file/venue"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("VENUE_OKX_DATABASE_URL_FILE", path)
	t.Setenv("VENUE_OKX_DATABASE_URL", "postgres://from-env/venue")
	setVenueEndpoints(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// SEC-01d: the CSI file mount is the source of truth for a secret.
	if cfg.DatabaseURL != "postgres://from-file/venue" {
		t.Fatalf("DSN = %q, want the file mount to win over the plaintext env", cfg.DatabaseURL)
	}
}

func TestNoDatabaseConfiguredLeavesDSNEmptyWithoutError(t *testing.T) {
	// The dev/rig default: neither _FILE nor the plaintext env set ⇒ empty DSN,
	// no error. openView's caller is responsible for warning about the
	// in-memory consequence; Load() itself must not refuse to start.
	setVenueEndpoints(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DatabaseURL != "" {
		t.Fatalf("DSN = %q, want empty when neither VENUE_OKX_DATABASE_URL nor _FILE is set", cfg.DatabaseURL)
	}
}

// The generic <K>_FILE / <K> precedence and fail-fast rules used to be tested
// here, against this package's own copy of secret(). That copy is gone and the
// rules now live in pkg/secret, tested once in pkg/secret/secret_test.go —
// including the case this file was written for, a declared-but-unreadable mount
// erroring with the var and path named. The DSN-level tests above stay, because
// they assert what THIS service does with the resolved value, which is a
// different question from how the value is resolved.

// THE ENDPOINT MUST BE STATED, NEVER ASSUMED (#147).
//
// OKX_BASE_URL and OKX_WS_BASE used to default to https://www.okx.com and
// wss://ws.okx.com:8443. OKX has no demo hostname — demo is the per-request
// `x-simulated-trading: 1` header, which the adapter now sends under
// OKX_TRADING_MODE (#147) — so that default meant deleting a line from the
// manifest promoted the adapter to the LIVE order book instead of degrading it.
// These two tests are the difference between "nobody configured this" and
// "somebody chose production", on the one path where that distinction is
// denominated in money.
//
// The endpoint and the mode are SEPARATE fail-closed checks because they answer
// separate questions: the endpoint decides which exchange is reached at all, the
// mode decides which book at that exchange. Neither can substitute for the other,
// which is why TestTradingModeHasNoDefaultAndFailsClosed exists alongside this.
func TestVenueEndpointsHaveNoDefaultAndFailClosed(t *testing.T) {
	for _, missing := range []string{"OKX_BASE_URL", "OKX_WS_BASE"} {
		t.Run("missing="+missing, func(t *testing.T) {
			setVenueEndpoints(t)
			t.Setenv(missing, "") // the variable the deployment forgot

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s unset = nil error, want a refusal — an unset venue "+
					"endpoint must not resolve to the live exchange", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error = %q, want it to name %s — a pod that will not start is only "+
					"actionable if the log says which setting is missing", err.Error(), missing)
			}
		})
	}
}

// The counterpart: an endpoint that IS stated is used verbatim. A fail-closed
// check that also mangled the configured value would trade one silent wrong
// destination for another.
func TestVenueEndpointsAreUsedAsConfigured(t *testing.T) {
	t.Setenv("OKX_BASE_URL", "https://sandbox.example.invalid")
	t.Setenv("OKX_TRADING_MODE", "demo")
	t.Setenv("OKX_WS_BASE", "wss://sandbox.example.invalid/ws")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.BaseURL != "https://sandbox.example.invalid" {
		t.Errorf("BaseURL = %q, want the configured value verbatim", cfg.BaseURL)
	}
	if cfg.WSBase != "wss://sandbox.example.invalid/ws" {
		t.Errorf("WSBase = %q, want the configured value verbatim", cfg.WSBase)
	}
}

// THE MODE MUST BE STATED, NEVER ASSUMED (#147) — the same rule as the endpoint,
// on the axis the endpoint cannot express.
//
// Removing the OKX_BASE_URL default made "nobody configured this" stop meaning
// live. It could not make demo REACHABLE: no URL reaches OKX's demo book, so
// until the header existed every authenticated order went to production whatever
// the endpoint said. OKX_TRADING_MODE is that missing axis, and it fails closed
// for the same reason — a default here is a default about whether orders are real.
func TestTradingModeHasNoDefaultAndFailsClosed(t *testing.T) {
	t.Run("unset is refused", func(t *testing.T) {
		setVenueEndpoints(t)
		t.Setenv("OKX_TRADING_MODE", "")

		_, err := Load()
		if err == nil {
			t.Fatal("Load() with OKX_TRADING_MODE unset = nil error, want a refusal — an unstated " +
				"mode must not resolve to live trading")
		}
		if !strings.Contains(err.Error(), "OKX_TRADING_MODE") {
			t.Errorf("error = %q, want it to name OKX_TRADING_MODE", err.Error())
		}
	})

	// A TYPO MUST NOT BE A MODE. "testnet" and "sandbox" are the words a reader
	// would reach for from other exchanges, and both are wrong here — accepting
	// anything non-empty as "not live" would make a misspelling place real orders.
	for _, bad := range []string{"testnet", "sandbox", "simulated", "true", "1", "Live", "DEMO"} {
		t.Run("rejected="+bad, func(t *testing.T) {
			setVenueEndpoints(t)
			t.Setenv("OKX_TRADING_MODE", bad)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted OKX_TRADING_MODE=%q — only \"demo\" and \"live\" mean "+
					"anything, and a near-miss that resolved to live would spend real money", bad)
			}
		})
	}

	// Both accepted values must survive verbatim onto the Config the adapter runs
	// with: a mode that is validated and then dropped is not a mode.
	for _, want := range []exchangeauth.OKXTradingMode{exchangeauth.OKXDemo, exchangeauth.OKXLive} {
		t.Run("accepted="+string(want), func(t *testing.T) {
			setVenueEndpoints(t)
			t.Setenv("OKX_TRADING_MODE", string(want))

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.TradingMode != want {
				t.Errorf("TradingMode = %q, want %q", string(cfg.TradingMode), string(want))
			}
		})
	}
}
