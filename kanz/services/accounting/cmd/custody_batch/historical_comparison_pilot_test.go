package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/eighred/kanz/services/accounting/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSignedHistoricalComparisonPilot(t *testing.T) {
	if os.Getenv("CUSTODY_PILOT_VERIFY") != "1" {
		t.Skip("requires a separately provisioned historical comparison pilot")
	}
	const tenant = "synthetic-custody-pilot"
	expected := map[string]string{"portfolio_id": "PF1", "custodian_id": "CUST-A"}
	retained := make(map[string][]byte)
	files := []pilotStatementFile{
		{name: "statement-r1", revision: 1, position: big.NewRat(19, 2), trade: true},
		{name: "following-r1", revision: 1, dayOffset: 1, position: big.NewRat(21, 2)},
		{name: "statement-r2", revision: 2, position: big.NewRat(21, 2), trade: true},
	}
	checkPilotSeries(t, 2*time.Minute, filepath.Join("testdata", "late_quantity"), nil, files,
		func(ctx context.Context, pool *pgxpool.Pool, statement custody.Statement, stage int, broker pilotBroker) {
			ctx = bus.WithTenantID(ctx, tenant)
			subject := statement.Subject()
			journal := ledger.NewPostgres(pool)
			if stage == 1 {
				appendPilotTrade(t, ctx, journal, subject.BusinessDate)
			}
			store, scope, reconciler := pilotComparison(t, ctx, pool, journal, subject, broker)
			if stage < len(files) {
				run, err := reconciler.Reconcile(ctx, subject)
				want := custody.OutcomeBreaks
				if stage == 2 {
					want = custody.OutcomeClean
				}
				if err != nil || run.Outcome != want || run.StatementID != statement.StatementID {
					t.Fatalf("ordinary comparison %d: outcome=%v error=%v", stage, run.Outcome, err)
				}
				fact, payload := pilotCompletion(t, ctx, broker, uint64(stage))
				if fact.GetRunId() != run.RunID || fact.GetStatementId() != statement.StatementID {
					t.Fatal("ordinary comparison differs from its broker completion")
				}
				retained[run.RunID] = payload
				if stage == 1 {
					expected["date"] = subject.BusinessDate.Format(time.DateOnly)
					expected["original_run"], expected["original_statement"] = run.RunID, statement.StatementID
				} else {
					expected["following_date"] = subject.BusinessDate.Format(time.DateOnly)
					expected["following_run"], expected["following_statement"] = run.RunID, statement.StatementID
				}
				return
			}
			items, err := store.OutstandingBreaks(ctx, subject.PortfolioID)
			if err != nil || len(items) != 1 || items[0].Kind != recon.BreakExecutionQuantity || items[0].Detection.RunID != expected["original_run"] {
				t.Fatalf("expected the earlier execution quantity to remain open: %v", err)
			}
			expected["break_id"], expected["statement_id"] = items[0].BreakID, statement.StatementID
			before := pilotHistoricalWorkingState(t, ctx, pool)
			site := httptest.NewServer(server.New(&server.Readiness{}, slog.Default(), journal, "USD",
				server.WithTenant(tenant), server.WithBreakStore(store), server.WithCustodyBookScope(scope),
				server.WithCustodyReruns("custody-pilot-rerun", reconciler.ReconcileManual)))
			defer site.Close()
			script := os.Getenv("CUSTODY_PILOT_BROWSER_SCRIPT")
			expected["phase"] = "operator"
			provePilotBrowser(t, ctx, site.URL, "TestSignedCustodyRerunBrowser", script, expected)
			fact, payload := pilotCompletion(t, ctx, broker, 3)
			if fact.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_HISTORICAL_CLEAN ||
				fact.GetStatementId() != statement.StatementID || !fact.GetBusinessDate().AsTime().Equal(subject.BusinessDate) ||
				fact.GetRequestId() == "" || fact.GetRunId() != "manual:"+fact.GetRequestId() || fact.GetActor() != "custody-pilot-rerun-authorized" ||
				len(fact.GetBreaks()) != 0 || len(fact.GetHistoricalQuantityResolutions()) != 0 || fact.GetExecutionQuantities().GetCompared() != 1 {
				t.Fatal("historical completion lacks the exact dated comparison and operator identity")
			}
			retained[fact.GetRunId()] = payload
			expected["phase"], expected["run_id"] = "customer", fact.GetRunId()
			provePilotBrowser(t, ctx, site.URL, "TestSignedCustodyOperatorBrowser", script, expected)
			latest, err := store.LatestRun(ctx, subject.PortfolioID, subject.CustodianID)
			if err != nil || latest.RunID != expected["following_run"] || pilotHistoricalWorkingState(t, ctx, pool) != before {
				t.Fatal("historical comparison changed the later close or the live working state")
			}
			info, err := broker.completions.Info(ctx)
			if err != nil || info.State.Msgs != 3 {
				t.Fatal("historical replay or read did not preserve one completion")
			}
			for id, original := range retained {
				var actual []byte
				if err := pool.QueryRow(ctx, "SELECT manual_receipt FROM custody_runs WHERE run_id=$1", id).Scan(&actual); err != nil || !bytes.Equal(actual, original) {
					t.Fatalf("original broker completion changed in PostgreSQL: %v", err)
				}
			}
			delete(expected, "phase")
			capture, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(os.Getenv("CUSTODY_PILOT_RECEIPT"), capture, 0600); err != nil {
				t.Fatal(err)
			}
			t.Log("signed historical comparison preserved the later close and live queue through Chromium")
		})
}

func pilotHistoricalWorkingState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot []json.RawMessage
	if err := json.Unmarshal([]byte(pilotFinancialSnapshot(t, ctx, pool)), &snapshot); err != nil || len(snapshot) != 6 {
		t.Fatal("missing pilot working state")
	}
	// Rows for the queue, quantity observations, actions and ledger must all be
	// unchanged. Run and outbox additions are verified individually above.
	return string(bytes.Join([][]byte{snapshot[1], snapshot[2], snapshot[3], snapshot[4]}, []byte("\n")))
}
