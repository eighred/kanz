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
	source, owner := os.Getenv("TEST_CUSTODY_BATCH_DATABASE_URL"), os.Getenv("TEST_POSTGRES_URL")
	if source == "" || owner == "" {
		t.Skip("requires isolated provisioned pilot source and owner")
	}
	broker := grantBroker(t)
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
	consumer, err := custody.NewSignedBatchConsumer(store, tenant, nil)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2020, 2, 4, 0, 0, 0, 0, time.UTC)
	subject := custody.Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: day}
	fixture := "testdata"
	if os.Getenv("CUSTODY_PILOT_QUANTITY_FIXTURE") == "1" {
		fixture = filepath.Join(fixture, "instrument_units")
	}
	for revision, expected := range []*big.Rat{big.NewRat(19, 2), big.NewRat(21, 2)} {
		name := fmt.Sprintf("statement-r%d", revision+1)
		batch := filepath.Join(fixture, name+".json")
		sig := filepath.Join(fixture, name+".sig")
		policy := filepath.Join(fixture, "authority.json")
		for attempt := 0; attempt < 2; attempt++ {
			if err := run(ctx, policy, batch, sig); err != nil {
				t.Fatalf("revision %d attempt %d: %v", revision+1, attempt+1, err)
			}
		}
		info, err := broker.statements.Info(ctx)
		if err != nil || info.State.Msgs != uint64(revision+1) {
			t.Fatalf("unexpected publication count at revision %d", revision+1)
		}
		msg, err := broker.statements.GetMsg(ctx, uint64(revision+1))
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
		if err := consumer.Handle(ctx, frame.Envelope, frame.Payload); err != nil {
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
		wantID := fmt.Sprintf("custody-batch.v1.synthetic-source.r%d.%x", revision+1, sha256.Sum256(raw))
		position := stored.Positions["SYN"]
		if stored.StatementID != wantID || position == nil || position.Cmp(expected) != 0 || len(stored.Transactions) != 1 || stored.Transactions[0].ExternalRef != "external-7" || stored.Cash["USD"] == nil || stored.Cash["USD"].Cmp(big.NewRat(-21, 2)) != 0 {
			t.Fatal("statement, revision or lineage differs on accounting readback")
		}
		t.Logf("revision %d stored %s at %s", revision+1, wantID, stored.ReceivedAt.UTC().Format(time.RFC3339Nano))
		if afterReceipt != nil {
			afterReceipt(ctx, pool, stored, revision+1, broker)
		}
	}
}
