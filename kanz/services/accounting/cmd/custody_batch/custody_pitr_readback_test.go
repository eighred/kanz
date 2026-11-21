package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pitrTenant = "synthetic-custody-pilot"

func pilotPITRBrowser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scope *custody.BookScope, expected map[string]string) {
	t.Helper()
	script := os.Getenv("CUSTODY_PILOT_PITR_BROWSER_SCRIPT")
	if !filepath.IsAbs(script) || !filepath.IsAbs(os.Getenv("CUSTODY_PILOT_RETAINED_DOWNLOADS")) {
		t.Fatal("PITR browser script and customer download destination required")
	}
	site := httptest.NewServer(server.New(&server.Readiness{}, slog.Default(), ledger.NewPostgres(pool), "USD",
		server.WithTenant(pitrTenant), server.WithCustodyBookScope(scope), server.WithBreakStore(custody.NewPostgres(pool)), server.WithReadOnlyCustodyReview()))
	defer site.Close()
	provePilotBrowser(t, ctx, site.URL, "TestSignedCustodyOperatorBrowser", script, expected)
}

func pilotPITRCut(t *testing.T, ctx context.Context, directory string, pool *pgxpool.Pool, scope *custody.BookScope, expected map[string]string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(directory)
	temp, tempErr := filepath.EvalSymlinks(os.TempDir())
	rel, relErr := filepath.Rel(temp, dir)
	if err != nil || tempErr != nil || relErr != nil || !filepath.IsAbs(directory) || !filepath.IsLocal(rel) || rel == "." {
		t.Fatal("PITR cut requires its own temporary directory")
	}
	ready := filepath.Join(dir, "cut-ready")
	cut := filepath.Join(dir, "cut.json")
	for _, path := range []string{ready, cut} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("PITR cut requires a fresh checkpoint")
		}
	}
	source := maps.Clone(expected)
	source["phase"] = "source"
	pilotPITRBrowser(t, ctx, pool, scope, source)
	// Rename a complete metadata file before the controller takes its WAL cut.
	tmp, err := os.CreateTemp(dir, "cut-*.tmp")
	if err != nil {
		t.Fatal("cannot create PITR checkpoint")
	}
	defer os.Remove(tmp.Name())
	err = json.NewEncoder(tmp).Encode(source)
	closeErr := tmp.Close()
	if err != nil || closeErr != nil || os.Rename(tmp.Name(), cut) != nil {
		t.Fatal("cannot publish PITR checkpoint")
	}
	wait, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-wait.Done():
			t.Fatal("PITR controller did not acknowledge the cut before revision 2")
		case <-ticker.C:
			stat, err := os.Lstat(ready)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !stat.Mode().IsRegular() {
				t.Fatal("invalid PITR checkpoint acknowledgement")
			}
			return
		}
	}
}

func TestSignedCustodyPITRReadback(t *testing.T) {
	if os.Getenv("CUSTODY_PILOT_PITR_READBACK") != "1" {
		t.Skip("requires a paused, independently restored custody pilot")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (16<<10)+1))
	var e map[string]string
	if err != nil || len(raw) > 16<<10 || json.Unmarshal(raw, &e) != nil || e["phase"] != "restored" || e["portfolio_id"] != "PF1" || e["custodian_id"] != "CUST-A" {
		t.Fatal("invalid restored pilot metadata")
	}
	for _, kind := range []string{"_run", "_statement"} {
		seen := make(map[string]bool)
		for _, phase := range []string{"original", "following", "future"} {
			id := e[phase+kind]
			if !custody.ValidReceiptID(id) || seen[id] {
				t.Fatal("PITR source and future record identities must be distinct")
			}
			seen[id] = true
		}
	}
	day, err := time.Parse(time.DateOnly, e["date"])
	if err != nil || e["following_date"] != day.AddDate(0, 0, 1).Format(time.DateOnly) {
		t.Fatal("invalid restored pilot business dates")
	}
	dsn := os.Getenv("TEST_POSTGRES_URL")
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "postgres" || u.User.Username() != "pilot_check" || u.Path != "/kanz_custody_pilot" ||
		u.Fragment != "" || u.RawQuery != "sslmode=disable" {
		t.Fatal("restricted pilot_check loopback PostgreSQL URL required")
	}
	addr, err := netip.ParseAddrPort(u.Host)
	if err != nil || !addr.Addr().IsLoopback() || addr.Port() == 0 {
		t.Fatal("literal loopback PostgreSQL host and port required")
	}
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), pitrTenant), 2*time.Minute)
	defer cancel()
	pool, err := pg.NewTenantPool(ctx, dsn, pitrTenant)
	if err != nil {
		t.Fatal("cannot open restricted restored tenant")
	}
	defer pool.Close()
	var paused bool
	err = pool.QueryRow(ctx, `SELECT pg_is_in_recovery() AND pg_get_wal_replay_pause_state()='paused'
        AND current_setting('transaction_read_only')='on' AND session_user='pilot_check' AND current_user=session_user
        AND current_database()='kanz_custody_pilot' AND NOT rolsuper AND NOT rolbypassrls
        FROM pg_roles WHERE rolname=current_user`).Scan(&paused)
	if err != nil || !paused {
		t.Fatal("restored readback requires paused recovery and a restricted pilot reader")
	}
	store := custody.NewPostgres(pool)
	sub := custody.Subject{PortfolioID: e["portfolio_id"], CustodianID: e["custodian_id"], BusinessDate: day}
	scope, err := custody.NewBookScope([]custody.Subject{sub}, map[string]map[string][]string{"PF1": {"CUST-A": {"sandbox-account"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err = scope.AdmitAssignments(ctx, time.Now().UTC(), store)
	if err != nil {
		t.Fatal("restored data could not prove the pilot account assignment")
	}
	for _, phase := range []string{"original", "following"} {
		wanted, subject := custody.OutcomeBreaks, sub
		if phase == "following" {
			wanted, subject.BusinessDate = custody.OutcomeClean, day.AddDate(0, 0, 1)
		}
		saved, err := store.HistoricalReceipt(ctx, subject, "sandbox-account", e[phase+"_run"])
		if err != nil || saved.State != "recorded" || saved.Run.Outcome != wanted || saved.StatementID != e[phase+"_statement"] ||
			saved.Run.RunID != e[phase+"_run"] || !saved.AccountAssignmentVerified || saved.SignedStatementSHA256 == "" {
			t.Fatalf("restored %s receipt did not match its signed comparison", phase)
		}
	}
	// Cover intake, materialization and completion. An unavailable API lookup
	// alone cannot distinguish an absent future record from a broken read path.
	var absent bool
	err = pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT FROM custody_runs WHERE run_id=$1 OR statement_id=$2)
        AND NOT EXISTS(SELECT FROM custody_batch_receipts WHERE statement_id=$2)
        AND NOT EXISTS(SELECT FROM custody_statements WHERE statement_id=$2)`, e["future_run"], e["future_statement"]).Scan(&absent)
	if err != nil || !absent || !strings.HasPrefix(e["future_run"], "manual:") {
		t.Fatal("future signed correction must be absent at the recovery cut")
	}
	pilotPITRBrowser(t, ctx, pool, scope, e)
	t.Log("signed customer recovered both retained custody receipts before the future correction")
}
