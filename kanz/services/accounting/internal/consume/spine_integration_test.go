package consume_test

// A CASH MOVEMENT MUST SURVIVE THE WHOLE WAY (#245).
//
// The cashmove producer and the Folder were each proven in isolation, against
// each other's shapes rather than against the spine between them: the producer's
// tests called the unexported encode(), and the Folder's called Handle directly
// with context.Background() and an envelope literal. Neither could see the two
// things that actually drop an investor's subscription —
//
//   - the subject is not carried by a stream, so the JetStream publish is a HARD
//     ERROR (infra/nats/bootstrap-job.yaml:116-118 records accounting.* being
//     exactly that: "Every one was a hard publish failure: the ledger's inputs,
//     dropped"), and
//   - the fold writes through an RLS pool, so a row the session's app.tenant_id
//     does not cover is not a permissions error the caller sees — it is zero
//     rows, silently.
//
// So this drives the real path: real NATS, real bus.Producer, real bus.Consumer,
// real Postgres behind internal/pg.NewTenantPool, and a REAL tenant rather than
// __system__ — the shared-bucket tenant makes RequireTenantScope's discriminating
// branch unreachable and would prove nothing about isolation.

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/consume"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

const cashSubjectWildcard = "accounting.cash.>"

// tenantPool opens the service's real RLS pool for tenant and (re)applies the
// accounting migrations, mirroring ledger/postgres_test.go's newPool. The pool is
// the production one — a test that scoped the session by hand would not be
// exercising the AfterConnect the service depends on.
func tenantPool(t *testing.T, ctx context.Context, dsn, tenant string) *pgxpool.Pool {
	t.Helper()
	pool, err := pg.NewTenantPool(ctx, dsn, tenant)
	if err != nil {
		t.Fatalf("NewTenantPool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS ledger_entries, ledger_snapshots CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("../../migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
	return pool
}

// foldCounter wraps the Folder's handler so the test can wait on a real ack
// rather than on a sleep, and so a fold that ERRORED is distinguishable from one
// that has not arrived yet — a nack loops forever and would otherwise read as a
// timeout.
//
// IT ALSO FILTERS BY TENANT, and that is not a convenience. ACCOUNTING is a
// 168h stream: a previous run's cash FACTs are still on it, and a fresh durable
// consumer replays from the beginning, so an unfiltered handler folds another
// run's subscription and the Folder correctly nacks it forever. That is the
// production topology behaving exactly as designed — one stream, many tenants,
// each service consuming only its own — so the harness models the tenant
// scoping a real deployment gets from its subject/stream binding. The refusal
// itself is proven in TestFolderRefusesAnotherTenantsCashFact rather than being
// swallowed here.
type foldCounter struct {
	tenant string
	mu     sync.Mutex
	ok     int
	errs   []error
}

func (f *foldCounter) wrap(h bus.EventHandler) bus.EventHandler {
	return func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		if env.GetTenantId() != f.tenant {
			return nil // another run's residue on a shared, long-retention stream
		}
		err := h(ctx, env, payload)
		f.mu.Lock()
		defer f.mu.Unlock()
		if err != nil {
			f.errs = append(f.errs, err)
		} else {
			f.ok++
		}
		return err
	}
}

func (f *foldCounter) counts() (int, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ok, append([]error(nil), f.errs...)
}

func TestACashMovementReachesTheLedgerOverTheRealSpine(t *testing.T) {
	natsURL := os.Getenv("TEST_NATS_URL")
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if natsURL == "" || dsn == "" {
		t.Skip("set TEST_NATS_URL and TEST_POSTGRES_URL to drive the cash-movement path over the real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	// A REAL tenant. __system__ short-circuits RequireTenantScope's shared-bucket
	// branch and would leave the isolation half of this file vacuous.
	tenant := "acct-it-" + suffix
	portfolio := "pf-" + suffix

	pool := tenantPool(t, ctx, dsn, tenant)
	store := ledger.NewPostgres(pool)

	admin, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	// The assertion no unit test can make: accounting.cash.> is BOUND. On a
	// provisioned topology this binds the real ACCOUNTING stream; on a bare broker
	// it creates a scratch one, and a half-bound topology fails loudly rather than
	// being papered over.
	bustest.EnsureSubjects(t, ctx, js, "ACCOUNTING_IT_"+suffix, []string{cashSubjectWildcard})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: natsURL, Name: "accounting-cash-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// The producer is configured exactly as the composition root configures it: a
	// fallback tenant, because a cash movement is raised by an HTTP request and has
	// no inbound delivery to inherit one from.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "accounting", ProducerVersion: "it", Tenant: tenant,
	})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := cashmove.NewPublisher(producer, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := consume.NewFolder(tenant, store, "USD")
	if err != nil {
		t.Fatal(err)
	}

	counter := &foldCounter{tenant: tenant}
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.Subscribe(subCtx, cashSubjectWildcard,
			"accounting-cash-it-"+suffix, counter.wrap(folder.HandleCash))
	}()

	// A subscription in, a fee out — two kinds, two subjects, one wildcard binding.
	movements := []cashmove.CashMovement{
		{
			MovementID: "sub-" + suffix, PortfolioID: portfolio, Kind: cashmove.Subscription,
			Amount: dec.Rat("100000"), Currency: "USD",
			Effective: time.Now().UTC(), SourceRef: "transfer-agent",
		},
		{
			MovementID: "fee-" + suffix, PortfolioID: portfolio, Kind: cashmove.Fee,
			Amount: dec.Rat("250"), Currency: "USD",
			Effective: time.Now().UTC(), SourceRef: "fee-run",
		},
	}
	for _, m := range movements {
		// A publish error HERE is the bootstrap-job failure mode: the subject has no
		// stream and the ledger's input never left the process.
		if err := pub.Publish(ctx, m); err != nil {
			t.Fatalf("publishing a %v to a real broker failed: %v\n"+
				"On the live spine this is a dropped ledger input and the fund's cash is wrong.", m.Kind, err)
		}
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ok, errs := counter.counts()
		if len(errs) > 0 {
			t.Fatalf("the Folder NACKED a cash FACT its own service published: %v", errs[0])
		}
		if ok >= len(movements) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ok, _ := counter.counts(); ok < len(movements) {
		t.Fatalf("folded %d of %d cash movements within 30s", ok, len(movements))
	}

	book, _, err := ledger.MaterializeCurrent(ctx, store, portfolio)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// 100000 subscription − 250 fee, read back through the RLS-scoped pool.
	if got := book.CashBalance("USD"); got.Cmp(big.NewRat(99750, 1)) != 0 {
		t.Fatalf("book cash = %s, want 99750 — the FACT round-tripped but the ledger does not agree",
			got.RatString())
	}

	// The rows carry the tenant the pool is scoped to. This is the half a
	// handler-level test cannot reach: the column defaults from
	// current_setting('app.tenant_id'), so an unscoped session writes rows nobody
	// can read back rather than failing.
	var rows int
	var rowTenant string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), coalesce(min(tenant_id), '') FROM ledger_entries WHERE portfolio_id = $1`,
		portfolio).Scan(&rows, &rowTenant); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if rows != len(movements) {
		t.Fatalf("ledger_entries holds %d rows for %s, want %d", rows, portfolio, len(movements))
	}
	if rowTenant != tenant {
		t.Fatalf("rows carry tenant_id %q, want %q — RLS scoped the write to the wrong tenant", rowTenant, tenant)
	}
}

// THE CROSS-TENANT REFUSAL THAT consume_test.go SAYS IS PROVEN ELSEWHERE (#223).
//
// consume_test.go:19-22 states "the cross-tenant refusal itself is proven in
// cross_tenant_test.go, which uses a real tenant". That file does not exist —
// the claim was true of an intention, not of the tree, and every fold test in
// the package runs under __system__, whose shared-bucket branch returns nil
// before any comparison happens. So the guard the comment points at had no test
// at all. This is it, and it uses a real tenant so the discriminating branch is
// the one that runs.
func TestFolderRefusesAnotherTenantsCashFact(t *testing.T) {
	store := ledger.NewMemoryStore()
	folder, err := consume.NewFolder("acme", store, "USD")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A valid, well-formed cash FACT — from the WRONG tenant.
	other := envelopeFor("accounting.cash.subscription", "rival-fund")
	payload := cashEntryBytes(t, "cash:MV-X", "PORT-1", 100000)
	if err := folder.HandleCash(ctx, other, payload); err == nil {
		t.Fatal("a rival tenant's cash FACT was ACKED into this fund's book — the fold writes " +
			"through an RLS pool pinned to acme, so it would have posted another tenant's " +
			"subscription as this one's")
	}
	if j, _ := store.Journal(ctx, "PORT-1"); len(j) != 0 {
		t.Fatalf("the refused FACT still wrote %d journal entries, want 0", len(j))
	}

	// NON-VACUITY: the SAME payload from the SAME service under the right tenant
	// folds. Without this, a Folder that refused everything would pass above.
	mine := envelopeFor("accounting.cash.subscription", "acme")
	if err := folder.HandleCash(ctx, mine, payload); err != nil {
		t.Fatalf("the tenant's own cash FACT was refused: %v", err)
	}
	if j, _ := store.Journal(ctx, "PORT-1"); len(j) != 1 {
		t.Fatalf("own-tenant fold wrote %d journal entries, want 1", len(j))
	}
}

// A FILL FROM ANOTHER TENANT IS REFUSED TOO — the fill path is a different
// handler and would have to forget the check independently.
func TestFolderRefusesAnotherTenantsFill(t *testing.T) {
	store := ledger.NewMemoryStore()
	folder, err := consume.NewFolder("acme", store, "USD")
	if err != nil {
		t.Fatal(err)
	}
	env := envelopeFor("order.order.filled", "rival-fund")
	if err := folder.Handle(context.Background(), env, fillBytes(t, "PORT-1", "F1")); err == nil {
		t.Fatal("a rival tenant's fill was folded into this fund's book of record")
	}
}

// --- helpers -----------------------------------------------------------------

func envelopeFor(eventType, tenant string) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventType:     eventType,
		TenantId:      tenant,
		IngestionTime: timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
	}
}

func cashEntryBytes(t *testing.T, entryID, portfolio string, amount int64) []byte {
	t.Helper()
	b, err := proto.Marshal(&accountingpb.LedgerEntry{
		EntryId:       entryID,
		PortfolioId:   portfolio,
		EntryType:     accountingpb.EntryType_ENTRY_TYPE_CASH,
		Cash:          dec.ToProto(big.NewRat(amount, 1)),
		CashCurrency:  "USD",
		EffectiveTime: timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
		KnowledgeTime: timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func fillBytes(t *testing.T, portfolio, fillID string) []byte {
	t.Helper()
	b, err := proto.Marshal(&orderpb.OrderFilled{
		State: &orderpb.OrderState{PortfolioId: portfolio},
		Fill: &orderpb.Fill{
			FillId:       fillID,
			InstrumentId: "AAPL",
			Side:         orderpb.Side_SIDE_BUY,
			Quantity:     dec.ToProto(big.NewRat(100, 1)),
			Price:        dec.ToProto(big.NewRat(150, 1)),
			ExecutedAt:   timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
