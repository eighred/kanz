package main

import (
	"bytes"
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSignedExecutionEconomicsPilot(t *testing.T) {
	if os.Getenv("CUSTODY_PILOT_VERIFY") != "1" {
		t.Skip("requires a separately provisioned synthetic custody pilot")
	}
	const tenant = "synthetic-custody-pilot"
	var files []pilotStatementFile
	for i, name := range []string{"statement-r1", "statement-r2", "statement-r3", "statement-r4"} {
		files = append(files, pilotStatementFile{name: name, revision: i + 1, position: big.NewRat(21, 2), cash: big.NewRat(-43, 4), trade: true})
	}
	var originalRun, missingRun, missingSite string
	var stopMissing func()
	retained := make(map[string][]byte)
	script := os.Getenv("CUSTODY_PILOT_BROWSER_SCRIPT")
	beforeIntake := func(ctx context.Context, pool *pgxpool.Pool, broker pilotBroker, day time.Time) {
		ctx = bus.WithTenantID(ctx, tenant)
		journal := ledger.NewPostgres(pool)
		// This independently booked fill supplies the actual commission and
		// settlement evidence. Only the shared external reference joins it to the statement.
		fill := pilotEconomicsFill(day)
		trade, err := ledger.FromFill("PF1", fill, "USD", day)
		if err != nil {
			t.Fatal(err)
		}
		if err = journal.Append(ctx, trade, nil); err != nil {
			t.Fatal(err)
		}
		missingSite, stopMissing = startPilotAccounting(t, ctx, broker, "pilot-missing", day)
		fact, payload := pilotCompletion(t, ctx, broker, 1)
		if fact.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_NO_STATEMENT || fact.GetStatementId() != "" || len(fact.GetBreaks()) != 0 {
			t.Fatal("initial scheduled close did not record missing statement")
		}
		missingRun = fact.GetRunId()
		retained[missingRun] = payload
		provePilotBrowser(t, ctx, missingSite, "TestSignedCustodyOperatorBrowser", script,
			map[string]any{"phase": "missing", "date": day.Format(time.DateOnly), "missing_run": missingRun})
		t.Log("scheduled missing statement displayed and downloaded through Chromium")
	}
	t.Cleanup(func() {
		if stopMissing != nil {
			stopMissing()
		}
	})
	checkPilotSeries(t, 2*time.Minute, filepath.Join("testdata", "execution_economics"), beforeIntake, files, func(ctx context.Context, pool *pgxpool.Pool, stmt custody.Statement, revision int, broker pilotBroker) {
		ctx = bus.WithTenantID(ctx, tenant)
		subject := stmt.Subject()
		if revision == 1 {
			provePilotBrowser(t, ctx, missingSite, "TestSignedCustodyOperatorBrowser", script,
				map[string]any{"phase": "pending", "date": subject.BusinessDate.Format(time.DateOnly), "statement_id": stmt.StatementID})
			stopMissing()
			stopMissing = nil
		}
		site, stop := startPilotAccounting(t, ctx, broker, "pilot-ingest", subject.BusinessDate)
		defer stop()
		completionSequence := uint64(revision + 1)
		fact, payload := pilotCompletion(t, ctx, broker, completionSequence)
		if fact.GetStatementId() != stmt.StatementID ||
			fact.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_CLEAN {
			t.Fatal("late signed close did not produce the expected compared outcome")
		}
		runID := fact.GetRunId()
		if revision == 1 {
			originalRun = runID
		}
		retained[runID] = payload
		awaitPilot(t, ctx, "persisted completion publication", func(check context.Context) bool {
			var sent bool
			err := pool.QueryRow(check, `SELECT EXISTS(SELECT FROM outbox WHERE producer_service='accounting' AND event_type=$1 AND correlation_id=$2 AND published_at IS NOT NULL)`, custody.SubjectRun, runID).Scan(&sent)
			return err == nil && sent
		})
		before := pilotFinancialSnapshot(t, ctx, pool)
		if revision == len(files) {
			// A fresh durable starts at the beginning of the retained statement
			// stream. It must ACK all four revisions without replacing receipts.
			_, stopReplay := startPilotAccounting(t, ctx, broker, "pilot-replay", time.Time{})
			defer stopReplay()
			awaitPilotStatementAck(t, ctx, broker, "pilot-replay", uint64(revision))
		}
		provePilotBrowser(t, ctx, site, "TestSignedCustodyOperatorBrowser", script, map[string]any{"revision": revision, "run_id": runID, "statement_id": stmt.StatementID, "original_run": originalRun, "missing_run": missingRun, "date": subject.BusinessDate.Format(time.DateOnly)})
		for id, payload := range retained {
			var data []byte
			if err := pool.QueryRow(ctx, "SELECT manual_receipt FROM custody_runs WHERE run_id=$1", id).Scan(&data); err != nil || !bytes.Equal(data, payload) {
				t.Fatalf("saved completion diverged on revision %d", revision)
			}
		}
		info, err := broker.completions.Info(ctx)
		if err != nil || info.State.Msgs != completionSequence || before != pilotFinancialSnapshot(t, ctx, pool) {
			t.Fatal("replay or read changed financial state or completion count")
		}
		t.Logf("signed economics revision %d retained and reopened through Chromium", revision)
	})
}

func pilotCompletion(t *testing.T, ctx context.Context, broker pilotBroker, sequence uint64) (*accountingpb.ReconciliationRun, []byte) {
	t.Helper()
	awaitPilot(t, ctx, "scheduled completion", func(check context.Context) bool {
		info, err := broker.completions.Info(check)
		return err == nil && info.State.Msgs >= sequence
	})
	msg, err := broker.completions.GetMsg(ctx, sequence)
	if err != nil {
		t.Fatal(err)
	}
	var frame envelopepb.EventFrame
	var fact accountingpb.ReconciliationRun
	if proto.Unmarshal(msg.Data, &frame) != nil || proto.Unmarshal(frame.Payload, &fact) != nil ||
		frame.GetEnvelope().GetSource() != "accounting" || frame.GetEnvelope().GetTenantId() != "synthetic-custody-pilot" || fact.GetRunId() == "" {
		t.Fatal("unexpected completion identity")
	}
	return &fact, frame.Payload
}

func pilotEconomicsFill(day time.Time) *orderpb.Fill {
	return &orderpb.Fill{FillId: "external-7", OrderId: "synthetic-order-7", Venue: "XSIM", VenueAccountId: "sandbox-account", VenueExecutionId: "7", InstrumentId: "SYN", Side: orderpb.Side_SIDE_BUY,
		Quantity: &commonpb.Decimal{Coefficient: 105, Exponent: -1}, Price: &commonpb.Decimal{Coefficient: 1},
		Fee: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 25, Exponent: -2}, CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(day)}
}
