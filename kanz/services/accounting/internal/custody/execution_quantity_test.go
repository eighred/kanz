package custody

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestSignedExecutionQuantityDiagnosticsAfterCorrection(t *testing.T) {
	runSignedQuantityFixture(t, false)
}

func TestSignedExecutionQuantityWorkingLifecycle(t *testing.T) {
	if os.Getenv("TEST_CUSTODY_ASSIGNMENT_FIXTURE") != "1" {
		t.Skip("requires distinct restricted evidence issuers")
	}
	runSignedQuantityFixture(t, true)
}

func runSignedQuantityFixture(t *testing.T, enrolled bool) {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires restricted PostgreSQL and real JetStream")
	}
	tenant := "execution-quantity-pilot"
	if enrolled {
		tenant = "synthetic-custody-pilot"
	}
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	source, key := exampleAuthority(t, tenant)
	source.Account = "ACC1"
	if enrolled {
		source.Account = "sandbox-account"
		_, v, g := assignmentRolePools(t, pool, tenant)
		write := signedAccountFixture(t, pool, ctx)
		if err := write(v, "venue_identity", nil); err != nil {
			t.Fatal(err)
		}
		if err := write(g, "custody_assignment", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO custody_assignment_readers VALUES(session_user,$1)", tenant); err != nil {
			t.Fatal(err)
		}
	}
	source.Instruments = []string{"AAPL"}
	st := NewPostgres(pool)
	receiver, err := NewSignedBatchConsumer(st, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, fmt.Sprintf("QUANTITY_%d", time.Now().UnixNano()), []string{SubjectStatement, SubjectRun})
	name, err := js.StreamNameBySubject(ctx, SubjectStatement)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "custody-quantity-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	publisher := func(service string) *bus.Producer {
		p, err := bus.NewProducer(client, bus.ProducerConfig{Source: service, ProducerVersion: "test", Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	importer, err := NewBatchImporter(source, tenant, []string{"PF1:CUST-A"}, "PF1:CUST-A:"+source.Account, pool, publisher(BatchProducer), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	journal := ledger.NewPostgres(pool)
	for i, id := range []string{"independent-buy", "independent-sell"} {
		q := int64(1 - 2*i)
		entry := closeTrade(id, q, -10*q, t0)
		entry.VenueAccountID = source.Account
		if err := journal.Append(ctx, entry, nil); err != nil {
			t.Fatal(err)
		}
	}
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	scope, err := NewBookScope([]Subject{sub}, map[string]map[string][]string{"PF1": {"CUST-A": {source.Account}}})
	if err != nil {
		t.Fatal(err)
	}
	if enrolled {
		scope, err = scope.AdmitAssignments(ctx, sub.BusinessDate, st)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewReconciler(st, LedgerBookLoader(journal, scope, testLogger()), publisher("accounting"), nil, nil, testLogger(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	previous := ""
	var retained []Run
	var originalIDs []string
	var originalRun string
	for pos, index := range []int{0, 1, 0, 2} {
		qty := 2
		if index == 2 {
			qty = 1
		}
		price := 10
		if index == 2 {
			// Both quantities agree. The independent buy/sell at 9 versus the
			// book's 10 have equal and opposite cash discrepancies.
			price = 9
		}
		line := func(ref string, q int) string {
			units := ""
			if index > 0 {
				units = `,"quantityInInstrumentUnits":true`
			}
			return fmt.Sprintf(`{"externalRef":%q,"instrumentId":"AAPL","currencyCode":"USD","tradeDate":%q,"quantity":{"coefficient":"%d"},"price":{"coefficient":"%d"},"cash":{"coefficient":"%d"}%s}`, ref, sub.BusinessDate.Format(time.RFC3339), q, price, -price*q, units)
		}
		level := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":%q,"positions":[],"cash":[],"transactions":[%s,%s],"grain":"STATEMENT_GRAIN_TRANSACTIONS"}`, sub.BusinessDate.Format(time.RFC3339), line("independent-buy", qty), line("independent-sell", -qty)))
		raw, err := json.Marshal(signedBatch{BatchFormat, source.Source, tenant, source.Account, int64(pos + 1), previous, true, level})
		if err != nil {
			t.Fatal(err)
		}
		id, err := importer.Import(ctx, raw, ed25519.Sign(key, raw))
		if err != nil {
			t.Fatal(err)
		}
		previous = fmt.Sprintf("%x", sha256.Sum256(raw))
		msg, err := stream.GetLastMsgForSubject(ctx, SubjectStatement)
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		if err := proto.Unmarshal(msg.Data, &frame); err != nil || frame.Envelope.GetTenantId() != tenant {
			t.Fatal("signed frame", err)
		}
		if err := receiver.Handle(ctx, frame.Envelope, frame.Payload); err != nil {
			t.Fatal(err)
		}
		run, err := r.Reconcile(ctx, sub)
		if err != nil || run.StatementID != id || run.Outcome != OutcomeClean || len(run.Breaks) != 0 {
			t.Fatal("membership receipt", run, err)
		}
		verifyExecutionQuantityRevision(t, index, run.ExecutionQuantities)
		retained = append(retained, run)
		// An older writer scans and resolves absence in its own generic working set.
		if _, err := st.UpsertBreaks(ctx, sub, nil, run.CompletedAt); err != nil {
			t.Fatal(err)
		}
		queue, err := st.OutstandingBreaks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if enrolled && (pos == 1 || pos == 2) {
			if len(queue) != 2 {
				t.Fatalf("expected both signed quantity working items, got %d", len(queue))
			}
			if pos == 1 {
				originalRun = run.RunID
				verifyMixedQueuePages(t, ctx, st, queue)
			}
			for i, b := range queue {
				if b.Kind.String() != "execution_quantity" || b.InstrumentID != "AAPL" || b.Detection.RunID != originalRun || b.PortfolioID != "PF1" || b.CustodianID != "CUST-A" || b.Revision != 0 || !b.ValuesVerified || b.Status != BreakOpen {
					t.Fatalf("incorrect working item: %+v", b)
				}
				sign := int64(1)
				if b.Key == "independent-sell" {
					sign = -1
				} else if b.Key != "independent-buy" {
					t.Fatal(b.Key)
				}
				if b.IBOR.Cmp(big.NewRat(sign, 1)) != 0 || b.Custodian.Cmp(big.NewRat(2*sign, 1)) != 0 || b.Diff.Cmp(big.NewRat(-sign, 1)) != 0 {
					t.Fatal(b)
				}
				if pos == 1 {
					originalIDs = append(originalIDs, b.BreakID)
				} else if b.BreakID != originalIDs[i] {
					t.Fatal("unmeasured revision replaced original item")
				}
				if _, err := st.LoadBreak(ctx, b.BreakID); !errors.Is(err, ErrNoBreak) {
					t.Fatal("quantity exposed via action target", err)
				}
			}
			foreign := NewPostgres(newCustodyPool(t, "second-tenant"))
			other, err := foreign.OutstandingBreaks(ctx)
			if err != nil || len(other) != 0 {
				t.Fatal("foreign queue", other, err)
			}
		} else if len(queue) != 0 {
			t.Fatal("unexpected outstanding", queue)
		}
		if enrolled && pos == 2 {
			// Mimic a direct writer applying the account's measured book amount
			// as an equality to a later, membership-only source.
			_, err := pool.Exec(ctx, `INSERT INTO custody_execution_quantity_observations
                (portfolio_id,custodian_id,business_date,break_id,run_id,statement_id,entry_id,account_id,instrument_id,external_ref,
                 source_revision,ibor,custodian,difference,first_seen_at,measured_at)
                SELECT o.portfolio_id,o.custodian_id,o.business_date,o.break_id,$1,$2,o.entry_id,o.account_id,o.instrument_id,o.external_ref,
                       3,o.ibor,o.ibor,'0',o.first_seen_at,$3 FROM custody_execution_quantity_observations o WHERE o.run_id=$4`,
				run.RunID, run.StatementID, run.CompletedAt, originalRun)
			var db *pgconn.PgError
			if !errors.As(err, &db) || db.Code != "P0001" || !strings.Contains(db.Message, "signed measurement") {
				t.Fatalf("undocumented equality was accepted: %v", err)
			}
		}
		if enrolled && pos == 3 {
			// Re-delivering the signed correction and re-running the same close
			// must leave exactly one resolving measurement per original item.
			if err := receiver.Handle(ctx, frame.Envelope, frame.Payload); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, sub); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM custody_execution_quantity_observations WHERE difference='0'").Scan(&count); err != nil || count != 2 {
				t.Fatal("duplicated correction", count, err)
			}
		}

	}
	if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE tenant_id=app_current_tenant()"); err != nil {
		t.Fatal(err)
	}
	for i, run := range retained {
		result, err := NewPostgres(pool).HistoricalReceipt(ctx, sub, source.Account, run.RunID)
		if err != nil || result.State != "recorded" || result.StatementID != run.StatementID {
			t.Fatalf("historical revision %d: %+v %v", i, result, err)
		}
		verifyExecutionQuantityRevision(t, []int{0, 1, 0, 2}[i], result.Run.ExecutionQuantities)
		reported, ok := result.ReportedExecutionFigures()
		if ok != enrolled {
			t.Fatalf("source details available without original account proof: %t", ok)
		}
		if enrolled && i == 3 {
			want := []ReportedExecution{{"independent-buy", "AAPL", "9", "-9", "USD"}, {"independent-sell", "AAPL", "9", "9", "USD"}}
			if !slices.Equal(reported, want) || result.Run.Outcome != OutcomeClean || len(result.Run.ExecutionQuantities.Differences) != 0 {
				t.Fatalf("lost signed offsetting source figures: %+v", reported)
			}
		}
	}
	outstanding, err := st.OutstandingBreaks(ctx)
	if err != nil || len(outstanding) != 0 {
		t.Fatal("diagnostic populated the working queue", outstanding, err)
	}
	if !enrolled {
		return
	}
	// Reopen on D, actually complete D+1, then deliver a signed correction for
	// D. The historical balance fence must keep the original working items open.
	deliver := func(level json.RawMessage, rev int64, previous string) (id, digest string) {
		t.Helper()
		raw, err := json.Marshal(signedBatch{BatchFormat, source.Source, tenant, source.Account, rev, previous, true, level})
		if err != nil {
			t.Fatal(err)
		}
		id, err = importer.Import(ctx, raw, ed25519.Sign(key, raw))
		if err != nil {
			t.Fatal(err)
		}
		msg, err := stream.GetLastMsgForSubject(ctx, SubjectStatement)
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		if err := proto.Unmarshal(msg.Data, &frame); err != nil {
			t.Fatal(err)
		}
		if err := receiver.Handle(ctx, frame.Envelope, frame.Payload); err != nil {
			t.Fatal(err)
		}
		return id, fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	levelAt := func(revision int64) json.RawMessage {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, "SELECT signed_batch FROM custody_batch_receipts WHERE business_date=$1 AND revision=$2", sub.BusinessDate, revision).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var batch signedBatch
		if err := json.Unmarshal(raw, &batch); err != nil {
			t.Fatal(err)
		}
		return batch.Statement
	}
	_, digest := deliver(levelAt(2), 5, previous)
	original, err := r.Reconcile(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	next := sub
	next.BusinessDate = next.BusinessDate.AddDate(0, 0, 1)
	nextLevel := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":%q,"positions":[],"cash":[],"transactions":[],"grain":"STATEMENT_GRAIN_TRANSACTIONS"}`, next.BusinessDate.Format(time.RFC3339)))
	deliver(nextLevel, 1, "")
	future, err := r.Reconcile(ctx, next)
	if err != nil || future.Outcome != OutcomeClean {
		t.Fatalf("future comparison: %+v %v", future, err)
	}
	// An omitted unit assertion on D cannot resolve even one item after D+1.
	omitted, nextDigest := deliver(levelAt(1), 6, digest)
	if _, err := r.Reconcile(ctx, sub); !errors.Is(err, ErrHistoricalLifecycle) {
		t.Fatalf("legacy source resolved a historical item: %v", err)
	}

	for _, other := range [][]string{{"other-portfolio"}, {"other-portfolio", "another"}} {
		denied, err := st.OutstandingBreaks(ctx, other...)
		if err != nil || len(denied) != 0 {
			t.Fatal("out-of-scope queue", denied, err)
		}
	}
	blocked, err := st.OutstandingBreaks(ctx, sub.PortfolioID)
	if err != nil || len(blocked) != 2 {
		t.Fatal("late correction removed open items", blocked, err)
	}
	for _, item := range blocked {
		if item.LateCorrectionStatementID != omitted || item.Detection.RunID != original.RunID {
			t.Fatal("late correction replaced original detection", item)
		}
	}

	// Only one source line asserts comparable units. The other reference stays
	// open with its original detection despite the successful partial correction.
	partialLevel := strings.Replace(string(levelAt(4)), `"quantityInInstrumentUnits":true`, `"quantityInInstrumentUnits":false`, 1)
	partialID, partialDigest := deliver(json.RawMessage(partialLevel), 7, nextDigest)
	stale, err := NewReconciler(st, func(ctx context.Context, s Subject) (*ledger.Book, []ledger.Execution, *RunBasis, error) {
		book, entries, basis, err := LedgerBookLoader(journal, scope, testLogger())(ctx, s)
		if err != nil {
			return nil, nil, nil, err
		}
		lateEntry := closeTrade("source-arrived-during-compare", 1, -10, s.BusinessDate.AddDate(0, 0, 2))
		lateEntry.VenueAccountID = source.Account
		if err := journal.Append(ctx, lateEntry, nil); err != nil {
			return nil, nil, nil, err
		}
		return book, entries, basis, nil
	}, publisher("accounting"), nil, nil, testLogger(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.ReconcileManual(ctx, sub, "00482737-9c5f-4c07-b6dc-051d6509240e", "operator:quantity"); !errors.Is(err, ErrBookBasisChanged) {
		t.Fatal("stale book acquired working item", err)
	}
	stillOpen, err := st.OutstandingBreaks(ctx, sub.PortfolioID)
	if err != nil || len(stillOpen) != 2 {
		t.Fatal("stale correction changed items", stillOpen, err)
	}
	partial, err := r.Reconcile(ctx, sub)
	if err != nil || partial.Outcome != OutcomeHistoricalQuantityCorrection || len(partial.HistoricalQuantityResolutions) != 1 {
		t.Fatal("partial signed correction", partial, err)
	}
	remaining, err := st.OutstandingBreaks(ctx, sub.PortfolioID)
	if err != nil || len(remaining) != 1 || remaining[0].Detection.RunID != original.RunID || remaining[0].Key != "independent-buy" || remaining[0].LateCorrectionStatementID != partialID {
		t.Fatal("unmeasured reference was cleared", remaining, err)
	}
	late, lateDigest := deliver(levelAt(4), 8, partialDigest)
	// Separate reconcilers/pools emulate simultaneous delivery, using the
	// original authenticated command identity for replay after contention.
	const request = "e8b0a101-bdbd-431f-8390-5413c6a28bde"
	type reply struct {
		run Run
		err error
	}
	start := make(chan struct{})
	done := make(chan reply, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			run, err := r.ReconcileManual(ctx, sub, request, "operator:quantity")
			done <- reply{run, err}
		}()
	}
	close(start)
	var accepted bool
	for i := 0; i < 2; i++ {
		got := <-done
		if got.err == nil {
			accepted = true
			if got.run.Outcome != OutcomeHistoricalQuantityCorrection {
				t.Fatal(got.run)
			}
		} else if !errors.Is(got.err, ErrRunBusy) {
			t.Fatal("concurrent correction", got.err)
		}
	}
	if !accepted {
		t.Fatal("both contenders refused")
	}
	receipt, err := r.ReconcileManual(ctx, sub, request, "operator:quantity")
	if err != nil || receipt.Outcome != OutcomeHistoricalQuantityCorrection || receipt.StatementID != late || len(receipt.HistoricalQuantityResolutions) != 1 || len(receipt.Breaks) != 0 {
		t.Fatalf("unexpected late receipt: %+v %v", receipt, err)
	}
	for _, link := range receipt.HistoricalQuantityResolutions {
		if link.DetectionRunId != original.RunID || link.InstrumentId != "AAPL" || link.EntryId != link.ExternalRef {
			t.Fatal("unbound correction", link)
		}
		sign := int64(1)
		if link.ExternalRef == "independent-sell" {
			sign = -1
		} else if link.ExternalRef != "independent-buy" {
			t.Fatal(link)
		}
		if link.AgreedQuantity == nil || link.AgreedQuantity.Coefficient != sign || link.AgreedQuantity.Exponent != 0 {
			t.Fatal("wrong agreed units", link)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM custody_execution_quantity_observations WHERE run_id=$1 AND source_revision=8 AND difference='0'", receipt.RunID).Scan(&count); err != nil || count != 1 {
		t.Fatal("replayed correction", count, err)
	}
	after, err := st.OutstandingBreaks(ctx, sub.PortfolioID)
	if err != nil || len(after) != 0 {
		t.Fatal("late equality did not clear its two specific items", after, err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE tenant_id=app_current_tenant()"); err != nil {
		t.Fatal(err)
	}
	restart := NewPostgres(pool)
	for _, want := range []Run{original, future, partial, receipt} {
		saved, err := restart.HistoricalReceipt(ctx, want.Subject, source.Account, want.RunID)
		if err != nil || saved.State != "recorded" || saved.Run.Outcome != want.Outcome || saved.StatementID != want.StatementID || len(saved.Run.HistoricalQuantityResolutions) != len(want.HistoricalQuantityResolutions) {
			t.Fatalf("prior/saved independent receipt: %s %s %+v %v", want.RunID, want.Outcome, saved, err)
		}
	}
	// A scheduler retry after outbox pruning retrieves the same authoritative
	// completion; it does not publish a new historical failure for this close.
	repeated, err := r.Reconcile(ctx, sub)
	if err != nil || repeated.RunID != receipt.RunID || repeated.Outcome != OutcomeHistoricalQuantityCorrection {
		t.Fatal("scheduled correction retry", repeated, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE tenant_id=app_current_tenant()").Scan(&count); err != nil || count != 0 {
		t.Fatal("scheduled replay enqueued another completion", count, err)
	}
	current, err := restart.DatedResult(ctx, sub)
	if err != nil || current.State != "pending" || current.HistoricalQuantityCorrectionRunID != receipt.RunID {
		t.Fatalf("current historical discovery: %+v %v", current, err)
	}
	nextRead, err := restart.DatedResult(ctx, next)
	if err != nil || nextRead.State != "recorded" || nextRead.Run.RunID != future.RunID {
		t.Fatal("replaced later close", nextRead, err)
	}

	// An incoming higher revision has reached the trusted receipt table but not the consumer.
	// It suppresses current discovery while the explicitly requested historical receipt survives.
	raw, err := json.Marshal(signedBatch{BatchFormat, source.Source, tenant, source.Account, 9, lateDigest, true, levelAt(4)})
	if err != nil {
		t.Fatal(err)
	}
	pendingID, err := importer.Import(ctx, raw, ed25519.Sign(key, raw))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := restart.DatedResult(ctx, sub)
	if err != nil || pending.State != "pending" || pending.StatementID != pendingID || pending.HistoricalQuantityCorrectionRunID != "" {
		t.Fatal("stale correction link", pending, err)
	}
	inaccessible, err := restart.HistoricalReceipt(ctx, sub, "another-account", receipt.RunID)
	if err != nil || inaccessible.State != "unknown" {
		t.Fatal("wrong account saw correction", inaccessible, err)
	}

}

// The two quantity observations were written by the signed source and real ledger
// fixture. Compare them with ordinary breaks in the same database snapshot.
func verifyMixedQueuePages(t *testing.T, ctx context.Context, st *Postgres, quantity []Break) {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	const add = `INSERT INTO custody_breaks(break_id,portfolio_id,custodian_id,kind,break_key,ibor,custodian,difference,first_seen_at,last_seen_at,status_changed_at,status,values_verified)
 VALUES($1,$2,'PAGE-C','quantity',$1,'1','2','-1',now(),now(),now(),'open',true)`
	for _, row := range [][2]string{{"A-before", "PF1"}, {"z-after", "PF1"}, {"00-other", "PF2"}} {
		if _, err := tx.Exec(ctx, add, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := &Postgres{pool: st.pool, tx: tx}
	want := []string{"A-before", quantity[0].BreakID, quantity[1].BreakID, "z-after"}
	for _, limit := range []int{1, 2, 3, 4} {
		var seen []string
		after := ""
		for len(seen) <= len(want) {
			page, err := snapshot.BreakPage(ctx, []string{"PF1"}, after, limit)
			if err != nil || len(page) > limit {
				t.Fatalf("limit %d rows %d: %v", limit, len(page), err)
			}
			for _, b := range page {
				if b.BreakID <= after || b.PortfolioID != "PF1" {
					t.Fatalf("invalid continuation after %q: %+v", after, b)
				}
				seen = append(seen, b.BreakID)
				after = b.BreakID
			}
			if len(page) < limit {
				break
			}
		}
		if !slices.Equal(seen, want) {
			t.Fatalf("limit %d got %v want %v", limit, seen, want)
		}
	}
}

func verifyExecutionQuantityRevision(t *testing.T, index int, q *accountingpb.ExecutionQuantityComparison) {
	t.Helper()
	if index == 0 {
		if q != nil {
			t.Fatal("legacy receipt acquired quantity evidence", q)
		}
		return
	}
	if q == nil || q.Compared != 2 || q.Unassessed != 0 {
		t.Fatal("wrong assessed population", q)
	}
	if index == 2 {
		if len(q.Differences) != 0 {
			t.Fatal("correction did not agree", q)
		}
		return
	}
	if len(q.Differences) != 2 {
		t.Fatal("offsetting differences missing", q)
	}
	for i, d := range q.Differences {
		sign := int64(1 - 2*i)
		if d.ExternalRef != []string{"independent-buy", "independent-sell"}[i] || d.InstrumentId != "AAPL" || dec.FromProto(d.Ibor).Cmp(big.NewRat(sign, 1)) != 0 || dec.FromProto(d.Custodian).Cmp(big.NewRat(2*sign, 1)) != 0 || dec.FromProto(d.Difference).Cmp(big.NewRat(-sign, 1)) != 0 {
			t.Fatal("wrong signed difference", d)
		}
	}
}

func TestOldQuantitySweepCannotClearReservedBreak(t *testing.T) {
	pool := newCustodyPool(t, "quantity-fence")
	requireRestrictedCustodyRole(t, pool)
	const migration = "0029_custody_quantity_queue_hold.sql"
	applyCustodySchema(t, pool, migration)
	ctx := t.Context()
	// Seed the future kind before the hold exists; a refusal over an empty table
	// would offer no evidence about the deployed membership-only writer.
	const add = `INSERT INTO custody_breaks(break_id,portfolio_id,custodian_id,kind,break_key,ibor,custodian,difference,first_seen_at,last_seen_at,status_changed_at,status,values_verified)
 VALUES($1,'PF1','CUST-A','execution_quantity','independent-buy','1','2','-1',now(),now(),now(),'open',true)`
	if _, err := pool.Exec(ctx, add, "existing"); err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile(filepath.Join(custodyMigrationDir, migration))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatal(err)
	}
	expectHold := func(err error) {
		t.Helper()
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || !strings.Contains(pgerr.Where, "ledger_entries_worm") {
			t.Fatalf("expected quantity queue hold: %v", err)
		}
	}
	_, err = pool.Exec(ctx, add, "new")
	expectHold(err)
	// UpsertBreaks uses the deployed resolve-on-absence SQL with zero detected IDs.
	_, err = NewPostgres(pool).UpsertBreaks(ctx, subject(), nil, t0)
	expectHold(err)
	_, err = pool.Exec(ctx, "DELETE FROM custody_breaks WHERE break_id='existing'")
	expectHold(err)
	var status, diff string
	if err := pool.QueryRow(ctx, "SELECT status,difference FROM custody_breaks WHERE break_id='existing'").Scan(&status, &diff); err != nil || status != "open" || diff != "-1" {
		t.Fatal("legacy sweep cleared the evidence", status, diff, err)
	}
}

func TestQuantityReceiptRefusesOversizedSerializedCompletion(t *testing.T) {
	ctx := bus.WithTenantID(t.Context(), "quantity-size")
	q := &accountingpb.ExecutionQuantityComparison{}
	run := Run{Subject: subject(), RunID: "sized", StatementID: "independent", CompletedAt: t0, Outcome: OutcomeClean, ExecutionQuantities: q}
	one, _ := exactProto(big.NewRat(1, 1))
	two, _ := exactProto(big.NewRat(2, 1))
	minusOne, _ := exactProto(big.NewRat(-1, 1))
	// All individual rows fit. Only the completed receipt crosses the bound.
	for i := 0; i < 3000; i++ {
		q.Differences = append(q.Differences, &accountingpb.ExecutionQuantityDifference{ExternalRef: fmt.Sprintf("%04d-%s", i, strings.Repeat("r", 470)), InstrumentId: "A", Ibor: one, Custodian: two, Difference: minusOne})
	}
	q.Compared = uint32(len(q.Differences))
	if _, err := runRecord(ctx, "quantity-size", run); !errors.Is(err, errQuantityReceiptLimit) {
		t.Fatal("oversized serialized completion", err)
	}
	q.Differences = q.Differences[:1900]
	q.Compared = uint32(len(q.Differences))
	record, err := runRecord(ctx, "quantity-size", run)
	if err != nil || len(record.Payload) > BatchLimit || len(record.Payload) < BatchLimit/2 {
		t.Fatal("bounded serialized completion", len(record.Payload), err)
	}
}
