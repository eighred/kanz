package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/eighred/kanz/services/accounting/internal/server"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

type pilotBreak struct {
	InstrumentID string            `json:"instrument_id,omitempty"`
	BreakID      string            `json:"break_id"`
	PortfolioID  string            `json:"portfolio_id"`
	CustodianID  string            `json:"custodian_id"`
	Key          string            `json:"key"`
	Kind         string            `json:"kind"`
	IBOR         string            `json:"ibor"`
	Custodian    string            `json:"custodian"`
	Difference   string            `json:"difference"`
	Detection    custody.Detection `json:"detection"`
}

type pilotQueueExpectation struct {
	pilotBreak
	ExecutionQuantity *pilotBreak `json:"execution_quantity,omitempty"`
}

type pilotObservation struct {
	RetainedBreaks     int    `json:"retained_breaks"`
	Revision           int    `json:"revision"`
	RunID              string `json:"run_id"`
	StatementID        string `json:"statement_id"`
	BusinessDate       string `json:"business_date"`
	Outcome            string `json:"outcome"`
	OutstandingBreaks  int    `json:"outstanding_breaks"`
	CompletionSequence uint64 `json:"completion_sequence"`
	Actor              string `json:"actor,omitempty"`
	RequestID          string `json:"request_id,omitempty"`
}

// Use a fresh instance of the provisioned pilot database for each invocation.
func TestSignedCustodyOperatorReadback(t *testing.T) {
	if os.Getenv("CUSTODY_PILOT_LATE_QUANTITY") == "1" {
		checkLateQuantityPilot(t)
		return
	}
	checkPilotReadback(t, false)
}

func TestSignedCustodyAuthorizedRerun(t *testing.T) {
	checkPilotReadback(t, true)
}

func checkPilotReadback(t *testing.T, rerun bool) {
	t.Helper()
	if os.Getenv("CUSTODY_PILOT_VERIFY") != "1" {
		t.Skip("requires a fresh provisioned synthetic custody pilot")
	}
	var observations []pilotObservation
	var firstDetection pilotBreak
	quantityProfile := os.Getenv("CUSTODY_PILOT_QUANTITY_FIXTURE") == "1"
	checkSignedPilotCommand(t, 2*time.Minute, func(ctx context.Context, pool *pgxpool.Pool, statement custody.Statement, revision int, broker pilotBroker) {
		const tenant = "synthetic-custody-pilot"
		ctx = bus.WithTenantID(ctx, tenant)
		day := statement.BusinessDate
		journal := ledger.NewPostgres(pool)
		if revision == 1 {
			appendPilotTrade(t, ctx, journal, day)
		}
		subject := statement.Subject()
		store, scope, r := pilotComparison(t, ctx, pool, journal, subject, broker)
		run, err := r.Reconcile(ctx, subject)
		if err != nil || run.StatementID != statement.StatementID {
			t.Fatalf("actual comparison %s: %v", run.RunID, err)
		}
		sequence := uint64(revision)
		if rerun && revision == 2 {
			sequence++ // The browser's one manual completion follows scheduled r1.
		}
		message, err := broker.completions.GetMsg(ctx, sequence)
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		var fact accountingpb.ReconciliationRun
		if proto.Unmarshal(message.Data, &frame) != nil || proto.Unmarshal(frame.Payload, &fact) != nil || frame.GetEnvelope().GetTenantId() != tenant || fact.GetRunId() != run.RunID || fact.GetStatementId() != statement.StatementID {
			t.Fatal("completion FACT did not identify the committed comparison")
		}
		options := []server.Option{server.WithTenant(tenant), server.WithBreakStore(store), server.WithCustodyBookScope(scope)}
		if rerun {
			options = append(options, server.WithCustodyReruns("custody-pilot-rerun", r.ReconcileManual))
		} else {
			options = append(options, server.WithReadOnlyCustodyReview())
		}
		site := httptest.NewServer(server.New(&server.Readiness{}, slog.Default(), journal, "USD", options...))
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
		if list.ActionsEnabled == nil || *list.ActionsEnabled != rerun {
			t.Fatal("accounting reported a different review configuration")
		}
		wantRetained := 0
		if revision == 1 {
			wantRetained = 1
		}
		if len(run.Breaks) != wantRetained || len(fact.GetBreaks()) != wantRetained {
			t.Fatal("stored run and delivered completion diverged from the historical break contract")
		}
		observations = append(observations, pilotObservation{
			RetainedBreaks: len(fact.GetBreaks()), Revision: revision, RunID: fact.GetRunId(), StatementID: fact.GetStatementId(),
			BusinessDate: day.Format(time.DateOnly), Outcome: run.Outcome.String(),
			OutstandingBreaks: len(list.Breaks), CompletionSequence: message.Sequence,
		})
		if revision == 2 {
			if run.Outcome != custody.OutcomeClean || len(list.Breaks) != 0 {
				t.Fatal("corrected statement left an outstanding break")
			}
			if !rerun && os.Getenv("CUSTODY_PILOT_GATEWAY_PROBE") != "" {
				missing := subject
				missing.BusinessDate = day.AddDate(0, 0, 1)
				absent, err := r.Reconcile(ctx, missing)
				if err != nil || absent.Outcome != custody.OutcomeNoStatement || absent.StatementID != "" || absent.RunID == "" {
					t.Fatal("scheduled missing statement did not produce a distinct dated outcome")
				}
				baseline := pilotFinancialSnapshot(t, ctx, pool)
				provePilotBrowser(t, ctx, site.URL, "TestSignedCustodyOperatorBrowser", os.Getenv("CUSTODY_PILOT_DATED_SCRIPT"), map[string]any{
					"portfolio": subject.PortfolioID, "custodian": subject.CustodianID, "clean_date": day.Format(time.DateOnly),
					"clean_run": run.RunID, "statement": run.StatementID, "missing_date": missing.BusinessDate.Format(time.DateOnly),
					"missing_run": absent.RunID, "unobserved_date": day.AddDate(0, 0, 2).Format(time.DateOnly),
					"prior_detection": firstDetection, "quantity_evidence": quantityProfile,
				})
				info, err := broker.completions.Info(ctx)
				if err != nil || info.State.Msgs != 3 || baseline != pilotFinancialSnapshot(t, ctx, pool) {
					t.Fatal("dated read or denied operator action modified accounting or its completion stream")
				}
				t.Log("signed customer verified clean, missing and unobserved business dates")
				if script := os.Getenv("CUSTODY_PILOT_PREVIOUS_SCRIPT"); script != "" {
					posting := ledger.Event{EntryID: "pilot-late-cash", PortfolioID: subject.PortfolioID, VenueAccountID: "sandbox-account",
						Type: ledger.EntryCash, Effective: day.Add(time.Hour), Knowledge: time.Now().UTC(),
						Cash: big.NewRat(-1, 4), CashCurrency: "USD"}
					if err := journal.Append(ctx, &posting, nil); err != nil {
						t.Fatal("append same-date cash adjustment", err)
					}
					pending, err := store.DatedResult(ctx, subject)
					if err != nil || pending.State != "pending" || pending.Reason != "journal_changed" {
						t.Fatal("same-date cash adjustment did not invalidate the current comparison", pending.State, pending.Reason, err)
					}
					current, err := r.Reconcile(ctx, subject)
					if err != nil || current.StatementID != run.StatementID || current.Outcome != custody.OutcomeBreaks ||
						len(current.Breaks) != 1 || current.Breaks[0].Kind != recon.BreakCash || current.Breaks[0].Diff == nil ||
						current.Breaks[0].Diff.Cmp(big.NewRat(-1, 4)) != 0 {
						t.Fatal("same signed correction did not detect the new 0.25 USD cash difference", err)
					}
					fact, _ := pilotCompletion(t, ctx, broker, 4)
					if fact.GetRunId() != current.RunID {
						t.Fatal("the new comparison has no matching broker completion")
					}
					baseline := pilotFinancialSnapshot(t, ctx, pool)
					provePilotBrowser(t, ctx, site.URL, "TestSignedCustodyOperatorBrowser", script, map[string]any{
						"date": day.Format(time.DateOnly), "current_run": current.RunID, "current_statement": current.StatementID,
						"prior": firstDetection,
					})
					if baseline != pilotFinancialSnapshot(t, ctx, pool) {
						t.Fatal("opening the previous comparison modified the book or working queue")
					}
					t.Log("signed Chromium discovered the original receipt from a revised comparison with breaks")
				}
			}
			return
		}
		wantOpen := 1
		if quantityProfile {
			wantOpen++
		}
		if run.Outcome != custody.OutcomeBreaks || len(list.Breaks) != wantOpen {
			t.Fatalf("expected %d outstanding source-backed breaks, got %d", wantOpen, len(list.Breaks))
		}
		var b pilotBreak
		var quantity *pilotBreak
		for i := range list.Breaks {
			row := list.Breaks[i]
			switch row.Kind {
			case "quantity":
				b = row
			case "execution_quantity":
				quantity = &list.Breaks[i]
			default:
				t.Fatalf("unexpected queue kind %q", row.Kind)
			}
		}
		if b.BreakID == "" || (quantity != nil) != quantityProfile {
			t.Fatal("unexpected queue composition")
		}
		t.Run("rollback_action_fence", func(t *testing.T) {
			checkPilotRollback(t, ctx, pool, b.BreakID)
		})
		expected := custody.Detection{RunID: run.RunID, StatementID: statement.StatementID, BusinessDate: "2020-02-04", ScopeMode: custody.BasisDeclared, JournalPosition: "1"}
		if b.Detection != expected || b.PortfolioID != "PF1" || b.CustodianID != "CUST-A" || b.Key != "SYN" || b.IBOR != "10.5" || b.Custodian != "9.5" || b.Difference != "1" {
			t.Fatalf("unexpected accounting evidence: %+v", b)
		}
		if quantity != nil {
			stableID, _ := regexp.MatchString("^q1:[0-9a-f]{64}$", quantity.BreakID)
			if !stableID || quantity.Detection != expected || quantity.PortfolioID != b.PortfolioID || quantity.CustodianID != b.CustodianID || quantity.Key != "external-7" || quantity.InstrumentID != "SYN" || quantity.IBOR != "10.5" || quantity.Custodian != "9.5" || quantity.Difference != "1" {
				t.Fatalf("incorrect execution-quantity queue evidence: %+v", quantity)
			}
		}
		firstDetection = b
		if !rerun {
			before := pilotFinancialSnapshot(t, ctx, pool)
			const id = "c9cb9d52-3414-4081-bb01-718183d12647"
			result, err := r.ReconcileManual(ctx, subject, id, "synthetic-operator")
			var denied *pgconn.PgError
			if result.RunID != "" || !errors.As(err, &denied) || denied.Code != "42501" || !strings.Contains(denied.Message, "signed_pilot_read_only_reruns") {
				t.Fatalf("expected database refusal of operator rerun in the read-only pilot: %v", err)
			}
			for _, target := range list.Breaks {
				for _, action := range []string{"actions", "assign", "explain"} {
					probe := httptest.NewRequest(http.MethodPost, "/v1/custody/breaks/"+target.BreakID+"/"+action, nil)
					probe.Header = req.Header.Clone()
					rec := httptest.NewRecorder()
					site.Config.Handler.ServeHTTP(rec, probe)
					if rec.Code != http.StatusNotFound {
						t.Fatalf("read-only %s returned %d", action, rec.Code)
					}
				}
			}
			if before != pilotFinancialSnapshot(t, ctx, pool) {
				t.Fatal("denied queue actions changed financial evidence")
			}
		}
		if binary := os.Getenv("CUSTODY_PILOT_GATEWAY_PROBE"); binary != "" {
			testName := "TestSignedCustodyOperatorBrowser"
			if rerun {
				testName = "TestSignedCustodyRerunBrowser"
			}
			provePilotBrowser(t, ctx, site.URL, testName, os.Getenv("CUSTODY_PILOT_BROWSER_SCRIPT"), pilotQueueExpectation{b, quantity})
			t.Log("signed gateway and Chromium verified the persisted detection")
		}
		if rerun {
			info, err := broker.completions.Info(ctx)
			if err != nil || info.State.Msgs != 2 {
				t.Fatal("browser and its retry must add one completion")
			}
			msg, err := broker.completions.GetMsg(ctx, 2)
			var frame envelopepb.EventFrame
			var manual accountingpb.ReconciliationRun
			if err != nil || proto.Unmarshal(msg.Data, &frame) != nil || proto.Unmarshal(frame.Payload, &manual) != nil ||
				frame.GetEnvelope().GetTenantId() != tenant || manual.GetActor() != "custody-pilot-rerun-authorized" ||
				manual.GetRequestId() == "" || manual.GetRunId() != "manual:"+manual.GetRequestId() || manual.GetStatementId() != statement.StatementID ||
				manual.GetOutcome() != fact.GetOutcome() || !manual.GetBusinessDate().AsTime().Equal(day) {
				t.Fatal("manual broker completion must identify the authorized exact-date comparison")
			}
			stored, err := store.ManualReceipt(ctx, custody.Run{RunID: manual.GetRunId(), RequestID: manual.GetRequestId(), Actor: manual.GetActor(), Subject: subject})
			if err != nil || len(stored.Breaks) != 1 || stored.StatementID != statement.StatementID || stored.Outcome != custody.OutcomeBreaks {
				t.Fatal("manual completion differs from its durable receipt")
			}
			observations = append(observations, pilotObservation{RetainedBreaks: len(stored.Breaks), Revision: revision, RunID: stored.RunID, StatementID: stored.StatementID,
				BusinessDate: day.Format(time.DateOnly), Outcome: stored.Outcome.String(), OutstandingBreaks: len(stored.Breaks),
				CompletionSequence: msg.Sequence, Actor: stored.Actor, RequestID: stored.RequestID})
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

func appendPilotTrade(t *testing.T, ctx context.Context, journal ledger.Store, day time.Time) {
	t.Helper()
	trade := ledger.Event{EntryID: "pilot-trade-7", SourceRef: "external-7", PortfolioID: "PF1", VenueAccountID: "sandbox-account",
		Type: ledger.EntryTrade, InstrumentID: "SYN", Quantity: big.NewRat(21, 2), Price: big.NewRat(1, 1), Cash: big.NewRat(-21, 2),
		CashCurrency: "USD", Effective: day, Knowledge: day, SettlementBasis: ledger.SettlementSettled, SettlementDate: day}
	if err := journal.Append(ctx, &trade, nil); err != nil {
		t.Fatal(err)
	}
}

func pilotComparison(t *testing.T, ctx context.Context, pool *pgxpool.Pool, journal ledger.Store, subject custody.Subject, broker pilotBroker) (*custody.Postgres, *custody.BookScope, *custody.Reconciler) {
	t.Helper()
	const tenant = "synthetic-custody-pilot"
	scope, err := custody.NewBookScope([]custody.Subject{subject}, map[string]map[string][]string{"PF1": {"CUST-A": {"sandbox-account"}}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, broker.accounting)
	if err != nil {
		t.Fatal("pilot accounting broker unavailable")
	}
	t.Cleanup(func() { _ = client.Close() })
	publisher, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "pilot", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	store := custody.NewPostgres(pool)
	scope, err = scope.AdmitAssignments(ctx, time.Now().UTC(), store)
	if err != nil {
		t.Fatalf("independent custody account assignment was not admitted: %v", err)
	}
	r, err := custody.NewReconciler(store, custody.LedgerBookLoader(journal, scope, slog.Default()), publisher, new(big.Rat), nil, slog.Default(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return store, scope, r
}

func provePilotBrowser(t *testing.T, ctx context.Context, endpoint, testName, script string, expected any) {
	t.Helper()
	payload, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(ctx, os.Getenv("CUSTODY_PILOT_GATEWAY_PROBE"), "-test.run=^"+testName+"$", "-test.count=1", "-test.v")
	probe.Stdin = bytes.NewReader(payload)
	probe.Env = []string{"CUSTODY_PILOT_ACCOUNTING_URL=" + endpoint, "CUSTODY_PILOT_BROWSER_SCRIPT=" + script}
	for _, k := range []string{"PATH", "SystemRoot", "SystemDrive", "TEMP", "TMP", "TMPDIR", "LOCALAPPDATA", "USERPROFILE", "HOME", "PLAYWRIGHT_BROWSERS_PATH", "CUSTODY_PILOT_BFF_PROBE", "CUSTODY_PILOT_RETAINED_DOWNLOADS", "TEST_MFA_NODE", "TEST_MFA_PLAYWRIGHT", "TEST_MFA_STATIC_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			probe.Env = append(probe.Env, k+"="+v)
		}
	}
	log, err := probe.CombinedOutput()
	if err != nil || !bytes.Contains(log, []byte("--- PASS: "+testName)) {
		t.Fatalf("signed gateway/browser exercise failed: %v\n%s", err, log)
	}
}

func pilotFinancialSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	err := pool.QueryRow(ctx, `SELECT jsonb_build_array(
		(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.run_id) FROM custody_runs r),
		(SELECT jsonb_agg(to_jsonb(b) ORDER BY b.break_id) FROM custody_breaks b),
 (SELECT jsonb_agg(to_jsonb(q) ORDER BY q.observation) FROM custody_execution_quantity_observations q),
		(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.request_id) FROM custody_actions a),
		(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.entry_id) FROM ledger_entries e),
		(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.id) FROM outbox o))::text`).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
