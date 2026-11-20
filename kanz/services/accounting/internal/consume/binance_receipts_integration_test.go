package consume_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/cashview"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	nserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// With a disposable restricted TEST_POSTGRES_URL, run from the kanz module:
//
//	go test -p 1 -c -o /tmp/binance-receipts.test ./services/venue_binance/internal/binance
//	TEST_BINANCE_RECEIPT_PROBE=/tmp/binance-receipts.test go test -p 1 -count=1 -run "^TestBinanceBrokerFramesBookOnceAcrossRestart$" -v ./services/accounting/internal/consume
//
// A separate adapter process owns the signed loopback REST/WS sessions and its PostgreSQL order store.
// Accounting consumes the actual retained frames from a dedicated JetStream.
func TestBinanceBrokerFramesBookOnceAcrossRestart(t *testing.T) {
	const tenant = "fund-alpha"
	probe, dsn := os.Getenv("TEST_BINANCE_RECEIPT_PROBE"), os.Getenv("TEST_POSTGRES_URL")
	if probe == "" || dsn == "" {
		t.Skip("requires the Binance integration test binary and a disposable restricted PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	pool := cashTimeRestrictedPool(t, ctx, dsn, tenant)
	server, err := nserver.NewServer(&nserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoSigs: true, NoLog: true})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	t.Cleanup(func() { server.Shutdown(); server.WaitForShutdown() })
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("private JetStream startup failed")
	}
	url := server.ClientURL()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "BINANCE_RECEIPTS", []string{fillfact.SubjectFilled})
	const probeTest = "TestVerifiedAccountsShareRawTradeAliasAcrossAdapterRestart"
	// Supply only isolated test endpoints and platform paths to the child.
	cmd := exec.CommandContext(ctx, probe, "-test.run=^"+probeTest+"$", "-test.count=1", "-test.v")
	cmd.Dir = filepath.Join("..", "..", "..", "venue_binance", "internal", "binance")
	cmd.Env = []string{"TEST_NATS_URL=" + url, "TEST_POSTGRES_URL=" + dsn}
	for _, key := range []string{"SystemRoot", "TEMP", "TMP", "TMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("--- PASS: "+probeTest+" ")) {
		t.Fatalf("Binance producer %s did not pass (child process error %v)", probeTest, err)
	}
	source := cashTimeStream(t, ctx, js, fillfact.SubjectFilled)
	sinfo, err := source.Info(ctx)
	if err != nil || sinfo.State.Msgs != 4 {
		t.Fatalf("expected 4 unchanged producer frames, info=%v err=%v", sinfo, err)
	}
	bustest.EnsureSubjects(t, ctx, js, "CASH_PROOF", []string{cashview.Subject})
	cashStream := cashTimeStream(t, ctx, js, cashview.Subject)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "account-proof"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pub, err := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "fixture", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	scoped := map[string]*orderpb.Fill{}
	hashes := map[uint64][32]byte{}
	expected := big.NewRat(500001, 10)
	for pass := 0; pass < 2; pass++ {
		st := ledger.NewPostgres(pool)
		ann := consume.NewAnnouncer(st, pub, "USDT", consume.EntrySourcePosture{Produced: []string{"trade"}, Unproduced: []string{"cash", "fee", "accrual", "corporate_action"}}, nil, time.Now)
		folder, err := consume.NewFolder(tenant, st, "USDT", consume.WithAnnouncer(ann))
		if err != nil {
			t.Fatal(err)
		}
		reader, err := source.CreateConsumer(ctx, jetstream.ConsumerConfig{Durable: fmt.Sprintf("accounting-generation-%d", pass), FilterSubject: fillfact.SubjectFilled, DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		for range 4 {
			m, err := reader.Next(jetstream.FetchMaxWait(4 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			meta, err := m.Metadata()
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(m.Data())
			if pass == 0 {
				hashes[meta.Sequence.Stream] = hash
			} else if hashes[meta.Sequence.Stream] != hash {
				t.Fatal("broker replay changed bytes")
			}
			env, body, err := bus.Unframe(m.Data())
			if err != nil || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetEventType() != fillfact.SubjectFilled || env.GetTenantId() != tenant || env.GetSource() != "venue-binance" || m.Subject() != fillfact.SubjectFilled {
				t.Fatal("wrong producer envelope", env, err)
			}
			var receipt orderpb.OrderFilled
			if err := proto.Unmarshal(body, &receipt); err != nil {
				t.Fatal(err)
			}
			fill := receipt.GetFill()
			account := fill.GetVenueAccountId()
			if account != "desk-a" && account != "desk-b" || fill.GetOrderId() != "order-"+account || env.GetPartitionKey() != fill.GetOrderId() || fill.GetVenueExecutionId() != "0" || fill.GetFillId() != "BTCUSDT-0" {
				t.Fatal("unidentified execution", fill)
			}
			if orig := scoped[account]; orig != nil && !fillfact.SameExecution(orig, fill) {
				t.Fatal("changed receipt")
			}
			scoped[account] = fill
			foldCtx := bus.WithCausationID(bus.WithCorrelationID(bus.WithTenantID(ctx, env.TenantId), env.CorrelationId), env.EventId)
			if err := folder.Handle(foldCtx, env, body); err != nil {
				t.Fatal(err)
			}
			if err := m.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
			t.Logf("pass=%d sequence=%d original-frame-sha256=%x source=%s account=%s", pass+1, meta.Sequence.Stream, hash, env.Source, account)
		}
		if len(hashes) != 4 || len(scoped) != 2 || fillfact.ExecutionKey(scoped["desk-a"]) == fillfact.ExecutionKey(scoped["desk-b"]) {
			t.Fatal("aliases were conflated")
		}
		rows, err := st.Journal(ctx, tenant)
		if err != nil || len(rows) != 2 {
			t.Fatal("journal entries", len(rows), err)
		}
		seen := map[string]bool{}
		for _, row := range rows {
			var original orderpb.Fill
			if err := proto.Unmarshal(row.ExecutionEvidence, &original); err != nil {
				t.Fatal(err)
			}
			acct := row.VenueAccountID
			if seen[acct] || !proto.Equal(&original, scoped[acct]) || row.EntryID != "fill:"+fillfact.ExecutionKey(scoped[acct]) || row.SourceRef != "BTCUSDT-0" || row.CashCurrency != "USDT" || row.Cash.Cmp(new(big.Rat).Neg(expected)) != 0 {
				t.Fatal("booked evidence differs", row)
			}
			seen[acct] = true
		}
		var debitCount int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM cash_commit_debits WHERE portfolio_id=$1 AND currency='USDT'", tenant).Scan(&debitCount); err != nil || debitCount != 2 {
			t.Fatal("debit count", debitCount, err)
		}
		p, err := ledger.MaterializeCash(ctx, st, tenant, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for acct := range scoped {
			var debit string
			if err := pool.QueryRow(ctx, "SELECT debit FROM cash_commit_debits WHERE portfolio_id=$1 AND currency='USDT' AND order_id=$2", tenant, "order-"+acct).Scan(&debit); err != nil || debit != expected.RatString() {
				t.Fatal("persistent debit", acct, debit, err)
			}
			if cash := p.Accounts[acct]["USDT"]; cash == nil || cash.Cmp(new(big.Rat).Neg(expected)) != 0 {
				t.Fatal("account cash", acct, cash)
			}
		}
		if p.Book.CashBalance("USDT").Cmp(big.NewRat(-500001, 5)) != 0 {
			t.Fatal("incorrect net cash")
		}
	}
	cinfo, err := cashStream.Info(ctx)
	if err != nil || cinfo.State.Msgs == 0 {
		t.Fatal("no accounting cash FACT", err)
	}
	reader, err := cashStream.CreateConsumer(ctx, jetstream.ConsumerConfig{FilterSubject: cashview.Subject, DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	applied := map[string]int{}
	for i := uint64(1); i <= cinfo.State.Msgs; i++ {
		m, err := reader.Next(jetstream.FetchMaxWait(4 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env, b, err := bus.Unframe(m.Data())
		if err != nil || env.GetSource() != "accounting" || env.GetTenantId() != tenant || env.GetEventType() != cashview.Subject || env.GetPartitionKey() != tenant || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
			t.Fatal("bad accounting FACT", err)
		}
		var balance accountingpb.PortfolioCashBalance
		if err := proto.Unmarshal(b, &balance); err != nil || balance.GetPortfolioId() != tenant || balance.GetBaseCurrency() != "USDT" || balance.GetCashCommit().GetRevision() != int64(i) || !balance.GetCashCommit().GetComplete() {
			t.Fatal("bad commit", i, err, &balance)
		}
		for _, v := range balance.GetCashCommit().GetApplied() {
			if dec.FromProto(v.GetDebit()).Cmp(expected) != 0 {
				t.Fatal("bad applied debit", v)
			}
			applied[v.GetOrderId()]++
		}
		if i == cinfo.State.Msgs && dec.FromProto(balance.GetTotal()).Cmp(big.NewRat(-500001, 5)) != 0 {
			t.Fatal("cash FACT total", balance.GetTotal())
		}
		if err := m.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(applied) != 2 || applied["order-desk-a"] != 1 || applied["order-desk-b"] != 1 {
		t.Fatal("replayed debit counted again", applied)
	}
	t.Logf("verified 4 venue FACT frames read twice across consumer/folder reconstruction; 2 exact source-backed ledger trades; 2 debits of 50000.1 USDT; net -100000.2 USDT; %d durable Accounting cash FACTs, each original debit applied once", cinfo.State.Msgs)
}
