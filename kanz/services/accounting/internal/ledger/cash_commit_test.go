package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresCashCommitCoverageAndAtomicHistory(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := bus.WithTenantID(context.Background(), "__system__")
	at := day(10)
	var latest *accountingpb.PortfolioCashBalance
	var reject bool
	var oversized bool
	refusal := errors.New("reject after sealing")
	announce := func(ctx context.Context, store Store) ([]outbox.Record, error) {
		p, err := MaterializeCash(ctx, store, "fund", at)
		if err != nil {
			return nil, err
		}
		total, ok := dec.ToProtoExact(p.Book.CashBalance("USD"))
		if !ok {
			return nil, ErrCashCommit
		}
		msg := &accountingpb.PortfolioCashBalance{PortfolioId: "fund", BaseCurrency: "USD", Total: total, AsOf: timestamppb.New(at), KnowledgeTime: timestamppb.New(at)}
		if oversized {
			msg.ExcludedCurrencies = []string{strings.Repeat("X", maxCashCommitBytes)}
		}
		if err := store.(*Postgres).SealCashBalance(ctx, msg, p); err != nil {
			return nil, err
		}
		latest = msg
		if reject {
			return nil, refusal
		}
		record, err := outbox.From(ctx, bus.Event{Subject: "accounting.balance.portfolio", EventType: "accounting.balance.portfolio", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting", EventTime: at, PartitionKey: "fund", PayloadSchemaRef: "accounting.v1.PortfolioCashBalance:1", Payload: msg})
		return []outbox.Record{record}, err
	}
	check := func(revision, total int64, debit *int64) {
		t.Helper()
		c := latest.GetCashCommit()
		if c.GetRevision() != revision || !c.GetComplete() || c.GetUnattributedEntries() != 0 || dec.FromProto(latest.GetTotal()).Cmp(big.NewRat(total, 1)) != 0 {
			t.Fatalf("unexpected balance/coverage: %v", latest)
		}
		if debit == nil {
			if len(c.Applied) != 0 {
				t.Fatalf("unchanged debit re-emitted: %v", c.Applied)
			}
		} else if len(c.Applied) != 1 || c.Applied[0].OrderId != "order-account" || dec.FromProto(c.Applied[0].Debit).Cmp(big.NewRat(*debit, 1)) != 0 {
			t.Fatalf("debit proof=%v want=%d", c.Applied, *debit)
		}
	}
	opening := cashOne("coverage:opening", "fund", 250)
	opening.Effective, opening.Knowledge = day(1), day(1)
	if err := st.Append(ctx, opening, announce); err != nil {
		t.Fatal(err)
	}
	check(1, 250, nil)
	appendFill := func(fill *orderpb.Fill) error {
		e, err := FromFill("fund", fill, "USD", day(4))
		if err != nil {
			return err
		}
		return st.Append(ctx, e, announce)
	}
	original := executionEvidenceFixture("account")
	if err := appendFill(original); err != nil {
		t.Fatal(err)
	}
	debit := int64(101)
	check(2, 149, &debit)
	if err := appendFill(original); err != nil {
		t.Fatal(err)
	}
	check(3, 149, nil)
	revised := approvedLedgerFee(t, original, "fee-up", 3)
	if err := appendFill(revised); err != nil {
		t.Fatal(err)
	}
	debit = 103
	check(4, 147, &debit)
	reversal := approvedLedgerFee(t, revised, "fee-down", 0)
	if err := appendFill(reversal); err != nil {
		t.Fatal(err)
	}
	debit = 100
	check(5, 150, &debit)
	reject = true
	last := approvedLedgerFee(t, reversal, "fee-retry", 7)
	if err := appendFill(last); !errors.Is(err, refusal) {
		t.Fatalf("rollback=%v", err)
	}
	var head int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM cash_commit_heads WHERE portfolio_id='fund'`).Scan(&head); err != nil || head != 5 {
		t.Fatalf("failed append consumed source revision: %d %v", head, err)
	}
	reject = false
	st = NewPostgres(pool) // recreate the source; sequence and debit baseline are durable
	if err := appendFill(last); err != nil {
		t.Fatal(err)
	}
	debit = 107
	check(6, 143, &debit)
	pending, err := st.Outbox().Pending(ctx, "fund", 20)
	if err != nil || len(pending) != 6 {
		t.Fatalf("outbox count=%d err=%v", len(pending), err)
	}
	for i, record := range pending {
		var retained []byte
		if err := pool.QueryRow(ctx, `SELECT payload FROM cash_commit_history WHERE portfolio_id='fund' AND currency='USD' AND revision=$1`, i+1).Scan(&retained); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(retained, record.Record.Payload) {
			t.Fatal("retained source proof differs from the published fact")
		}
	}
	for _, sql := range []string{`UPDATE cash_commit_history SET payload='bad' WHERE portfolio_id='fund'`, `DELETE FROM cash_commit_history WHERE portfolio_id='fund'`} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Fatal("source history was mutable")
		}
	}
	// No evidence is fabricated for a legacy trade. The cash still books, but
	// the handoff explicitly stops authorizing further spending.
	legacy := tradeEvent("legacy", "BTC-USD", 1, 10, -10, day(2), day(2))
	legacy.PortfolioID = "fund"
	if err := st.Append(ctx, legacy, announce); err != nil {
		t.Fatal(err)
	}
	if latest.CashCommit.Complete || latest.CashCommit.UnattributedEntries != 1 || len(latest.CashCommit.Applied) != 0 {
		t.Fatalf("legacy history certified: %v", latest.CashCommit)
	}
	oversized = true
	if err := st.Append(ctx, cashOne("oversized", "fund", 10), announce); !errors.Is(err, ErrCashCommit) {
		t.Fatalf("oversized fact committed: %v", err)
	}
	oversized = false
	// A regressing source clock cannot allocate a new revision or book an entry.
	at = day(9)
	regression := cashOne("clock-regression", "fund", 10)
	if err := st.Append(ctx, regression, announce); !errors.Is(err, ErrCashCommit) {
		t.Fatalf("clock regression accepted: %v", err)
	}
}

func TestPostgresCashCommitTenantIsolation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	var bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil || bypass {
		t.Fatalf("requires non-bypass PostgreSQL role: %v %v", bypass, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO cash_commit_heads(portfolio_id,currency) VALUES('fund','USD')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO cash_commit_debits(portfolio_id,currency,order_id,debit) VALUES('fund','USD','order','100');
		INSERT INTO cash_commit_history(portfolio_id,currency,revision,payload) VALUES('fund','USD',1,'retained')`); err != nil {
		t.Fatal(err)
	}
	config := pool.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id','another-tenant',false)`)
		return err
	}
	other, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for _, table := range []string{"cash_commit_heads", "cash_commit_debits", "cash_commit_history"} {
		var forced bool
		if err := pool.QueryRow(ctx, `SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, table).Scan(&forced); err != nil || !forced {
			t.Fatalf("RLS not forced for %s: %v", table, err)
		}
		var count int
		if err := other.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cross-tenant %s read: %d %v", table, count, err)
		}
	}
	if _, err := other.Exec(ctx, `INSERT INTO cash_commit_heads(tenant_id,portfolio_id,currency) VALUES('__system__','foreign','USD')`); err == nil {
		t.Fatal("foreign tenant source write accepted")
	}
}

func TestPostgresCashCommitSequenceSurvivesCompetingWritersAndRollbacks(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()
	refusal := errors.New("rollback after sequence allocation")
	var wg sync.WaitGroup
	failures := make(chan error, 32)
	for i := range 32 {
		wg.Go(func() {
			e := cashOne(fmt.Sprintf("concurrent:%d", i), "fund", 1)
			e.Effective, e.Knowledge = day(1), day(1)
			err := st.Append(ctx, e, func(ctx context.Context, txStore Store) ([]outbox.Record, error) {
				projection, err := MaterializeCash(ctx, txStore, "fund", day(10))
				if err != nil {
					return nil, err
				}
				total, ok := dec.ToProtoExact(projection.Book.CashBalance("USD"))
				if !ok {
					return nil, ErrCashCommit
				}
				msg := &accountingpb.PortfolioCashBalance{PortfolioId: "fund", BaseCurrency: "USD", Total: total, AsOf: timestamppb.New(day(10)), KnowledgeTime: timestamppb.New(day(10))}
				if err := txStore.(*Postgres).SealCashBalance(ctx, msg, projection); err != nil {
					return nil, err
				}
				if i%4 == 0 {
					return nil, refusal
				}
				return nil, nil
			})
			if i%4 == 0 && errors.Is(err, refusal) {
				err = nil
			}
			failures <- err
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT revision,payload FROM cash_commit_history WHERE portfolio_id='fund' ORDER BY revision`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var count int64
	for rows.Next() {
		var revision int64
		var payload []byte
		if err := rows.Scan(&revision, &payload); err != nil {
			t.Fatal(err)
		}
		count++
		var msg accountingpb.PortfolioCashBalance
		if err := proto.Unmarshal(payload, &msg); err != nil || revision != count || msg.GetCashCommit().GetRevision() != count || dec.FromProto(msg.Total).Cmp(big.NewRat(count, 1)) != 0 {
			t.Fatalf("non-contiguous or misordered commit: rev=%d count=%d msg=%v err=%v", revision, count, &msg, err)
		}
	}
	if rows.Err() != nil || count != 24 {
		t.Fatalf("committed revisions=%d err=%v", count, rows.Err())
	}
}

func TestExecutionCoverageRejectsMalformedCorrectionsAndUsesCutoff(t *testing.T) {
	original := executionEvidenceFixture("account")
	entry, err := FromFill("fund", original, "USD", day(2))
	if err != nil {
		t.Fatal(err)
	}
	events := []*Event{entry, entry}
	projection := newCashProjection(ReplayAsOf("fund", events, day(1).Add(-time.Nanosecond), time.Time{}), events, day(1).Add(-time.Nanosecond))
	if debits, unknown := executionDebits(projection.events, "USD"); len(debits) != 0 || unknown != 0 {
		t.Fatal("future execution received coverage")
	}
	debits, unknown := executionDebits(events, "USD")
	if unknown != 0 || debits[original.OrderId].Cmp(big.NewRat(101, 1)) != 0 {
		t.Fatal("duplicate execution counted twice")
	}
	malformed := *entry
	malformed.EntryID = "bad"
	malformed.Type = EntryFee
	if _, unknown := executionDebits([]*Event{&malformed}, "USD"); unknown != 1 {
		t.Fatal("malformed fee correction certified")
	}
	changed := proto.Clone(original).(*orderpb.Fill)
	changed.Side = orderpb.Side_SIDE_SELL
	sale, err := FromFill("fund", changed, "USD", day(2))
	if err != nil {
		t.Fatal(err)
	}
	debits, unknown = executionDebits([]*Event{sale}, "USD")
	if unknown != 0 || debits[original.OrderId].Sign() != 0 {
		t.Fatal("sale proceeds counted as negative commitment")
	}
}
