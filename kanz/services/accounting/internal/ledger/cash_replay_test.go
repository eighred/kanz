package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func appendReplayFact(t *testing.T, store *Postgres, id string, padding int) {
	t.Helper()
	e := cashOne(id, "fund", 1)
	e.Effective, e.Knowledge = day(1), day(1)
	if err := store.Append(t.Context(), e, func(ctx context.Context, txStore Store) ([]outbox.Record, error) {
		projection, err := MaterializeCash(ctx, txStore, "fund", day(10))
		if err != nil {
			return nil, err
		}
		total, ok := dec.ToProtoExact(projection.Book.CashBalance("USD"))
		if !ok {
			return nil, ErrCashCommit
		}
		msg := &accountingpb.PortfolioCashBalance{PortfolioId: "fund", BaseCurrency: "USD", Total: total, AsOf: timestamppb.New(day(10)), KnowledgeTime: timestamppb.New(day(10))}
		if padding > 0 {
			// An opaque future schema extension must survive replay byte-for-byte,
			// and still count against the response's byte budget.
			unknown := protowire.AppendTag(nil, 500, protowire.BytesType)
			msg.ProtoReflect().SetUnknown(protowire.AppendBytes(unknown, bytes.Repeat([]byte{1}, padding)))
		}
		return nil, txStore.(*Postgres).SealCashBalance(ctx, msg, projection)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresCashReplayPinsHeadAndPreservesOriginalEvidence(t *testing.T) {
	pool := newPool(t)
	store := NewPostgres(pool)
	for i := range 3 {
		appendReplayFact(t, store, fmt.Sprint(i), 0)
	}
	page, err := store.ReadCashCommits(t.Context(), "fund", "USD", 0, 0, 2)
	if err != nil || page.Through != 3 || page.Next != 2 || !page.HasMore || len(page.Records) != 2 {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	appendReplayFact(t, store, "later", 0)
	next, err := NewPostgres(pool).ReadCashCommits(t.Context(), "fund", "USD", page.Next, page.Through, 2)
	if err != nil || next.Through != 3 || next.Next != 3 || next.HasMore || len(next.Records) != 1 {
		t.Fatalf("restart lost frozen watermark: %+v err=%v", next, err)
	}
	for _, record := range append(page.Records, next.Records...) {
		var retained []byte
		if err := pool.QueryRow(t.Context(), `SELECT payload FROM cash_commit_history WHERE portfolio_id='fund' AND currency='USD' AND revision=$1`, record.Revision).Scan(&retained); err != nil || !bytes.Equal(retained, record.Payload) {
			t.Fatalf("history re-encoded or changed: %v", err)
		}
		var msg accountingpb.PortfolioCashBalance
		if err := proto.Unmarshal(record.Payload, &msg); err != nil || dec.FromProto(msg.Total).Cmp(big.NewRat(record.Revision, 1)) != 0 {
			t.Fatalf("wrong original financial evidence: %v", err)
		}
	}
	finished, err := store.ReadCashCommits(t.Context(), "fund", "USD", 3, 3, 2)
	if err != nil || len(finished.Records) != 0 || finished.HasMore {
		t.Fatalf("finished page=%+v err=%v", finished, err)
	}
	fresh, err := store.ReadCashCommits(t.Context(), "fund", "USD", 3, 0, 2)
	if err != nil || fresh.Through != 4 || fresh.Next != 4 || len(fresh.Records) != 1 {
		t.Fatalf("new watermark omitted later commit: %+v err=%v", fresh, err)
	}
	for _, bounds := range [][3]int64{{-1, 0, 1}, {0, -1, 1}, {4, 3, 1}, {0, 9, 1}, {9, 0, 1}, {0, 0, 0}, {0, 0, 65}} {
		if _, err := store.ReadCashCommits(t.Context(), "fund", "USD", bounds[0], bounds[1], int(bounds[2])); !errors.Is(err, ErrCashReplayBounds) {
			t.Fatalf("unsafe bounds %v: %v", bounds, err)
		}
	}
	config := pool.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id','replay-other',false)`)
		return err
	}
	other, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := NewPostgres(other).ReadCashCommits(t.Context(), "fund", "USD", 0, 0, 2); !errors.Is(err, ErrNoCashHistory) {
		t.Fatalf("foreign tenant history disclosed: %v", err)
	}
}

func TestPostgresCashReplayByteBudgetAndUnknownFields(t *testing.T) {
	store := NewPostgres(newPool(t))
	for i := range 10 {
		appendReplayFact(t, store, fmt.Sprint(i), 480<<10)
	}
	page, err := store.ReadCashCommits(t.Context(), "fund", "USD", 0, 0, 64)
	if err != nil || len(page.Records) != 8 || page.Next != 8 || page.Through != 10 || !page.HasMore {
		t.Fatalf("byte budget: count=%d next=%d through=%d more=%v err=%v", len(page.Records), page.Next, page.Through, page.HasMore, err)
	}
	size := 0
	for _, record := range page.Records {
		size += len(record.Payload)
		var msg accountingpb.PortfolioCashBalance
		if err := proto.Unmarshal(record.Payload, &msg); err != nil || len(msg.ProtoReflect().GetUnknown()) < 480<<10 {
			t.Fatalf("future schema evidence stripped: %v", err)
		}
	}
	if size > maxCashReplayBytes {
		t.Fatal("response exceeded byte budget")
	}
	wire, err := json.Marshal(page)
	if err != nil || len(wire) > 6<<20 {
		t.Fatalf("base64/JSON expansion exceeded the response budget: bytes=%d err=%v", len(wire), err)
	}
	next, err := store.ReadCashCommits(t.Context(), "fund", "USD", page.Next, page.Through, 64)
	if err != nil || len(next.Records) != 2 || next.HasMore || next.Next != 10 {
		t.Fatalf("byte-bound continuation lost records: %v", err)
	}
}

func TestPostgresCashReplayRefusesHolesAndPreservesInt64Cursors(t *testing.T) {
	pool := newPool(t)
	store := NewPostgres(pool)
	const large = int64(9007199254740993)
	for _, pf := range []string{"gap", "large"} {
		head := int64(3)
		revisions := []int64{1, 3}
		if pf == "large" {
			head, revisions = large, []int64{large}
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO cash_commit_heads(portfolio_id,currency,revision) VALUES($1,'USD',$2)`, pf, head); err != nil {
			t.Fatal(err)
		}
		for _, revision := range revisions {
			body, err := proto.Marshal(&accountingpb.PortfolioCashBalance{PortfolioId: pf, BaseCurrency: "USD", CashCommit: &accountingpb.CashCommitCoverage{Revision: revision}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO cash_commit_history(portfolio_id,currency,revision,payload) VALUES($1,'USD',$2,$3)`, pf, revision, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if page, err := store.ReadCashCommits(t.Context(), "gap", "USD", 0, 0, 64); !errors.Is(err, ErrCashCommit) || len(page.Records) != 0 {
		t.Fatalf("partial history certified: %+v %v", page, err)
	}
	page, err := store.ReadCashCommits(t.Context(), "large", "USD", large-1, large, 1)
	if err != nil || page.Next != large {
		t.Fatalf("large cursor lost: %v", err)
	}
	body, err := json.Marshal(page)
	if err != nil || !strings.Contains(string(body), `"next_revision":"9007199254740993"`) || !strings.Contains(string(body), `"revision":"9007199254740993"`) {
		t.Fatalf("JSON cursor may round: %s %v", body, err)
	}
}
