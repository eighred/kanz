package custody

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"math/big"
	"os"
	"path/filepath"
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
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires restricted PostgreSQL and real JetStream")
	}
	const tenant = "execution-quantity-pilot"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	source, key := exampleAuthority(t, tenant)
	source.Account = "ACC1"
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
	importer, err := NewBatchImporter(source, tenant, []string{"PF1:CUST-A"}, "PF1:CUST-A:ACC1", pool, publisher(BatchProducer), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	journal := ledger.NewPostgres(pool)
	for i, id := range []string{"independent-buy", "independent-sell"} {
		q := int64(1 - 2*i)
		if err := journal.Append(ctx, closeTrade(id, q, -10*q, t0), nil); err != nil {
			t.Fatal(err)
		}
	}
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	r, err := NewReconciler(st, LedgerBookLoader(journal, closeScope(t, true), testLogger()), publisher("accounting"), nil, nil, testLogger(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	previous := ""
	var retained []Run
	for index, qty := range []int{2, 2, 1} {
		line := func(ref string, q int) string {
			units := ""
			if index > 0 {
				units = `,"quantityInInstrumentUnits":true`
			}
			return fmt.Sprintf(`{"externalRef":%q,"instrumentId":"AAPL","currencyCode":"USD","tradeDate":%q,"quantity":{"coefficient":"%d"},"price":{"coefficient":"10"},"cash":{"coefficient":"%d"}%s}`, ref, sub.BusinessDate.Format(time.RFC3339), q, -10*q, units)
		}
		level := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":%q,"positions":[],"cash":[],"transactions":[%s,%s],"grain":"STATEMENT_GRAIN_TRANSACTIONS"}`, sub.BusinessDate.Format(time.RFC3339), line("independent-buy", qty), line("independent-sell", -qty)))
		raw, err := json.Marshal(signedBatch{BatchFormat, source.Source, tenant, source.Account, int64(index + 1), previous, true, level})
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
	}
	if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE tenant_id=app_current_tenant()"); err != nil {
		t.Fatal(err)
	}
	for i, run := range retained {
		result, err := NewPostgres(pool).HistoricalReceipt(ctx, sub, "ACC1", run.RunID)
		if err != nil || result.State != "recorded" || result.StatementID != run.StatementID {
			t.Fatalf("historical revision %d: %+v %v", i, result, err)
		}
		verifyExecutionQuantityRevision(t, i, result.Run.ExecutionQuantities)
	}
	outstanding, err := st.OutstandingBreaks(ctx)
	if err != nil || len(outstanding) != 0 {
		t.Fatal("diagnostic populated the working queue", outstanding, err)
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
