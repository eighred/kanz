package main

import (
	"bytes"
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSignedBookedFeePilot(t *testing.T) {
	if os.Getenv("CUSTODY_PILOT_VERIFY") != "1" {
		t.Skip("requires a separately provisioned signed booked-fee pilot")
	}
	const tenant = "synthetic-custody-pilot"
	fixture := filepath.Join("testdata", "execution_economics")
	files := []pilotStatementFile{{name: "statement-r1", revision: 1, position: big.NewRat(21, 2), cash: big.NewRat(-43, 4), trade: true}}
	var original *orderpb.Fill
	appendFill := func(ctx context.Context, journal *ledger.Postgres, fill *orderpb.Fill) {
		t.Helper()
		entry, err := ledger.FromFill("PF1", fill, "USD", time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Append(ctx, entry, nil); err != nil {
			t.Fatal("book the fixture execution", err)
		}
	}
	beforeIntake := func(ctx context.Context, pool *pgxpool.Pool, _ pilotBroker, day time.Time) {
		original = pilotEconomicsFill(day)
		appendFill(bus.WithTenantID(ctx, tenant), ledger.NewPostgres(pool), original)
	}
	checkPilotSeries(t, 2*time.Minute, fixture, beforeIntake, files, func(ctx context.Context, pool *pgxpool.Pool, stmt custody.Statement, _ int, broker pilotBroker) {
		ctx = bus.WithTenantID(ctx, tenant)
		site, stop := startPilotAccounting(t, ctx, broker, "pilot-fee-original", stmt.BusinessDate)
		defer func() {
			if stop != nil {
				stop()
			}
		}()
		first, firstPayload := pilotCompletion(t, ctx, broker, 1)
		if first.GetStatementId() != stmt.StatementID || first.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_CLEAN ||
			len(first.GetExecutionEconomics().GetRows()) != 1 || first.GetExecutionEconomics().GetRows()[0].GetStatus() != "mismatch" {
			t.Fatal("original comparison lost balance agreement or execution economics discrepancy")
		}
		read := func(site, phase, run string) {
			t.Helper()
			provePilotBrowser(t, ctx, site, "TestSignedCustodyOperatorBrowser", os.Getenv("CUSTODY_PILOT_BROWSER_SCRIPT"), map[string]any{
				"phase": phase, "date": stmt.BusinessDate.Format(time.DateOnly), "statement_id": stmt.StatementID,
				"original_run": first.GetRunId(), "run_id": run,
			})
		}
		read(site, "original", first.GetRunId())

		revised := proto.Clone(original).(*orderpb.Fill)
		revised.Fee.Amount = &commonpb.Decimal{Coefficient: 50, Exponent: -2}
		previous, err := fillfact.EconomicDigest(original)
		if err != nil {
			t.Fatal(err)
		}
		proposal := &orderpb.ExecutionFeeCorrectionProposed{
			CaseId: "pilot-fee-correction", OrderId: original.OrderId, PortfolioId: stmt.PortfolioID, Proposer: "synthetic:maker",
			Changes: []*orderpb.ExecutionFeeChange{{Previous: original, Revised: revised}},
			Reason:  "synthetic commission correction",
		}
		digest, err := fillfact.FeeProposalDigest(tenant, proposal)
		if err != nil {
			t.Fatal(err)
		}
		revised.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: proposal.CaseId, FeeApproval: &orderpb.ExecutionFeeApproval{
			ProposalId: proposal.CaseId, Digest: digest, Proposer: proposal.Proposer, Approver: "synthetic:checker",
			ApprovedAt: timestamppb.Now(), PreviousExecutionDigest: previous, PreviousFee: proto.Clone(original.Fee).(*commonpb.Money),
		}}
		journal := ledger.NewPostgres(pool)
		appendFill(ctx, journal, revised)
		appendFill(ctx, journal, revised)
		read(site, "pending", "")
		stop()
		stop = nil

		site, stopCorrected := startPilotAccounting(t, ctx, broker, "pilot-fee-corrected", stmt.BusinessDate)
		defer stopCorrected()
		second, secondPayload := pilotCompletion(t, ctx, broker, 2)
		if second.GetRunId() == first.GetRunId() || second.GetStatementId() != stmt.StatementID ||
			second.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_BREAKS ||
			len(second.GetBreaks()) != 1 || second.GetBreaks()[0].GetKey() != "USD" ||
			len(second.GetExecutionEconomics().GetRows()) != 1 || second.GetExecutionEconomics().GetRows()[0].GetStatus() != "equal" {
			t.Fatal("new book prefix did not separate cash disagreement from agreeing execution figures")
		}
		awaitPilotStatementAck(t, ctx, broker, "pilot-fee-corrected", 1)
		read(site, "corrected", second.GetRunId())
		for id, want := range map[string][]byte{first.GetRunId(): firstPayload, second.GetRunId(): secondPayload} {
			var got []byte
			if err := pool.QueryRow(ctx, "SELECT manual_receipt FROM custody_runs WHERE run_id=$1", id).Scan(&got); err != nil || !bytes.Equal(want, got) {
				t.Fatal("recovery or browser read changed a retained completion", err)
			}
		}
		entries, err := journal.Journal(ctx, stmt.PortfolioID)
		if err != nil || len(entries) != 2 || entries[1].Type != ledger.EntryFee || entries[1].Cash == nil || entries[1].Cash.Cmp(big.NewRat(-1, 4)) != 0 {
			t.Fatal("fee retry changed booked quantity or posted multiple cash adjustments", err)
		}
		info, err := broker.statements.Info(ctx)
		if err != nil || info.State.Msgs != 1 {
			t.Fatal("book correction changed the retained signed statement inventory", err)
		}
		completions, err := broker.completions.Info(ctx)
		if err != nil || completions.State.Msgs != 2 {
			t.Fatal("book fee retry or receipt read added a comparison completion", err)
		}
		t.Log("signed Chromium verified unchanged-source booked fee correction and original receipt")
	})
}
