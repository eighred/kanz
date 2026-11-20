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
	"strings"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/eighred/kanz/services/accounting/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func checkLateQuantityPilot(t *testing.T) {
	t.Helper()
	if os.Getenv("CUSTODY_PILOT_VERIFY") != "1" {
		t.Skip("requires a fresh provisioned synthetic custody pilot")
	}
	const tenant = "synthetic-custody-pilot"
	expected := map[string]string{
		"portfolio": "PF1", "custodian": "CUST-A", "tenant": tenant,
		"entry_id": "pilot-trade-7", "external_ref": "external-7", "instrument_id": "SYN", "agreed_quantity": "10.5",
	}
	sequence := []pilotStatementFile{
		{name: "statement-r1", revision: 1, position: big.NewRat(19, 2), trade: true},
		{name: "following-r1", revision: 1, dayOffset: 1, position: big.NewRat(21, 2)},
		{name: "statement-r2", revision: 2, position: big.NewRat(21, 2), trade: true},
	}
	sealed := make(map[string][]byte)
	outcomes := []string{"breaks", "clean", "historical_quantity_correction"}
	checkPilotSeries(t, 2*time.Minute, filepath.Join("testdata", "late_quantity"), false, sequence, func(ctx context.Context, pool *pgxpool.Pool, stmt custody.Statement, stage int, broker pilotBroker) {
		ctx = bus.WithTenantID(ctx, tenant)
		subject := stmt.Subject()
		journal := ledger.NewPostgres(pool)
		if stage == 1 {
			appendPilotTrade(t, ctx, journal, subject.BusinessDate)
		}
		store, scope, r := pilotComparison(t, ctx, pool, journal, subject, broker)
		run, err := r.Reconcile(ctx, subject)
		if err != nil || run.StatementID != stmt.StatementID || run.Outcome.String() != outcomes[stage-1] {
			t.Fatalf("signed stage %d comparison: outcome=%v error=%v", stage, run.Outcome, err)
		}
		message, err := broker.completions.GetMsg(ctx, uint64(stage))
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		var fact accountingpb.ReconciliationRun
		if proto.Unmarshal(message.Data, &frame) != nil || proto.Unmarshal(frame.Payload, &fact) != nil ||
			frame.GetEnvelope().GetTenantId() != tenant || frame.GetEnvelope().GetSource() != "accounting" ||
			frame.GetEnvelope().GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || message.Subject != custody.SubjectRun ||
			fact.GetRunId() != run.RunID || fact.GetStatementId() != stmt.StatementID ||
			!fact.GetBusinessDate().AsTime().Equal(subject.BusinessDate) || fact.GetOutcome().String() != "RECONCILIATION_OUTCOME_"+strings.ToUpper(outcomes[stage-1]) {
			t.Fatalf("completion FACT differs from signed stage %d", stage)
		}
		sealed[run.RunID] = frame.Payload
		for id, published := range sealed {
			var retained []byte
			if err := pool.QueryRow(ctx, "SELECT manual_receipt FROM custody_runs WHERE run_id=$1", id).Scan(&retained); err != nil || !bytes.Equal(retained, published) {
				t.Fatalf("retained completion changed or differs from its FACT at stage %d: %v", stage, err)
			}
		}
		items, err := store.OutstandingBreaks(ctx, subject.PortfolioID)
		if err != nil {
			t.Fatal(err)
		}
		switch stage {
		case 1:
			expected["original_run"], expected["original_statement"] = run.RunID, stmt.StatementID
			expected["business_date"] = subject.BusinessDate.Format(time.DateOnly)
			for _, item := range items {
				if item.Kind == recon.BreakExecutionQuantity && item.Detection.RunID == run.RunID {
					expected["break_id"] = item.BreakID
				}
			}
			if len(items) != 2 || len(fact.GetBreaks()) != 1 || expected["break_id"] == "" {
				t.Fatal("original comparison lacks its position or execution discrepancy")
			}
		case 2:
			expected["following_run"], expected["following_statement"] = run.RunID, stmt.StatementID
			expected["following_date"] = subject.BusinessDate.Format(time.DateOnly)
			if len(items) != 1 || items[0].BreakID != expected["break_id"] || items[0].Detection.RunID != expected["original_run"] {
				t.Fatal("the next day's comparison changed the earlier execution discrepancy")
			}
		case 3:
			if len(items) != 0 || len(sealed) != 3 {
				t.Fatal("late correction did not clear exactly the remaining execution discrepancy")
			}
			expected["correction_run"], expected["corrected_statement"] = run.RunID, stmt.StatementID
			before := pilotFinancialSnapshot(t, ctx, pool)
			site := httptest.NewServer(server.New(&server.Readiness{}, slog.Default(), journal, "USD",
				server.WithTenant(tenant), server.WithBreakStore(store), server.WithCustodyBookScope(scope), server.WithReadOnlyCustodyReview()))
			defer site.Close()
			provePilotBrowser(t, ctx, site.URL, "TestSignedCustodyOperatorBrowser", os.Getenv("CUSTODY_PILOT_BROWSER_SCRIPT"), expected)
			info, err := broker.completions.Info(ctx)
			if err != nil || info.State.Msgs != 3 || before != pilotFinancialSnapshot(t, ctx, pool) {
				t.Fatal("review of the correction changed accounting or its completion stream")
			}
			data, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(os.Getenv("CUSTODY_PILOT_RECEIPT"), data, 0600); err != nil {
				t.Fatal(err)
			}
			t.Log("signed customer recovered the applied historical quantity correction")
		}
	})
}
