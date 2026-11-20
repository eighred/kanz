package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// Run against a freshly provisioned synthetic database with distinct source and
// accounting identities. Both signed files and the broker grant are versioned.
func TestSignedPilotCommandRestricted(t *testing.T) {
	checkSignedPilotCommand(t, 20*time.Second, nil)
}

func checkSignedPilotCommand(t *testing.T, limit time.Duration, afterReceipt func(context.Context, *pgxpool.Pool, custody.Statement, int, pilotBroker)) {
	t.Helper()
	fixture := "testdata"
	if os.Getenv("CUSTODY_PILOT_QUANTITY_FIXTURE") == "1" {
		fixture = filepath.Join(fixture, "instrument_units")
	}
	checkPilotSeries(t, limit, fixture, nil, []pilotStatementFile{
		{name: "statement-r1", revision: 1, position: big.NewRat(19, 2), trade: true},
		{name: "statement-r2", revision: 2, position: big.NewRat(21, 2), trade: true},
	}, afterReceipt)
}

type pilotStatementFile struct {
	name      string
	revision  int
	dayOffset int
	position  *big.Rat
	cash      *big.Rat
	trade     bool
}

func checkPilotSeries(t *testing.T, limit time.Duration, fixture string, beforeIntake func(context.Context, *pgxpool.Pool, pilotBroker, time.Time), files []pilotStatementFile, afterReceipt func(context.Context, *pgxpool.Pool, custody.Statement, int, pilotBroker)) {
	t.Helper()
	source, owner := os.Getenv("TEST_CUSTODY_BATCH_DATABASE_URL"), os.Getenv("TEST_POSTGRES_URL")
	if source == "" || owner == "" {
		t.Skip("requires isolated provisioned pilot source and owner")
	}
	processes := beforeIntake != nil
	broker := grantBroker(t, processes)
	const tenant = "synthetic-custody-pilot"
	t.Setenv("ACCOUNTING_DATABASE_URL", source)
	t.Setenv("ACCOUNTING_NATS_URL", broker.address)
	t.Setenv("ACCOUNTING_TENANT", tenant)
	t.Setenv("ACCOUNTING_CUSTODY_PAIRS", "PF1:CUST-A")
	t.Setenv("ACCOUNTING_CUSTODY_ACCOUNTS", "PF1:CUST-A:sandbox-account")
	t.Setenv("ACCOUNTING_CUSTODY_SIGNED_BATCH_ONLY", "true")
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	pool, err := pg.NewTenantPool(ctx, owner, tenant)
	if err != nil {
		t.Fatal("accounting tenant database unavailable")
	}
	defer pool.Close()
	var limited bool
	if err := pool.QueryRow(ctx, "SELECT NOT rolsuper AND NOT rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&limited); err != nil || !limited {
		t.Fatal("restricted accounting reader required")
	}
	store := custody.NewPostgres(pool)
	var receiver *custody.StatementConsumer
	if processes {
		_, stop := startPilotAccounting(t, ctx, broker, "pilot-ingest", time.Time{})
		defer stop()
	} else {
		receiver, err = custody.NewSignedBatchConsumer(store, tenant, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	firstDay := time.Date(2020, 2, 4, 0, 0, 0, 0, time.UTC)
	if beforeIntake != nil {
		beforeIntake(ctx, pool, broker, firstDay)
	}
	for index, file := range files {
		sequence := uint64(index + 1)
		subject := custody.Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: firstDay.AddDate(0, 0, file.dayOffset)}
		batch := filepath.Join(fixture, file.name+".json")
		sig := filepath.Join(fixture, file.name+".sig")
		policy := filepath.Join(fixture, "authority.json")
		for attempt := 0; attempt < 2; attempt++ {
			if processes {
				runPilotImporter(t, ctx, source, broker.address, policy, batch, sig)
			} else if err := run(ctx, policy, batch, sig); err != nil {
				t.Fatalf("revision %d attempt %d: %v", file.revision, attempt+1, err)
			}
		}
		info, err := broker.statements.Info(ctx)
		if err != nil || info.State.Msgs != sequence {
			t.Fatalf("unexpected publication count at revision %d", file.revision)
		}
		msg, err := broker.statements.GetMsg(ctx, sequence)
		if err != nil || msg.Subject != custody.SubjectStatement {
			t.Fatal("canonical statement missing")
		}
		var frame envelopepb.EventFrame
		if err := proto.Unmarshal(msg.Data, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.GetEnvelope().GetSource() != custody.BatchProducer || frame.GetEnvelope().GetTenantId() != tenant {
			t.Fatal("unexpected frame scope")
		}
		if processes {
			awaitPilotStatementAck(t, ctx, broker, "pilot-ingest", sequence)
		} else if err := receiver.Handle(ctx, frame.Envelope, frame.Payload); err != nil {
			t.Fatal(err)
		}
		stored, err := store.LatestStatement(ctx, subject)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(batch)
		if err != nil {
			t.Fatal(err)
		}
		wantID := fmt.Sprintf("custody-batch.v1.synthetic-source.r%d.%x", file.revision, sha256.Sum256(raw))
		wantCash := file.cash
		if wantCash == nil {
			wantCash = big.NewRat(-21, 2)
		}
		position := stored.Positions["SYN"]
		if stored.StatementID != wantID || position == nil || position.Cmp(file.position) != 0 ||
			stored.Cash["USD"] == nil || stored.Cash["USD"].Cmp(wantCash) != 0 {
			t.Fatal("statement, revision or lineage differs on accounting readback")
		}
		if file.trade {
			if len(stored.Transactions) != 1 || stored.Transactions[0].ExternalRef != "external-7" {
				t.Fatal("expected the original external execution in the dated statement")
			}
		} else if len(stored.Transactions) != 0 {
			t.Fatal("the following close contains an unexpected trade")
		}
		t.Logf("revision %d stored %s at %s", file.revision, wantID, stored.ReceivedAt.UTC().Format(time.RFC3339Nano))
		if afterReceipt != nil {
			afterReceipt(ctx, pool, stored, index+1, broker)
		}
	}
}
