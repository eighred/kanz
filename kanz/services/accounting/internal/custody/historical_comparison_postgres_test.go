package custody

import (
	"bytes"

	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestManualHistoricalComparisonPreservesLaterWorkingSet(t *testing.T) {
	const tenant, account, actor = "synthetic-custody-pilot", "sandbox-account", "operator:historical-review"
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real JetStream")
	}
	pool, _, v, g := assignmentTestPools(t, tenant)
	ctx := bus.WithTenantID(t.Context(), tenant)
	attest := signedAccountFixture(t, pool, ctx)
	if err := attest(v, "venue_identity", nil); err != nil {
		t.Fatal(err)
	}
	if err := attest(g, "custody_assignment", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO custody_assignment_readers VALUES(session_user,$1)", tenant); err != nil {
		t.Fatal(err)
	}
	st, journal := NewPostgres(pool), ledger.NewPostgres(pool)
	day := time.Date(2020, 2, 4, 0, 0, 0, 0, time.UTC)
	d := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: day}
	next := d
	next.BusinessDate = day.AddDate(0, 0, 1)
	scope, err := NewBookScope([]Subject{d}, map[string]map[string][]string{"PF1": {"CUST-A": {account}}})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := scope.AdmitAssignments(ctx, day, st)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "HISTORICAL_"+uuid.NewString(), []string{SubjectRun, SubjectStatement})
	last := func(subject string) (*envelopepb.Envelope, []byte, uint64) {
		t.Helper()
		name, err := js.StreamNameBySubject(ctx, subject)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := stream.GetLastMsgForSubject(ctx, subject)
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(msg.Data)
		if err != nil || env.GetTenantId() != tenant || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetEventType() != subject {
			t.Fatalf("unexpected retained %s envelope: %v %v", subject, env, err)
		}
		return env, payload, msg.Sequence
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "historical-comparison"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer := func(service string) *bus.Producer {
		t.Helper()
		p, err := bus.NewProducer(client, bus.ProducerConfig{Source: service, ProducerVersion: "test", Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	accounting := producer("accounting")
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	metrics.SeedPair(d.PortfolioID, d.CustodianID)
	now := time.Now().UTC()
	compare := func(s *BookScope) *Reconciler {
		t.Helper()
		r, err := NewReconciler(NewPostgres(pool), LedgerBookLoader(journal, s, testLogger()), accounting, nil, metrics, testLogger(), func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := compare(admitted)
	authority, key := exampleAuthority(t, tenant)
	importer, err := NewBatchImporter(authority, tenant, []string{"PF1:CUST-A"}, "PF1:CUST-A:"+account, pool, producer(BatchProducer), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewSignedBatchConsumer(st, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	sign := func(s *accountingpb.CustodianStatement, rev int64, previous string) string {
		t.Helper()
		level, err := protojson.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(signedBatch{BatchFormat, authority.Source, tenant, account, rev, digests[previous], true, level})
		if err != nil {
			t.Fatal(err)
		}
		id, err := importer.Import(ctx, raw, ed25519.Sign(key, raw))
		if err != nil {
			t.Fatal(err)
		}
		env, payload, _ := last(SubjectStatement)
		if err := receiver.Handle(ctx, env, payload); err != nil {
			t.Fatal(err)
		}
		digests[id] = fmt.Sprintf("%x", sha256.Sum256(raw))
		return id
	}
	statement := func(name string) *accountingpb.CustodianStatement {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("../../cmd/custody_batch/testdata/execution_economics", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var fixture signedBatch
		if err := json.Unmarshal(data, &fixture); err != nil {
			t.Fatal(err)
		}
		var stmt accountingpb.CustodianStatement
		if err := protojson.Unmarshal(fixture.Statement, &stmt); err != nil {
			t.Fatal(err)
		}
		return &stmt
	}
	book := func(ref string, at time.Time) {
		t.Helper()
		fill := &orderpb.Fill{FillId: ref, OrderId: "order-" + ref, Venue: "XSIM", VenueAccountId: account, VenueExecutionId: ref, InstrumentId: "SYN", Side: orderpb.Side_SIDE_BUY, ExecutedAt: timestamppb.New(at), Quantity: &commonpb.Decimal{Coefficient: 105, Exponent: -1}, Price: &commonpb.Decimal{Coefficient: 1}, Fee: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 25, Exponent: -2}}}
		event, err := ledger.FromFill("PF1", fill, "USD", at)
		if err != nil {
			t.Fatal(err)
		}
		if err = journal.Append(ctx, event, nil); err != nil {
			t.Fatal(err)
		}
	}
	book("external-7", day)
	firstID := sign(statement("statement-r1"), 1, "")
	original, err := r.Reconcile(ctx, d)
	if err != nil || original.Outcome != OutcomeClean || original.StatementID != firstID {
		t.Fatal("first close", original, err)
	}
	checkHistoricalEconomics(t, original, false)

	// D+1 holds an open position discrepancy on SYN and a separate signed
	// execution-quantity observation. D's manual close has no authority over either.
	book("external-8", next.BusinessDate)
	later := statement("statement-r2")
	at := timestamppb.New(next.BusinessDate)
	later.BusinessDate = at
	later.Positions[0].Quantity = &commonpb.Decimal{Coefficient: 19}
	later.Cash[0].Balance = &commonpb.Decimal{Coefficient: -215, Exponent: -1}
	line := later.Transactions[0]
	line.ExternalRef, line.TradeDate, line.SettlementDate = "external-8", at, at
	line.Quantity = &commonpb.Decimal{Coefficient: 11}
	line.Cash = &commonpb.Decimal{Coefficient: -1125, Exponent: -2}
	laterID := sign(later, 1, "")
	now = now.Add(time.Second)
	future, err := r.Reconcile(ctx, next)
	if err != nil || future.Outcome != OutcomeBreaks || future.StatementID != laterID || len(future.Breaks) != 1 || future.Breaks[0].Diff.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatal("later close", future, err)
	}
	queue, err := st.OutstandingBreaks(ctx, d.PortfolioID)
	if err != nil || len(queue) != 2 {
		t.Fatal("later working set", queue, err)
	}
	quantity := false
	for _, b := range queue {
		if b.Kind.String() == "execution_quantity" {
			quantity = b.Key == "external-8" && b.Detection.RunID == future.RunID && b.Diff.Cmp(big.NewRat(-1, 2)) == 0
		}
	}
	if !quantity {
		t.Fatal("missing independent D+1 quantity observation", queue)
	}
	snapshot := func() string {
		t.Helper()
		var state string
		err := pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(b) ORDER BY break_id) FROM custody_breaks b),
 (SELECT jsonb_agg(to_jsonb(d) ORDER BY break_id) FROM custody_break_detections d),
 (SELECT jsonb_agg(to_jsonb(q) ORDER BY observation) FROM custody_execution_quantity_observations q),
 (SELECT jsonb_agg(to_jsonb(e) ORDER BY entry_id) FROM ledger_entries e))::text`).Scan(&state)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := snapshot()
	fixedID := sign(statement("statement-r2"), 2, firstID)
	verifySaved := func(want Run) {
		t.Helper()
		got, err := NewPostgres(pool).HistoricalReceipt(ctx, want.Subject, account, want.RunID)
		if err != nil || got.State != "recorded" || got.Run.Outcome != want.Outcome || got.StatementID != want.StatementID || got.SignedStatementSHA256 != digests[want.StatementID] || !proto.Equal(got.Run.ExecutionEconomics, want.ExecutionEconomics) || len(got.Run.Breaks) != len(want.Breaks) {
			t.Fatalf("saved %s (%s): %+v %v", want.RunID, want.Outcome, got, err)
		}
		for i, b := range got.Run.Breaks {
			w := want.Breaks[i]
			if b.BreakID != w.BreakID || !equalFigure(b.IBOR, w.IBOR) || !equalFigure(b.Custodian, w.Custodian) || !equalFigure(b.Diff, w.Diff) {
				t.Fatal("saved discrepancy changed", b, w)
			}
		}
	}
	manual := func(id string, want Outcome) Run {
		t.Helper()
		request := uuid.NewString()
		now = now.Add(time.Second)
		run, err := r.ReconcileManual(ctx, d, request, actor)
		if err != nil || run.Outcome != want || run.StatementID != id || run.Actor != actor || run.RequestID != request {
			t.Fatalf("manual %s: %+v %v", want, run, err)
		}
		env, wire, seq := last(SubjectRun)
		var receipt []byte
		err = pool.QueryRow(ctx, "SELECT manual_receipt FROM custody_runs WHERE run_id=$1", run.RunID).Scan(&receipt)
		if err != nil || !bytes.Equal(wire, receipt) || env.GetSource() != "accounting" || env.GetCorrelationId() != run.RunID || env.GetCausationId() != request {
			t.Fatal("completion lineage or payload", env, err)
		}
		var fact accountingpb.ReconciliationRun
		if err := proto.Unmarshal(wire, &fact); err != nil || fact.GetActor() != actor || fact.GetRequestId() != request || fact.GetRunId() != run.RunID || fact.GetStatementId() != id {
			t.Fatal("retained manual fact", err)
		}
		repeated, err := compare(admitted).ReconcileManual(ctx, d, request, actor)
		if err != nil || repeated.RunID != run.RunID || repeated.Outcome != want {
			t.Fatal("restart replay", repeated, err)
		}
		afterEnv, _, afterSeq := last(SubjectRun)
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE event_type=$1 AND causation_id=$2 AND published_at IS NOT NULL", SubjectRun, request).Scan(&count); err != nil || count != 1 || afterSeq != seq || afterEnv.GetEventId() != env.GetEventId() {
			t.Fatal("replay published again", count, err)
		}
		if got := snapshot(); got != before {
			t.Fatal("historical review changed live queue, quantity observations or financial journal")
		}
		v, ok := gaugeValue(t, registry, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": d.PortfolioID, "custodian": d.CustodianID})
		if !ok || v != float64(future.CompletedAt.Unix()) {
			t.Fatal("historical snapshot refreshed D+1 freshness", v)
		}
		v, ok = gaugeValue(t, registry, "kanz_accounting_reconciliation_runs_total", map[string]string{"custodian": d.CustodianID, "outcome": want.String()})
		if !ok || v != 1 {
			t.Fatal("historical outcome count after replay", v)
		}
		verifySaved(run)
		return run
	}
	fixed := manual(fixedID, OutcomeHistoricalClean)
	if len(fixed.Breaks) != 0 {
		t.Fatal("corrected source emitted a break", fixed.Breaks)
	}
	checkHistoricalEconomics(t, fixed, true)
	disputed := statement("statement-r1")
	disputed.Positions[0].Quantity = &commonpb.Decimal{Coefficient: 85, Exponent: -1}
	disputedID := sign(disputed, 3, fixedID)
	disputedRun := manual(disputedID, OutcomeHistoricalBreaks)
	if len(disputedRun.Breaks) != 1 {
		t.Fatal(disputedRun.Breaks)
	}
	b := disputedRun.Breaks[0]
	if b.BreakID != future.Breaks[0].BreakID || b.Key != "SYN" || b.Status != BreakOpen || b.Assignee != "" || b.Explanation != "" || b.IBOR.Cmp(big.NewRat(21, 2)) != 0 || b.Custodian.Cmp(big.NewRat(17, 2)) != 0 || b.Diff.Cmp(big.NewRat(2, 1)) != 0 || !b.FirstSeenAt.Equal(disputedRun.CompletedAt) {
		t.Fatal("historical discrepancy inherited live figures or lifecycle", b)
	}
	checkHistoricalEconomics(t, disputedRun, false)
	for _, run := range []Run{original, future, fixed, disputedRun} {
		verifySaved(run)
	}
	current, err := st.DatedResult(ctx, next)
	if err != nil || current.State != "recorded" || current.Run.RunID != future.RunID || current.StatementID != laterID {
		t.Fatal("D+1 lost its comparison", current, err)
	}

	// Same signed statement and declared account, but no dated assignment
	// admission on this loader. Its failed manual receipt cannot clear the queue.
	refused, err := compare(scope).ReconcileManual(ctx, d, uuid.NewString(), actor)
	if !errors.Is(err, ErrHistoricalLifecycle) || refused.Outcome != OutcomeFailed || refused.ExecutionEconomics != nil || snapshot() != before {
		t.Fatal("unproven comparison accepted or affected live state", refused, err)
	}
}

func checkHistoricalEconomics(t *testing.T, run Run, corrected bool) {
	t.Helper()
	rows := run.ExecutionEconomics.GetRows()
	if len(rows) != 1 {
		t.Fatalf("%s: %d economic rows", run.Outcome, len(rows))
	}
	row := rows[0]
	fee, net, status := big.NewRat(1, 2), big.NewRat(-11, 1), "mismatch"
	if corrected {
		fee, net, status = big.NewRat(1, 4), big.NewRat(-43, 4), "equal"
	}
	if row.Status != status || row.ExternalRef != "external-7" || row.InstrumentId != "SYN" || row.CurrencyCode != "USD" || row.EntryId == "" || row.Book == nil || row.Custodian == nil || row.Difference == nil {
		t.Fatal(row)
	}
	bookFee, bookNet := big.NewRat(1, 4), big.NewRat(-43, 4)
	for _, amount := range []struct {
		got  *commonpb.Decimal
		want *big.Rat
	}{
		{row.Book.Commission, bookFee}, {row.Book.NetCash, bookNet}, {row.Custodian.Commission, fee}, {row.Custodian.NetCash, net},
		{row.Difference.Commission, new(big.Rat).Sub(bookFee, fee)}, {row.Difference.NetCash, new(big.Rat).Sub(bookNet, net)},
	} {
		if amount.got == nil || dec.FromProto(amount.got).Cmp(amount.want) != 0 {
			t.Fatalf("%s: got %v, want %s", status, amount.got, amount.want)
		}
	}
}
