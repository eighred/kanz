package cashmove_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	pb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func poolFor(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("requires real PostgreSQL")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SELECT set_config('app.tenant_id',$1,false)`, tenant)
		return err
	}
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	var privileged bool
	if err = p.QueryRow(t.Context(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("non-bypass RLS role required: %v %v", privileged, err)
	}
	return p
}
func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p := poolFor(t, "tenant-A")
	if _, err := p.Exec(t.Context(), `DROP TABLE IF EXISTS cash_commands,collateral_allocation_proofs,collateral_confirmations,collateral_requests,collateral_reservations,collateral_active_agreements,collateral_workflows,collateral_snapshots,collateral_lots,custody_actions,custody_statements,custody_runs,custody_breaks,ledger_entries,ledger_snapshots,outbox CASCADE`); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	sort.Strings(files)
	for _, file := range files {
		b, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = p.Exec(t.Context(), string(b)); e != nil {
			t.Fatalf("%s: %v", file, e)
		}
	}
	return p
}
func command() cashmove.Command {
	return cashmove.Command{Tenant: "tenant-A", Actor: "maker", Reason: "transfer-agent evidence reviewed", Movement: cashmove.CashMovement{MovementID: "M1", PortfolioID: "PF", Kind: cashmove.Subscription, Amount: big.NewRat(1, 1000000000), Currency: "USDT", Effective: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), SourceRef: "transfer-123", VenueAccountID: "account"}}
}
func digest(t *testing.T, c cashmove.Command) string {
	t.Helper()
	d, e := cashmove.ReviewDigest(c)
	if e != nil {
		t.Fatal(e)
	}
	return d
}

func TestReviewDigestCanonicalAndExact(t *testing.T) {
	c := command()
	d := digest(t, c)
	c.Movement.Amount = big.NewRat(10, 10000000000)
	if digest(t, c) != d {
		t.Fatal("equivalent decimals differ")
	}
	c.Movement.Effective = c.Movement.Effective.In(time.FixedZone("offset", 3600))
	if digest(t, c) != d {
		t.Fatal("equivalent instants differ")
	}
	for _, mutate := range []func(*cashmove.Command){func(c *cashmove.Command) { c.Actor = "other" }, func(c *cashmove.Command) { c.Tenant = "other" }, func(c *cashmove.Command) { c.Reason = "changed" }, func(c *cashmove.Command) { c.Movement.SourceRef = "other" }, func(c *cashmove.Command) { c.Movement.VenueAccountID = "other" }, func(c *cashmove.Command) { c.Movement.PortfolioID = "other" }, func(c *cashmove.Command) { c.Movement.Kind = cashmove.Redemption }, func(c *cashmove.Command) { c.Movement.Currency = "USD" }} {
		other := command()
		mutate(&other)
		if digest(t, other) == d {
			t.Fatal("review omitted command term")
		}
	}
	for _, amount := range []string{"1/3", "9223372036854775809.1", "0", "-1"} {
		c = command()
		c.Movement.Amount = dec.Rat(amount)
		if _, err := cashmove.ReviewDigest(c); err == nil {
			t.Fatalf("accepted %s", amount)
		}
	}
	c = command()
	c.Movement.Effective = c.Movement.Effective.Add(time.Nanosecond)
	if _, err := cashmove.ReviewDigest(c); err == nil {
		t.Fatal("sub-microsecond time silently truncated")
	}
}

func TestPostgresCashCommandsConcurrentReplayIsolationAndRollback(t *testing.T) {
	p := database(t)
	s := cashmove.NewCommands(p)
	c := command()
	d := digest(t, c)
	const n = 12
	results := make(chan cashmove.Receipt, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := s.Accept(t.Context(), c, d); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first cashmove.Receipt
	for r := range results {
		if first.Digest == "" {
			first = r
		}
		if r != first {
			t.Fatal("retry receipt differs")
		}
	}
	var count int
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("outbox count %d %v", count, err)
	}
	for _, mutate := range []func(*cashmove.Command){func(c *cashmove.Command) { c.Actor = "other" }, func(c *cashmove.Command) { c.Movement.Amount = big.NewRat(2, 1) }, func(c *cashmove.Command) { c.Movement.PortfolioID = "other" }, func(c *cashmove.Command) { c.Reason = "changed" }} {
		changed := command()
		mutate(&changed)
		if _, err := s.Accept(t.Context(), changed, digest(t, changed)); !errors.Is(err, cashmove.ErrCommandConflict) {
			t.Fatalf("changed retry: %v", err)
		}
	}
	// No process-local idempotency cache: reconnect and receive the same receipt.
	restarted := cashmove.NewCommands(poolFor(t, "tenant-A"))
	if r, e := restarted.Accept(t.Context(), c, d); e != nil || r != first {
		t.Fatalf("restart: %+v %v", r, e)
	}
	other := cashmove.NewCommands(poolFor(t, "tenant-B"))
	if _, e := other.Status(t.Context(), "tenant-B", "PF", "M1"); !errors.Is(e, cashmove.ErrCommandNotFound) {
		t.Fatalf("RLS read: %v", e)
	}
	if _, e := other.Accept(t.Context(), c, d); !errors.Is(e, cashmove.ErrCommandNotFound) {
		t.Fatalf("RLS write: %v", e)
	}
	c.Tenant = "tenant-B"
	if _, e := other.Accept(t.Context(), c, digest(t, c)); e != nil {
		t.Fatal(e)
	}
	if _, e := p.Exec(t.Context(), `UPDATE cash_commands SET actor='tampered'`); e == nil {
		t.Fatal("mutable command evidence")
	}
	if _, e := p.Exec(t.Context(), `CREATE OR REPLACE FUNCTION fail_cash_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected outbox failure'; END $$; CREATE TRIGGER fail_cash_outbox BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION fail_cash_outbox()`); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DROP TRIGGER IF EXISTS fail_cash_outbox ON outbox; DROP FUNCTION IF EXISTS fail_cash_outbox()`)
	})
	c = command()
	c.Movement.MovementID = "rollback"
	if _, e := s.Accept(t.Context(), c, digest(t, c)); e == nil {
		t.Fatal("accepted without outbox")
	}
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM cash_commands WHERE movement_id='rollback'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial transaction: %d %v", count, err)
	}
}

func TestPostgresCashCommandBindsJournalAndConflictingRace(t *testing.T) {
	p := database(t)
	s := cashmove.NewCommands(p)
	c := command()
	r, err := s.Accept(t.Context(), c, digest(t, c))
	if err != nil {
		t.Fatal(err)
	}
	st := ledger.NewPostgres(p)
	e := &ledger.Event{EntryID: "cash:M1", PortfolioID: "PF", VenueAccountID: "account", Type: ledger.EntryCash, Cash: big.NewRat(1, 1), CashCurrency: "USDT", Effective: c.Movement.Effective, Knowledge: r.AcceptedAt, SourceRef: c.Movement.SourceRef}
	if err = st.Append(t.Context(), e, nil); !errors.Is(err, ledger.ErrCashConflict) {
		t.Fatalf("forged fold: %v", err)
	}
	e.Cash = new(big.Rat).Set(c.Movement.Amount)
	if err = st.Append(t.Context(), e, nil); err != nil {
		t.Fatal(err)
	}
	if err = st.Append(t.Context(), e, nil); err != nil {
		t.Fatal(err)
	}
	e.Cash = big.NewRat(9, 1)
	if err = st.Append(t.Context(), e, nil); !errors.Is(err, ledger.ErrCashConflict) {
		t.Fatalf("changed duplicate: %v", err)
	}
	if status, e := s.Status(t.Context(), "tenant-A", "PF", "M1"); e != nil || status.Status != "journal_recorded" {
		t.Fatalf("fold state: %+v %v", status, e)
	}
	if retry, e := s.Accept(t.Context(), c, digest(t, c)); e != nil || retry != r {
		t.Fatalf("POST changed after fold: %+v %v", retry, e)
	}
	// A legacy producer and a new command compete for one identity. Exactly one
	// economic meaning can win, regardless of which transaction gets the lock.
	c.Movement.MovementID = "race"
	d := digest(t, c)
	e.EntryID = "cash:race"
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; _, err := s.Accept(t.Context(), c, d); results <- err }()
	go func() { <-start; results <- st.Append(t.Context(), e, nil) }()
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("expected exactly one winner: %v / %v", a, b)
	}
}

func TestPostgresJetStreamAcceptedCashSurvivesPublisherRestart(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires real JetStream")
	}
	p := database(t)
	c := command()
	name := fmt.Sprintf("CASH_COMMAND_%d", time.Now().UnixNano())
	c.Movement.MovementID = name
	s := cashmove.NewCommands(p)
	if _, e := s.Accept(t.Context(), c, digest(t, c)); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	nc, e := nats.Connect(url)
	if e != nil {
		t.Fatal(e)
	}
	defer nc.Close()
	js, e := jetstream.New(nc)
	if e != nil {
		t.Fatal(e)
	}
	subject := "accounting.cash.subscription"
	// Reuse a provisioned production stream or bind a scratch file-backed one.
	if _, e = js.StreamNameBySubject(ctx, subject); e != nil {
		if _, e = js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: []string{"accounting.cash.>"}, Storage: jetstream.FileStorage}); e != nil {
			t.Fatal(e)
		}
		defer func() { _ = js.DeleteStream(context.Background(), name) }()
	}
	client, e := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: name})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = client.Close() }()
	producer, e := bus.NewProducer(client, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: "tenant-A"})
	if e != nil {
		t.Fatal(e)
	}
	consumer, e := bus.NewConsumer(client)
	if e != nil {
		t.Fatal(e)
	}
	st := ledger.NewPostgres(p)
	folder, e := consume.NewFolder("tenant-A", st, "USD")
	if e != nil {
		t.Fatal(e)
	}
	folded := make(chan error, 8)
	done := make(chan error, 1)
	go func() {
		done <- consumer.Subscribe(ctx, subject, name, func(ctx context.Context, env *envelopepb.Envelope, b []byte) error {
			var entry pb.LedgerEntry
			if err := proto.Unmarshal(b, &entry); err != nil {
				return err
			}
			if entry.EntryId != "cash:"+c.Movement.MovementID || env.TenantId != "tenant-A" {
				return nil
			}
			err := folder.HandleCash(ctx, env, b)
			select {
			case folded <- err:
			default:
			}
			return err
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("consumer failed to stop")
		}
	}()
	// The command was committed before any publisher existed. A fresh relay
	// recovers it from PostgreSQL; the original HTTP process is unnecessary.
	relay, e := outbox.NewRelay(st.Outbox(), producer, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = relay.Flush(ctx, "PF"); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-folded:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("cash FACT did not fold")
	}
	entries, e := st.Journal(ctx, "PF")
	if e != nil || len(entries) != 1 || entries[0].Cash.Cmp(c.Movement.Amount) != 0 {
		t.Fatalf("exact journal: %v %v", entries, e)
	}
	if status, e := s.Status(ctx, "tenant-A", "PF", c.Movement.MovementID); e != nil || status.Status != "journal_recorded" {
		t.Fatalf("status: %+v %v", status, e)
	}
	if entries[0].SettlementBasis != ledger.SettlementUnknown {
		t.Fatal("acceptance invented settlement")
	}
}
