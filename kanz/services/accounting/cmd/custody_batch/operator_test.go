package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

type pilotBreak struct {
	BreakID     string            `json:"break_id"`
	PortfolioID string            `json:"portfolio_id"`
	CustodianID string            `json:"custodian_id"`
	Key         string            `json:"key"`
	IBOR        string            `json:"ibor"`
	Custodian   string            `json:"custodian"`
	Difference  string            `json:"difference"`
	Detection   custody.Detection `json:"detection"`
}

type pilotObservation struct {
	Revision           int    `json:"revision"`
	RunID              string `json:"run_id"`
	StatementID        string `json:"statement_id"`
	BusinessDate       string `json:"business_date"`
	Outcome            string `json:"outcome"`
	OutstandingBreaks  int    `json:"outstanding_breaks"`
	CompletionSequence uint64 `json:"completion_sequence"`
}

// Use a fresh instance of the provisioned pilot database for each invocation.
func TestSignedCustodyOperatorReadback(t *testing.T) {
	if os.Getenv("CUSTODY_PILOT_VERIFY") != "1" {
		t.Skip("requires a fresh provisioned synthetic custody pilot")
	}
	var observations []pilotObservation
	checkSignedPilotCommand(t, 2*time.Minute, func(ctx context.Context, pool *pgxpool.Pool, statement custody.Statement, revision int, broker pilotBroker) {
		const tenant = "synthetic-custody-pilot"
		ctx = bus.WithTenantID(ctx, tenant)
		day := statement.BusinessDate
		journal := ledger.NewPostgres(pool)
		if revision == 1 {
			trade := ledger.Event{EntryID: "pilot-trade-7", SourceRef: "external-7", PortfolioID: "PF1", VenueAccountID: "sandbox-account",
				Type: ledger.EntryTrade, InstrumentID: "SYN", Quantity: big.NewRat(21, 2), Price: big.NewRat(1, 1), Cash: big.NewRat(-21, 2),
				CashCurrency: "USD", Effective: day, Knowledge: day, SettlementBasis: ledger.SettlementSettled, SettlementDate: day}
			if err := journal.Append(ctx, &trade, nil); err != nil {
				t.Fatal(err)
			}
		}
		subject := statement.Subject()
		scope, err := custody.NewBookScope([]custody.Subject{subject}, map[string]map[string][]string{"PF1": {"CUST-A": {"sandbox-account"}}})
		if err != nil {
			t.Fatal(err)
		}
		client, err := bus.DialNATS(ctx, broker.accounting)
		if err != nil {
			t.Fatal("pilot accounting broker unavailable")
		}
		defer client.Close()
		publisher, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "pilot", Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		store := custody.NewPostgres(pool)
		r, err := custody.NewReconciler(store, custody.LedgerBookLoader(journal, scope, slog.Default()), publisher, new(big.Rat), nil, slog.Default(), time.Now)
		if err != nil {
			t.Fatal(err)
		}
		run, err := r.Reconcile(ctx, subject)
		if err != nil || run.StatementID != statement.StatementID {
			t.Fatalf("actual comparison %s: %v", run.RunID, err)
		}
		message, err := broker.completions.GetMsg(ctx, uint64(revision))
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		var fact accountingpb.ReconciliationRun
		if proto.Unmarshal(message.Data, &frame) != nil || proto.Unmarshal(frame.Payload, &fact) != nil || frame.GetEnvelope().GetTenantId() != tenant || fact.GetRunId() != run.RunID || fact.GetStatementId() != statement.StatementID {
			t.Fatal("completion FACT did not identify the committed comparison")
		}
		site := httptest.NewServer(server.New(&server.Readiness{}, slog.Default(), journal, "USD", server.WithTenant(tenant), server.WithBreakStore(store), server.WithReadOnlyCustodyReview()))
		defer site.Close()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.URL+"/v1/custody/breaks", nil)
		if err != nil {
			t.Fatal(err)
		}
		auth.SetPrincipalHeaders(req.Header, "synthetic-pilot-reader", tenant, []string{"fund"})
		if err := auth.SetPrincipalPortfolios(req.Header, []string{"PF1"}); err != nil {
			t.Fatal(err)
		}
		response, err := site.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var list struct {
			Breaks         []pilotBreak `json:"breaks"`
			ActionsEnabled *bool        `json:"actions_enabled"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&list) != nil {
			t.Fatalf("accounting read failed: %d", response.StatusCode)
		}
		if list.ActionsEnabled == nil || *list.ActionsEnabled {
			t.Fatal("read-only pilot offered an operator action")
		}
		observations = append(observations, pilotObservation{
			Revision: revision, RunID: fact.GetRunId(), StatementID: fact.GetStatementId(),
			BusinessDate: day.Format(time.DateOnly), Outcome: run.Outcome.String(),
			OutstandingBreaks: len(list.Breaks), CompletionSequence: message.Sequence,
		})
		if revision == 2 {
			if run.Outcome != custody.OutcomeClean || len(list.Breaks) != 0 {
				t.Fatal("corrected statement left an outstanding break")
			}
			return
		}
		if run.Outcome != custody.OutcomeBreaks || len(list.Breaks) != 1 {
			t.Fatalf("expected one source-backed break, got %d", len(list.Breaks))
		}
		b := list.Breaks[0]
		t.Run("rollback_action_fence", func(t *testing.T) {
			checkPilotRollback(t, ctx, pool, b.BreakID)
		})
		expected := custody.Detection{RunID: run.RunID, StatementID: statement.StatementID, BusinessDate: "2020-02-04", ScopeMode: custody.BasisDeclared, JournalPosition: "1"}
		if b.Detection != expected || b.PortfolioID != "PF1" || b.CustodianID != "CUST-A" || b.Key != "SYN" || b.IBOR != "10.5" || b.Custodian != "9.5" || b.Difference != "1" {
			t.Fatalf("unexpected accounting evidence: %+v", b)
		}
		for _, action := range []string{"actions", "assign", "explain"} {
			probe := httptest.NewRequest(http.MethodPost, "/v1/custody/breaks/test/"+action, nil)
			probe.Header = req.Header.Clone()
			rec := httptest.NewRecorder()
			site.Config.Handler.ServeHTTP(rec, probe)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("read-only %s returned %d", action, rec.Code)
			}
		}
		if binary := os.Getenv("CUSTODY_PILOT_GATEWAY_PROBE"); binary != "" {
			payload, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			probe := exec.CommandContext(ctx, binary, "-test.run=^TestSignedCustodyOperatorBrowser$", "-test.count=1", "-test.v")
			probe.Stdin = bytes.NewReader(payload)
			probe.Env = []string{"CUSTODY_PILOT_ACCOUNTING_URL=" + site.URL}
			for _, k := range []string{"PATH", "SystemRoot", "TEMP", "TMP", "TMPDIR", "LOCALAPPDATA", "USERPROFILE", "HOME", "PLAYWRIGHT_BROWSERS_PATH", "CUSTODY_PILOT_BFF_PROBE", "TEST_MFA_NODE", "TEST_MFA_PLAYWRIGHT", "TEST_MFA_STATIC_DIR", "CUSTODY_PILOT_BROWSER_SCRIPT"} {
				if v, ok := os.LookupEnv(k); ok {
					probe.Env = append(probe.Env, k+"="+v)
				}
			}
			log, err := probe.CombinedOutput()
			if err != nil || !bytes.Contains(log, []byte("--- PASS: TestSignedCustodyOperatorBrowser")) {
				t.Fatalf("signed gateway/browser exercise failed: %v\n%s", err, log)
			}
			t.Log("signed gateway and Chromium verified the persisted detection")
		}
	})
	if path := os.Getenv("CUSTODY_PILOT_RECEIPT"); path != "" && !t.Failed() {
		raw, err := json.Marshal(observations)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
