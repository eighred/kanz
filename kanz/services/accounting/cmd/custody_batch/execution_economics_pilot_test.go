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
	var originalRun string
	retained := make(map[string][]byte)
	checkPilotSeries(t, 2*time.Minute, filepath.Join("testdata", "execution_economics"), true, files, func(ctx context.Context, pool *pgxpool.Pool, stmt custody.Statement, revision int, broker pilotBroker) {
		ctx = bus.WithTenantID(ctx, tenant)
		subject := stmt.Subject()
		journal := ledger.NewPostgres(pool)
		if revision == 1 {
			// This independently booked fill supplies the actual commission and
			// settlement evidence. Only the shared external reference joins it to the statement.
			fill := &orderpb.Fill{FillId: "external-7", OrderId: "synthetic-order-7", Venue: "XSIM", VenueAccountId: "sandbox-account", VenueExecutionId: "7", InstrumentId: "SYN", Side: orderpb.Side_SIDE_BUY,
				Quantity: &commonpb.Decimal{Coefficient: 105, Exponent: -1}, Price: &commonpb.Decimal{Coefficient: 1},
				Fee: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 25, Exponent: -2}, CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(subject.BusinessDate)}
			trade, err := ledger.FromFill("PF1", fill, "USD", subject.BusinessDate)
			if err != nil {
				t.Fatal(err)
			}
			if err = journal.Append(ctx, trade, nil); err != nil {
				t.Fatal(err)
			}
		}
		site, stop := startPilotAccounting(t, ctx, broker, "pilot-ingest", subject.BusinessDate)
		defer stop()
		awaitPilot(t, ctx, "scheduled completion", func(check context.Context) bool {
			info, err := broker.completions.Info(check)
			return err == nil && info.State.Msgs >= uint64(revision)
		})
		msg, err := broker.completions.GetMsg(ctx, uint64(revision))
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		var fact accountingpb.ReconciliationRun
		if proto.Unmarshal(msg.Data, &frame) != nil || proto.Unmarshal(frame.Payload, &fact) != nil || frame.GetEnvelope().GetSource() != "accounting" || frame.GetEnvelope().GetTenantId() != tenant || fact.GetRunId() == "" || fact.GetStatementId() != stmt.StatementID {
			t.Fatal("unexpected completion identity")
		}
		runID := fact.GetRunId()
		if revision == 1 {
			originalRun = runID
		}
		retained[runID] = frame.Payload
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
		provePilotBrowser(t, ctx, site, "TestSignedCustodyOperatorBrowser", os.Getenv("CUSTODY_PILOT_BROWSER_SCRIPT"), map[string]any{"revision": revision, "run_id": runID, "statement_id": stmt.StatementID, "original_run": originalRun, "date": subject.BusinessDate.Format(time.DateOnly)})
		for id, payload := range retained {
			var data []byte
			if err := pool.QueryRow(ctx, "SELECT manual_receipt FROM custody_runs WHERE run_id=$1", id).Scan(&data); err != nil || !bytes.Equal(data, payload) {
				t.Fatalf("saved completion diverged on revision %d", revision)
			}
		}
		info, err := broker.completions.Info(ctx)
		if err != nil || info.State.Msgs != uint64(revision) || before != pilotFinancialSnapshot(t, ctx, pool) {
			t.Fatal("replay or read changed financial state or completion count")
		}
		t.Logf("signed economics revision %d retained and reopened through Chromium", revision)
	})
}
