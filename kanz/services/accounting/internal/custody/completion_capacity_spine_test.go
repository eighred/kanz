package custody

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

type observedCapacityProducer struct {
	*bus.Producer
	refusedBody int
}

func (p *observedCapacityProducer) PreflightNATSFact(ctx context.Context, event bus.Event) error {
	err := p.Producer.PreflightNATSFact(ctx, event)
	if errors.Is(err, bus.ErrPublishFrameTooLarge) {
		p.refusedBody = proto.Size(event.Payload)
	}
	return err
}

// The completion stream runs on private loopback with server max_payload=4096.
// All actual sends here are ordinary small financial FACTs.
func TestSignedCustodyCompletionFitsNegotiatedNATSPayload(t *testing.T) {
	const tenant, account = "synthetic-custody-pilot", "sandbox-account"
	const maxPayload = 4096
	ctx := bus.WithTenantID(t.Context(), tenant)
	pool, _, venue, governance := assignmentTestPools(t, tenant)
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir(), MaxPayload: maxPayload})
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("private JetStream server did not start")
	}
	url := srv.ClientURL()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if nc.MaxPayload() != maxPayload {
		t.Fatalf("negotiated server MaxPayload=%d want %d", nc.MaxPayload(), maxPayload)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ACCOUNTING", Subjects: []string{SubjectRun}, Storage: jetstream.FileStorage})
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.Config.MaxMsgSize > 0 && info.Config.MaxMsgSize < maxPayload {
		t.Fatal("fixture stream must admit any frame within the server budget", err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "custody-small-broker"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "fixture", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}

	enroll := signedAccountFixture(t, pool, ctx)
	if err := enroll(venue, "venue_identity", nil); err != nil {
		t.Fatal(err)
	}
	if err := enroll(governance, "custody_assignment", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO custody_assignment_readers VALUES(session_user,$1)", tenant); err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	sub := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: BusinessDay(t0)}
	scope, err := NewBookScope([]Subject{sub}, map[string]map[string][]string{"PF1": {"CUST-A": {account}}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err = scope.AdmitAssignments(ctx, sub.BusinessDate, store)
	if err != nil {
		t.Fatal(err)
	}
	// Fix the fractional second: its protobuf width occurs on each break.
	now := t0.AddDate(0, 0, 3).Add(567890 * time.Microsecond)
	reg := prometheus.NewRegistry()
	observed := &observedCapacityProducer{Producer: producer}
	load := LedgerBookLoader(ledger.NewPostgres(pool), scope, testLogger())
	metrics := NewMetrics(reg)
	writer, err := NewReconciler(store, load, observed, nil, metrics, testLogger(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	// The ledger is empty. Each independent signed position detects one ordinary
	// missing instrument. The statement importer retains the exact signed bytes
	// in PostgreSQL. Only the Accounting completion path uses this small broker.
	instruments := make([]string, 39)
	for i := range instruments {
		instruments[i] = fmt.Sprintf("F%02d", i)
	}
	retain := retainedStatementFeedForAccount(t, ctx, store, account, instruments...)
	deliver := func(ids []string) string {
		t.Helper()
		rows := make([]string, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, fmt.Sprintf(`{"instrumentId":%q,"quantity":{"coefficient":"1"}}`, id))
		}
		level := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":%q,"positions":[%s],"cash":[],"transactions":[],"grain":"STATEMENT_GRAIN_BALANCES_ONLY"}`, sub.BusinessDate.Format(time.RFC3339), strings.Join(rows, ",")))
		if len(level) >= maxPayload {
			t.Fatal("source fixture unexpectedly large", len(level))
		}
		id, apply := retain(level)
		apply()
		return id
	}
	retained := func(run Run) (uint64, []byte) {
		t.Helper()
		m, err := stream.GetLastMsgForSubject(ctx, SubjectRun)
		if err != nil {
			t.Fatal(err)
		}
		frame := &nats.Msg{Subject: m.Subject, Header: m.Header, Data: m.Data}
		size := frame.Size() - len(frame.Subject)
		env, payload, err := bus.Unframe(m.Data)
		if err != nil || bus.Validate(env) != nil || env.GetTenantId() != tenant || env.GetCorrelationId() != run.RunID || env.GetCausationId() != run.RequestID || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetEventType() != SubjectRun || size > maxPayload {
			t.Fatal("retained completion frame", run.RunID, size, err)
		}
		var saved, out []byte
		if err := pool.QueryRow(ctx, `SELECT r.manual_receipt,o.payload FROM custody_runs r JOIN outbox o ON o.tenant_id=r.tenant_id AND o.correlation_id=r.run_id
     WHERE r.run_id=$1 AND o.event_type=$2 AND o.published_at IS NOT NULL`, run.RunID, SubjectRun).Scan(&saved, &out); err != nil || !bytes.Equal(saved, payload) || !bytes.Equal(out, payload) {
			t.Fatal("stored/outbox/broker completion disagree", err)
		}
		got, err := runFromRecord(payload)
		if err != nil || got.RunID != run.RunID || got.Outcome != run.Outcome || len(got.Breaks) != len(run.Breaks) || got.FailureReason != run.FailureReason {
			t.Fatal("completion outcome", got, err)
		}
		t.Logf("%s completion broker bytes=%d domain bytes=%d", run.Outcome, size, len(payload))
		return m.Sequence, payload
	}

	first := deliver([]string{"AAPL"})
	prior, err := writer.Reconcile(ctx, sub)
	if err != nil || prior.Outcome != OutcomeBreaks || prior.StatementID != first || len(prior.Breaks) != 1 {
		t.Fatal("first valid comparison", prior, err)
	}
	seq, priorBytes := retained(prior)
	original := prior.Breaks[0]
	revision := deliver(instruments)
	now = now.Add(time.Minute)
	failed, err := writer.Reconcile(ctx, sub)
	if !errors.Is(err, bus.ErrPublishFrameTooLarge) || errors.Is(err, ErrPublicationPending) || failed.Outcome != OutcomeFailed || len(failed.Breaks) != 0 {
		t.Fatalf("incompatible comparison got %s with %d breaks: %v", failed.Outcome, len(failed.Breaks), err)
	}
	if observed.refusedBody <= 0 || observed.refusedBody >= maxPayload {
		t.Fatalf("fixture must fit as a bare domain payload: %d bytes, server limit %d", observed.refusedBody, maxPayload)
	}
	t.Logf("preflight refused the framed comparison, domain bytes=%d: %v", observed.refusedBody, err)
	after, _ := retained(failed)
	if after != seq+1 {
		t.Fatal("expected only the compact failed FACT", seq, after)
	}
	open, err := store.OutstandingBreaks(ctx, sub.PortfolioID)
	if err != nil || len(open) != 1 || open[0].BreakID != original.BreakID || open[0].Revision != original.Revision || open[0].Detection.RunID != prior.RunID || !open[0].LastSeenAt.Equal(original.LastSeenAt) || open[0].Status != BreakOpen {
		t.Fatal("working discrepancy changed", open, err)
	}
	value, ok := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": sub.PortfolioID, "custodian": sub.CustodianID})
	if !ok || value != float64(prior.CompletedAt.Unix()) {
		t.Fatal("failed comparison advanced freshness", value)
	}
	saved, err := store.HistoricalReceipt(ctx, sub, account, prior.RunID)
	if err != nil || saved.State != "recorded" || !saved.AccountAssignmentVerified || saved.StatementID != first || len(saved.Run.Breaks) != 1 || saved.Run.Breaks[0].BreakID != original.BreakID {
		t.Fatal("original retained receipt", saved.State, err)
	}
	current, err := store.DatedResult(ctx, sub)
	if err != nil || current.State != "pending" || current.StatementID != revision {
		t.Fatal("current signed revision", current.State, err)
	}

	now = now.Add(time.Minute)
	key := uuid.NewString()
	manual, err := writer.ReconcileManual(ctx, sub, key, "operator:small-close")
	if !errors.Is(err, bus.ErrPublishFrameTooLarge) || errors.Is(err, ErrPublicationPending) || manual.Outcome != OutcomeFailed || manual.RequestID != key || manual.StatementID != revision {
		t.Fatal("manual refusal", manual, err)
	}
	manualSeq, _ := retained(manual)
	replay, err := writer.ReconcileManual(ctx, sub, key, "operator:small-close")
	if !errors.Is(err, bus.ErrPublishFrameTooLarge) || replay.RunID != manual.RunID || replay.Outcome != OutcomeFailed || replay.FailureReason != manual.FailureReason {
		t.Fatal("manual terminal refusal changed", replay, err)
	}
	replaySeq, _ := retained(replay)
	if manualSeq != after+1 || replaySeq != manualSeq {
		t.Fatal("manual retry emitted another completion", after, manualSeq, replaySeq)
	}

	now = now.Add(time.Minute)
	fixed := deliver(nil)
	offline, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "custody-disconnected"})
	if err != nil {
		t.Fatal(err)
	}
	offlineProducer, err := bus.NewProducer(offline, bus.ProducerConfig{Source: "accounting", ProducerVersion: "fixture", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.Close(); err != nil {
		t.Fatal(err)
	}
	disconnected, err := NewReconciler(store, load, offlineProducer, nil, metrics, testLogger(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	retryKey := uuid.NewString()
	refused, err := disconnected.ReconcileManual(ctx, sub, retryKey, "operator:small-close")
	if !errors.Is(err, bus.ErrPublishCapacityUnknown) || refused.RunID != "" {
		t.Fatal("unavailable capacity must preserve retry", refused.RunID, err)
	}
	var claims int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM custody_runs WHERE run_id=$1", "manual:"+retryKey).Scan(&claims); err != nil || claims != 0 {
		t.Fatal("unavailable capacity consumed the manual request", claims, err)
	}
	open, err = store.OutstandingBreaks(ctx, sub.PortfolioID)
	value, ok = gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": sub.PortfolioID, "custodian": sub.CustodianID})
	if err != nil || len(open) != 1 || open[0].BreakID != original.BreakID || open[0].Revision != original.Revision || !ok || value != float64(prior.CompletedAt.Unix()) {
		t.Fatal("unavailable capacity changed discrepancy or freshness", open, value, err)
	}
	clean, err := writer.ReconcileManual(ctx, sub, retryKey, "operator:small-close")
	if err != nil || clean.Outcome != OutcomeClean || clean.StatementID != fixed {
		t.Fatal("subsequent valid close", clean, err)
	}
	final, _ := retained(clean)
	var unchanged []byte
	if err := pool.QueryRow(ctx, "SELECT manual_receipt FROM custody_runs WHERE run_id=$1", prior.RunID).Scan(&unchanged); err != nil || !bytes.Equal(unchanged, priorBytes) || final != replaySeq+1 {
		t.Fatal("prior receipt or completion count", err)
	}
	open, err = store.OutstandingBreaks(ctx, sub.PortfolioID)
	if err != nil || len(open) != 0 {
		t.Fatal("valid correction did not resolve discrepancy", len(open), err)
	}
}
