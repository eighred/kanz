package custody

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func signedExample(t *testing.T, a BatchAuthority, key ed25519.PrivateKey, revision int64, previous string, position int) ([]byte, []byte) {
	t.Helper()
	// Independent synthetic custody side: the line and end-of-day cash agree
	// with the booked trade. The first reported position is short by one unit.
	stmt := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":"2020-02-04T00:00:00Z","positions":[{"instrumentId":"SYN","quantity":{"coefficient":"%d","exponent":-1}}],"cash":[{"currencyCode":"USD","balance":{"coefficient":"-105","exponent":-1}}],"transactions":[{"externalRef":"external-7","instrumentId":"SYN","quantity":{"coefficient":"105","exponent":-1},"price":{"coefficient":"1"},"cash":{"coefficient":"-105","exponent":-1},"currencyCode":"USD","tradeDate":"2020-02-04T00:00:00Z","settlementDate":"2020-02-04T00:00:00Z"}],"grain":"STATEMENT_GRAIN_TRANSACTIONS"}`, position))
	raw, err := json.Marshal(signedBatch{BatchFormat, a.Source, a.Tenant, a.Account, revision, previous, true, stmt})
	if err != nil {
		t.Fatal(err)
	}
	return raw, ed25519.Sign(key, raw)
}

func exampleAuthority(t *testing.T, tenant string) (BatchAuthority, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return BatchAuthority{Source: "synthetic-source", Tenant: tenant, Portfolio: "PF1", Custodian: "CUST-A", Account: "sandbox-account", PublicKey: base64.StdEncoding.EncodeToString(public), Instruments: []string{"SYN"}, Currencies: []string{"USD"}}, private
}

func TestSignedBatchRefusesIncompleteOrUnauthorizedInput(t *testing.T) {
	a, key := exampleAuthority(t, "fund-a")
	pairs := []string{"PF1:CUST-A"}
	v, err := newBatchVerifier(a, a.Tenant, pairs, "PF1:CUST-A:sandbox-account")
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedExample(t, a, key, 1, "", 95)
	if _, err = v.verify(raw, sig); err != nil {
		t.Fatal(err)
	}
	for _, declaration := range []string{"", "PF1:CUST-A:sandbox-account,another", "PF1:CUST-A:sandbox-account PF2:CUST-B:sandbox-account"} {
		if _, err = newBatchVerifier(a, a.Tenant, pairs, declaration); err == nil {
			t.Fatal("partial/ambiguous account authority accepted")
		}
	}
	if _, err = newBatchVerifier(a, "fund-b", pairs, "PF1:CUST-A:sandbox-account"); err == nil {
		t.Fatal("wrong tenant accepted")
	}
	if _, err = v.verify(raw, make([]byte, 64)); err == nil {
		t.Fatal("wrong signature accepted")
	}
	for _, mutate := range []func(*signedBatch){
		func(b *signedBatch) { b.Tenant = "fund-b" }, func(b *signedBatch) { b.Source = "other" }, func(b *signedBatch) { b.Account = "other" }, func(b *signedBatch) { b.Complete = false }, func(b *signedBatch) { b.Revision = 2 },
		func(b *signedBatch) {
			var rows map[string]json.RawMessage
			_ = json.Unmarshal(b.Statement, &rows)
			delete(rows, "cash")
			b.Statement, _ = json.Marshal(rows)
		},
	} {
		var h signedBatch
		if err = json.Unmarshal(raw, &h); err != nil {
			t.Fatal(err)
		}
		mutate(&h)
		changed, _ := json.Marshal(h)
		if _, err = v.verify(changed, ed25519.Sign(key, changed)); err == nil {
			t.Fatal("signed but invalid input accepted")
		}
	}
}

func TestSignedCustodyBatchReconciliationOnRealSpine(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" || os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("requires restricted PostgreSQL and JetStream")
	}
	tenant := fmt.Sprintf("batch-%d", time.Now().UnixNano())
	a, key := exampleAuthority(t, tenant)
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(bus.WithTenantID(t.Context(), tenant), 45*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "SIGNED_"+fmt.Sprint(time.Now().UnixNano()), []string{SubjectStatement, SubjectRun})
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "signed-custody-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	pub, err := bus.NewProducer(client, bus.ProducerConfig{Source: BatchProducer, ProducerVersion: "test", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	newImporter := func(p outbox.Publisher) *BatchImporter {
		i, err := NewBatchImporter(a, tenant, []string{"PF1:CUST-A"}, "PF1:CUST-A:sandbox-account", pool, p, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	st := NewPostgres(pool)
	owner, err := NewStatementConsumer(st, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	ready, done, accepted := make(chan struct{}), make(chan error, 1), make(chan error, 4)
	subCtx, stop := context.WithCancel(ctx)
	go func() {
		done <- consumer.SubscribeReady(subCtx, SubjectStatement, tenant, func(c context.Context, e *envelopepb.Envelope, payload []byte) error {
			if e.GetTenantId() != tenant {
				return nil
			}
			err := owner.Handle(c, e, payload)
			accepted <- err
			return err
		}, func() { close(ready) })
	}()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("consumer did not join")
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitAccepted := func() {
		t.Helper()
		select {
		case err := <-accepted:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	day := time.Date(2020, 2, 4, 0, 0, 0, 0, time.UTC)
	subject := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: day}
	scope, err := NewBookScope([]Subject{subject}, map[string]map[string][]string{"PF1": {"CUST-A": {"sandbox-account"}}})
	if err != nil {
		t.Fatal(err)
	}
	journal := ledger.NewPostgres(pool)
	err = journal.Append(ctx, &ledger.Event{EntryID: "trade7", SourceRef: "external-7", PortfolioID: "PF1", VenueAccountID: "sandbox-account", Type: ledger.EntryTrade, InstrumentID: "SYN", Quantity: big.NewRat(21, 2), Price: big.NewRat(1, 1), Cash: big.NewRat(-21, 2), CashCurrency: "USD", Effective: day, Knowledge: day, SettlementBasis: ledger.SettlementSettled, SettlementDate: day}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewReconciler(st, LedgerBookLoader(journal, scope, testLogger()), pub, new(big.Rat), nil, testLogger(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	run, err := reconciler.Reconcile(ctx, subject)
	if err != nil || run.Outcome != OutcomeNoStatement {
		t.Fatalf("missing level %v %v", run, err)
	}
	raw, sig := signedExample(t, a, key, 1, "", 95)
	interrupted := newImporter(&capturePublisher{err: errors.New("fixture broker interrupted")})
	id, err := interrupted.Import(ctx, raw, sig)
	if err == nil || id == "" {
		t.Fatal("pending receipt was not returned")
	}
	i := newImporter(pub)
	again, err := i.Import(ctx, raw, sig)
	if err != nil || again != id {
		t.Fatalf("restart lost receipt: %v", err)
	}
	waitAccepted()
	run, err = reconciler.Reconcile(ctx, subject)
	if err != nil || run.StatementID != id || run.Outcome != OutcomeBreaks || len(run.Breaks) != 1 || run.Breaks[0].Key != "SYN" || run.Breaks[0].Diff.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("want only the one-unit position discrepancy; got %+v %v", run, err)
	}
	if _, err = i.Import(ctx, raw, sig); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	wrong, wrongSig := signedExample(t, a, key, 1, "", 105)
	if _, err = i.Import(ctx, wrong, wrongSig); err == nil {
		t.Fatal("mutable revision accepted")
	}
	gap, gapSig := signedExample(t, a, key, 3, digest, 105)
	if _, err = i.Import(ctx, gap, gapSig); err == nil {
		t.Fatal("missing preceding revision accepted")
	}
	correction, cs := signedExample(t, a, key, 2, digest, 105)
	corrected, err := i.Import(ctx, correction, cs)
	if err != nil || corrected == id {
		t.Fatal("invalid correction", err)
	}
	waitAccepted()
	run, err = reconciler.Reconcile(ctx, subject)
	if err != nil || run.StatementID != corrected || run.Outcome != OutcomeClean {
		t.Fatalf("correction %+v %v", run, err)
	}
	other := newCustodyPool(t, tenant+"-other")
	var count int
	if err = other.QueryRow(ctx, "SELECT count(*) FROM custody_batch_receipts").Scan(&count); err != nil || count != 0 {
		t.Fatal("foreign tenant visible", err)
	}
	foreign, err := NewBatchImporter(a, tenant, []string{"PF1:CUST-A"}, "PF1:CUST-A:sandbox-account", other, pub, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = foreign.Import(ctx, raw, sig); err == nil {
		t.Fatal("same account accepted through another tenant's DB session")
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE producer_service=$1", BatchProducer).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate outbox publication count %d: %v", count, err)
	}
	var first []byte
	if err = pool.QueryRow(ctx, "SELECT signed_batch FROM custody_batch_receipts WHERE statement_id=$1", id).Scan(&first); err != nil || !ed25519.Verify(i.verify.key, first, sig) {
		t.Fatal("original signed evidence lost", err)
	}
}
