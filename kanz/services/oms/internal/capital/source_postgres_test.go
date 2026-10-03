package capital

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func sourcePayload(t *testing.T, edit func(*accountingpb.PortfolioCashBalance)) []byte {
	t.Helper()
	m := coverageBalance()
	m.CashCommit.Applied = nil
	if edit != nil {
		edit(m)
	}
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBadSourceEvidenceQuarantinesExistingAndFuturePortfolios(t *testing.T) {
	for name, bad := range map[string][]byte{
		"undecodable":      {0xff},
		"legacy":           sourcePayload(t, func(m *accountingpb.PortfolioCashBalance) { m.CashCommit = nil }),
		"missing identity": sourcePayload(t, func(m *accountingpb.PortfolioCashBalance) { m.PortfolioId = "" }),
		"unrepresentable time": sourcePayload(t, func(m *accountingpb.PortfolioCashBalance) {
			m.AsOf = timestamppb.New(time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC))
			m.KnowledgeTime = m.AsOf
		}),
		"oversized": make([]byte, (512<<10)+1),
	} {
		t.Run(name, func(t *testing.T) {
			p := database(t)
			ctx := context.Background()
			if err := ApplyPayload(ctx, p, sourcePayload(t, nil)); err != nil {
				t.Fatal(err)
			}
			now := coverageBalance().AsOf.AsTime()
			reserve := func(fund, id string) error {
				return transaction(p, func(tx pgx.Tx) error {
					return Reserve(ctx, tx, fund, "USD", id, "100", now, time.Minute)
				})
			}
			if err := reserve("fund", "existing"); err != nil {
				t.Fatal(err)
			}
			if err := ApplyPayload(ctx, p, bad); !errors.Is(err, ErrUnknown) && !errors.Is(err, ErrInvalid) {
				t.Fatalf("source failure not committed: %v", err)
			}
			// Another pool is the post-restart view; later healthy facts and newly
			// discovered portfolios must not erase the malformed source evidence.
			restarted := tenantPool(t, "tenant-a")
			for _, fund := range []string{"fund", "new-fund"} {
				payload := sourcePayload(t, func(m *accountingpb.PortfolioCashBalance) {
					m.PortfolioId = fund
					if fund == "fund" {
						m.CashCommit.Revision = 2
					}
				})
				if err := ApplyPayload(ctx, restarted, payload); err != nil {
					t.Fatal(err)
				}
				if err := reserve(fund, fund+"-refused"); !errors.Is(err, ErrUnknown) {
					t.Fatalf("quarantined source admitted %s: %v", fund, err)
				}
			}
			if err := transaction(p, func(tx pgx.Tx) error {
				return Change(ctx, tx, "fund", "USD", "existing", 1, "101", now, time.Minute)
			}); !errors.Is(err, ErrUnknown) {
				t.Fatalf("unsafe increase: %v", err)
			}
			// Facts and confirmed reductions still commit during quarantine.
			if err := transaction(p, func(tx pgx.Tx) error {
				if err := ObserveExecution(ctx, tx, "fund", "USD", "existing", "20"); err != nil {
					return err
				}
				return Change(ctx, tx, "fund", "USD", "existing", 2, "20", now, time.Minute)
			}); err != nil {
				t.Fatal(err)
			}
			assertReserved(t, p, "20")
			other := tenantPool(t, "tenant-b")
			if err := ApplyPayload(ctx, other, sourcePayload(t, nil)); err != nil {
				t.Fatal(err)
			}
			if err := transaction(other, func(tx pgx.Tx) error {
				return Reserve(ctx, tx, "fund", "USD", "other", "100", now, time.Minute)
			}); err != nil {
				t.Fatalf("fault leaked across RLS: %v", err)
			}
			var digest []byte
			if err := restarted.QueryRow(ctx, `SELECT fault_digest FROM capital_source_health`).Scan(&digest); err != nil {
				t.Fatal(err)
			}
			want := sha256.Sum256(bad)
			if string(digest) != string(want[:]) {
				t.Fatal("first source fault lost")
			}
			for _, sql := range []string{`UPDATE capital_source_health SET fault_digest=NULL`, `DELETE FROM capital_source_health`} {
				if _, err := p.Exec(ctx, sql); err == nil {
					t.Fatalf("source fault erased: %s", sql)
				}
			}
		})
	}
}

func TestSourceFaultCannotOvertakeAnAdmissionTransaction(t *testing.T) {
	p := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Apply(ctx, p, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := Reserve(ctx, tx, "fund", "USD", "before", "100", snapshot(1, "250").ObservedAt, time.Minute); err != nil {
		t.Fatal(err)
	}
	fault := make(chan error, 1)
	go func() { fault <- ApplyPayload(ctx, p, []byte{0xff}) }()
	// Inspect PostgreSQL's actual lock wait instead of assuming a scheduling
	// delay proves exclusion. The source writer must wait on the reader's lock.
	for {
		var blocked bool
		if err := p.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
			AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO capital_source_health(fault_digest)%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-fault:
			t.Fatalf("invalidation overtook admission: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-fault; !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := transaction(p, func(tx pgx.Tx) error {
		return Reserve(ctx, tx, "fund", "USD", "after", dec.Exact("1"), snapshot(1, "250").ObservedAt, time.Minute)
	}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("later admission ignored committed fault: %v", err)
	}
	assertReserved(t, p, "100")
}

func TestSourceFaultWriteFailureIsRetryable(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	if _, err := p.Exec(ctx, `CREATE FUNCTION fail_source_fault() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected source storage outage'; END $$;
		CREATE TRIGGER fail_source BEFORE INSERT ON capital_source_health FOR EACH ROW EXECUTE FUNCTION fail_source_fault()`); err != nil {
		t.Fatal(err)
	}
	err := ApplyPayload(ctx, p, []byte{0xff})
	if err == nil || errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnknown) {
		t.Fatalf("storage failure disguised as committed refusal: %v", err)
	}
	if _, err := p.Exec(ctx, `DROP TRIGGER fail_source ON capital_source_health`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPayload(ctx, p, []byte{0xff}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := ApplyPayload(ctx, p, sourcePayload(t, nil)); err != nil {
		t.Fatal(err)
	}
	if err := transaction(p, func(tx pgx.Tx) error {
		return Reserve(ctx, tx, "fund", "USD", "refused", "1", coverageBalance().AsOf.AsTime(), time.Minute)
	}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("fault before initial balance was forgotten: %v", err)
	}
}

func TestHealthySourceAllowsIndependentPortfolioTransactions(t *testing.T) {
	p := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, fund := range []string{"first", "second"} {
		e := snapshot(1, "100")
		e.PortfolioID = fund
		if err := Apply(ctx, p, e); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now := snapshot(1, "100").ObservedAt
	if err := Reserve(ctx, tx, "first", "USD", "held", "50", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	// The first transaction deliberately stays open. A tenant-wide exclusive
	// lock would deadlock progress here even though the cash pools are distinct.
	if err := transaction(p, func(other pgx.Tx) error {
		return Reserve(ctx, other, "second", "USD", "parallel", "50", now, time.Minute)
	}); err != nil {
		t.Fatalf("unrelated portfolio blocked by source reader: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
