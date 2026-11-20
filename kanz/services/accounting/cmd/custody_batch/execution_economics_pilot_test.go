package main

import (
	"bytes"
	"context"
	"log/slog"
	"math/big"
	"net/http/httptest"
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
	"github.com/eighred/kanz/services/accounting/internal/server"
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
	checkPilotSeries(t, 2*time.Minute, filepath.Join("testdata", "execution_economics"), files, func(ctx context.Context, pool *pgxpool.Pool, stmt custody.Statement, revision int, broker pilotBroker) {
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
		store, scope, reconciler := pilotComparison(t, ctx, pool, journal, subject, broker)
		run, err := reconciler.Reconcile(ctx, subject)
		if err != nil || run.StatementID != stmt.StatementID {
			t.Fatalf("comparison %d: %v", revision, err)
		}
		if revision == 1 {
			originalRun = run.RunID
		}
		msg, err := broker.completions.GetMsg(ctx, uint64(revision))
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		var fact accountingpb.ReconciliationRun
		if proto.Unmarshal(msg.Data, &frame) != nil || proto.Unmarshal(frame.Payload, &fact) != nil || frame.GetEnvelope().GetSource() != "accounting" || frame.GetEnvelope().GetTenantId() != tenant || fact.GetRunId() != run.RunID || fact.GetStatementId() != stmt.StatementID {
			t.Fatal("unexpected completion identity")
		}
		retained[run.RunID] = frame.Payload
		before := pilotFinancialSnapshot(t, ctx, pool)
		// Re-deliver the same accepted source FACT to a newly constructed consumer.
		// A fresh scheduled invocation is a different request and has its own run ID.
		restarted, err := custody.NewSignedBatchConsumer(custody.NewPostgres(pool), tenant, nil)
		if err != nil {
			t.Fatal(err)
		}
		source, err := broker.statements.GetMsg(ctx, uint64(revision))
		if err != nil {
			t.Fatal(err)
		}
		var observed envelopepb.EventFrame
		if err = proto.Unmarshal(source.Data, &observed); err != nil {
			t.Fatal(err)
		}
		if err = restarted.Handle(ctx, observed.Envelope, observed.Payload); err != nil {
			t.Fatal(err)
		}
		site := httptest.NewServer(server.New(&server.Readiness{}, slog.Default(), journal, "USD", server.WithTenant(tenant), server.WithBreakStore(store), server.WithCustodyBookScope(scope), server.WithReadOnlyCustodyReview()))
		defer site.Close()
		provePilotBrowser(t, ctx, site.URL, "TestSignedCustodyOperatorBrowser", os.Getenv("CUSTODY_PILOT_BROWSER_SCRIPT"), map[string]any{"revision": revision, "run_id": run.RunID, "statement_id": stmt.StatementID, "original_run": originalRun, "date": subject.BusinessDate.Format(time.DateOnly)})
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
