package orderview

// THE DURABLE ORDER VIEW, AGAINST A REAL POSTGRES (#905).
//
// Postgres is the half both deployed adapters actually run on —
// infra/deploy/venue-binance-deploy.yaml and venue-okx-deploy.yaml each set
// VENUE_*_DATABASE_URL_FILE — and until this file it had no test of any kind.
// Memory got behavioural tests in #891 and eleven more for Progress in #904;
// the store that survives a restart and holds real orders had none, which is
// the wrong way round.
//
// FOUR OF THE PROPERTIES HERE ARE NOT PROPERTIES OF THE GO CODE AT ALL. They
// are properties of the SQL and of the engine, and a double reports success for
// every one of them:
//
//   - Record's ON CONFLICT ... DO UPDATE. A redelivered ExecuteRequest must
//     REFRESH the row. DO NOTHING would also "succeed", leaving the adapter's
//     view pinned at the first status it ever saw.
//   - Open's `status NOT IN ($1,$2,$3,$4)`. That is a hand-written second copy
//     of the set Terminal() defines, twelve lines above it in the same file, and
//     #806/#803 are this repository's evidence for what a copied set costs. The
//     test below does not write a THIRD copy: it walks the order.v1 status enum
//     descriptor and asks Terminal() for the answer, so a status added to
//     Terminal and not to the SQL fails here rather than in production.
//   - The tenant_isolation policy. It keys on the app.tenant_id GUC and is
//     FORCE ROW LEVEL SECURITY, and Postgres.Get's `WHERE order_id = $1` carries
//     no tenant predicate at all — it relies on the policy entirely. Nothing
//     exercised a cross-tenant read.
//   - decode over a row written by a different schema version.
//
// THE ROLE IS ASSERTED, NOT ASSUMED (see newPGFixture). A SUPERUSER or
// BYPASSRLS role bypasses row-level security unconditionally, so the isolation
// tests below would pass because the policy was never consulted — indis-
// tinguishable, in the output, from passing because it works. That check is a
// FATAL, not a skip: a run that cannot establish it must not report green.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// The migrations under test. venue-binance's are applied; the OKX twin is held
// to be the same DDL by TestBothDeployedAdaptersRunTheSameOrderViewSchema, which
// is what makes everything below a statement about both deployed adapters rather
// than about one of them.
const (
	binanceMigrations = "../../../services/venue-binance/migrations"
	okxMigrations     = "../../../services/venue-okx/migrations"
)

// pgFixture is one throwaway schema with the venue adapter's migrations applied.
// Every pool it hands out is scoped to that schema, so a run cannot collide with
// a developer's real venue_orders — or with another gated package's.
type pgFixture struct {
	url    string
	schema string
}

func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL (kanz/test/backing/up.sh) to run the durable order-view tests")
	}
	ctx := context.Background()

	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (bootstrap): %v", err)
	}
	defer boot.Close()

	// CAN THIS CONNECTION SEE PAST A POLICY? pg_has_role rather than a lookup on
	// current_user alone, because BYPASSRLS is INHERITED through role membership:
	// a role that is not itself rolbypassrls but is a member of one that is
	// bypasses RLS just the same. Same question internal/pg.NewTenantPool asks
	// every service connection (#634), asked here for the same reason — if the
	// answer is yes, the two isolation tests below assert nothing and say PASS.
	var exempt bool
	if err := boot.QueryRow(ctx,
		`SELECT COALESCE(bool_or(r.rolsuper OR r.rolbypassrls), false)
		   FROM pg_roles r
		  WHERE pg_has_role(current_user, r.oid, 'USAGE')`).Scan(&exempt); err != nil {
		t.Fatalf("could not establish whether TEST_POSTGRES_URL's role is exempt from row-level "+
			"security, so the isolation assertions below cannot be trusted: %v", err)
	}
	if exempt {
		t.Fatal("TEST_POSTGRES_URL points at a SUPERUSER or BYPASSRLS role (directly or through role " +
			"membership). Postgres exempts both from row-level security unconditionally — FORCE ROW " +
			"LEVEL SECURITY does not reach them — so venue_orders' tenant_isolation policy would be a " +
			"no-op and this file's cross-tenant tests would pass without the policy existing at all. " +
			"Point TEST_POSTGRES_URL at the NOSUPERUSER application role")
	}

	schema := fmt.Sprintf("venue_orderview_test_%d", time.Now().UnixNano())
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		drop, derr := pgxpool.New(context.Background(), url)
		if derr != nil {
			t.Errorf("reconnect to drop schema %s: %v — it is now residue", schema, derr)
			return
		}
		defer drop.Close()
		if _, derr := drop.Exec(context.Background(),
			`DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); derr != nil {
			t.Errorf("drop schema %s: %v — it is now residue", schema, derr)
		}
	})

	f := &pgFixture{url: url, schema: schema}
	f.applyMigrations(t)
	return f
}

// applyMigrations applies EVERY migration the service ships, in order, not a
// named one. 0002 replaces 0001's policy with the app_current_tenant() form that
// RAISES on an unscoped session (MT-01e); a fixture that stopped at 0001 would
// build a schema the deployment does not have, and the unscoped-session test
// below would be asserting against DDL nobody runs.
func (f *pgFixture) applyMigrations(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool := f.rawPool(t, "")

	files, err := filepath.Glob(filepath.Join(binanceMigrations, "*.sql"))
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no migrations under %s — the fixture would build an empty schema and every test "+
			"below would fail for the wrong reason", binanceMigrations)
	}
	sort.Strings(files)
	for _, file := range files {
		ddl, rerr := os.ReadFile(file)
		if rerr != nil {
			t.Fatalf("read migration %s: %v", file, rerr)
		}
		if _, eerr := pool.Exec(ctx, string(ddl)); eerr != nil {
			t.Fatalf("apply migration %s: %v", file, eerr)
		}
	}
}

// rawPool opens a pool on the fixture's schema. tenant == "" leaves app.tenant_id
// unset — the unscoped-session case the engine must REFUSE rather than answer
// with zero rows.
func (f *pgFixture) rawPool(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = f.schema
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if tenant == "" {
			return nil
		}
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect as tenant %q: %v", tenant, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// storeAs returns the durable store as one tenant, and the pool under it so a
// test can go round the store with raw SQL — which is how the status column is
// compared against the state blob.
func (f *pgFixture) storeAs(t *testing.T, tenant string) (*Postgres, *pgxpool.Pool) {
	t.Helper()
	pool := f.rawPool(t, tenant)
	return NewPostgres(pool), pool
}

// routed is a fully-populated order — every field an OMS hands the adapter,
// including the twenty Progress must not lose. working() next door sets three.
func routed(id string) *orderpb.OrderState {
	at := timestamppb.New(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	return &orderpb.OrderState{
		OrderId:         id,
		PortfolioId:     "pf-alpha",
		InstrumentId:    "BTC-USD",
		Side:            orderpb.Side_SIDE_BUY,
		OrderType:       orderpb.OrderType_ORDER_TYPE_LIMIT,
		TimeInForce:     orderpb.TimeInForce_TIME_IN_FORCE_GTD,
		OrderedQuantity: dec(10, 0),
		LimitPrice:      dec(6500000, -2),
		StopPrice:       dec(6400000, -2),
		ExpireAt:        at,
		ArrivalPrice:    dec(6490000, -2),
		ArrivalAt:       at,
		ExecutionSchedule: &orderpb.ExecutionSchedule{
			Algo:                 orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP,
			WindowStart:          at,
			WindowEnd:            at,
			SliceCount:           5,
			MaxSliceQuantity:     dec(2, 0),
			MaxParticipationRate: dec(15, -2),
		},
		ParentOrderId:  "parent-1",
		Status:         orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		Venue:          "binance",
		VenueAccountId: "binance-alpha",
		VenueAckAt:     at,
		Quarantine: &orderpb.OrderQuarantine{
			At:          at,
			Reason:      "venue query timed out",
			LastQueryAt: at,
		},
		CancelAnnouncedAt:   at,
		OutcomeAnnouncedAt:  at,
		AcceptedAnnouncedAt: at,
		Leverage:            dec(3, 0),
		MarginMode:          orderpb.MarginMode_MARGIN_MODE_ISOLATED,
		ReleasePrice:        dec(6495000, -2),
		ReleaseAt:           at,
		ReleaseBid:          dec(6494000, -2),
		ReleaseAsk:          dec(6496000, -2),
	}
}

// dec is exact base-10, never a float — money and quantities on a capital path.
func dec(coefficient int64, exponent int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coefficient, Exponent: exponent}
}

// differingFields names every field on which two OrderStates disagree, derived
// from the message descriptor rather than listed. A hand-written list of what
// Progress must preserve would be a fourth copy of the same set, and the failure
// mode it is here to catch is precisely a field nobody remembered to list.
func differingFields(a, b *orderpb.OrderState) []string {
	ra, rb := a.ProtoReflect(), b.ProtoReflect()
	fields := ra.Descriptor().Fields()
	var out []string
	for i := range fields.Len() {
		fd := fields.Get(i)
		if !ra.Get(fd).Equal(rb.Get(fd)) {
			out = append(out, string(fd.Name()))
		}
	}
	return out
}

// TestRecordRefreshesARedeliveredOrderRatherThanFailing.
//
// Record is NOT an admission gate — admission happened in the OMS, before this
// adapter was called — so a retry or a redelivered ExecuteRequest for an order
// already in the view must refresh it. Two ways for the upsert to be wrong and
// both are silent: a bare INSERT raises a unique violation the caller reports as
// a failed placement, and ON CONFLICT DO NOTHING succeeds while pinning the view
// at the first status it ever saw — an order the venue has since moved on.
func TestRecordRefreshesARedeliveredOrderRatherThanFailing(t *testing.T) {
	f := newPGFixture(t)
	st, pool := f.storeAs(t, "acme")
	ctx := context.Background()

	first := routed("o-redelivered")
	if err := st.Record(ctx, first); err != nil {
		t.Fatalf("Record: %v", err)
	}

	second := proto.Clone(first).(*orderpb.OrderState)
	second.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	second.FilledQuantity = dec(4, 0)
	second.LeavesQuantity = dec(6, 0)
	if err := st.Record(ctx, second); err != nil {
		t.Fatalf("re-recording an order already in the view was REJECTED (%v). A redelivery or a "+
			"retried ExecuteRequest would surface to the OMS as a failed placement for an order "+
			"this adapter is already working", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM venue_orders WHERE order_id = $1`,
		"o-redelivered").Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("a redelivered order left %d rows in venue_orders, want 1", rows)
	}

	got, ok, err := st.Get(ctx, "o-redelivered")
	if err != nil || !ok {
		t.Fatalf("Get after the redelivery: ok=%v err=%v", ok, err)
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Errorf("the view still holds %v after a re-record at PARTIALLY_FILLED — the upsert "+
			"accepted the write and discarded it, so this adapter's view is pinned at the first "+
			"status it ever saw", got.GetStatus())
	}
	if !proto.Equal(got.GetFilledQuantity(), dec(4, 0)) {
		t.Errorf("filled_quantity = %v after the re-record, want 4 — the state blob was not "+
			"refreshed", got.GetFilledQuantity())
	}

	// created_at PRESERVED, updated_at MOVED: the row was updated in place rather
	// than replaced, and DO UPDATE's `updated_at = now()` actually fired. Each
	// statement is its own transaction, so now() differs between them.
	var created, updated time.Time
	if err := pool.QueryRow(ctx,
		`SELECT created_at, updated_at FROM venue_orders WHERE order_id = $1`,
		"o-redelivered").Scan(&created, &updated); err != nil {
		t.Fatalf("timestamps: %v", err)
	}
	if !updated.After(created) {
		t.Errorf("updated_at (%s) did not move past created_at (%s) — the conflicting write did "+
			"not take the DO UPDATE branch", updated, created)
	}

	// AND THE STATUS COLUMN MOVED WITH THE BLOB. Open filters on the column while
	// Get decodes the blob, so a refresh that touched one and not the other is a
	// view that answers two different questions two different ways.
	assertColumnMatchesBlob(t, pool, "o-redelivered")
}

// TestOpenReturnsExactlyTheStatusesTerminalDoesNotCallTerminal.
//
// Open's `status NOT IN ($1,$2,$3,$4)` is a hand-written second enumeration of
// the set Terminal() owns, in the same file. The expectation here is DERIVED —
// every value of the order.v1 status enum, classified by calling Terminal — so a
// status added to Terminal and not to the SQL fails here, and a status added to
// the SQL and not to Terminal fails here too. Both directions cost something:
// a terminal order left in Open is #904's leak (the reconciler re-queries a
// finished order forever and re-emits StateHealed every pass); a working order
// dropped from Open is the healing watchdog going blind to a live order at the
// exchange, which is worse.
func TestOpenReturnsExactlyTheStatusesTerminalDoesNotCallTerminal(t *testing.T) {
	f := newPGFixture(t)
	st, _ := f.storeAs(t, "acme")
	ctx := context.Background()

	values := orderpb.OrderStatus(0).Descriptor().Values()
	if values.Len() < 5 {
		t.Fatalf("the order.v1 status enum has %d values — this test is enumerating almost nothing",
			values.Len())
	}

	want := make(map[string]orderpb.OrderStatus)
	seededTerminal := 0
	for i := range values.Len() {
		s := orderpb.OrderStatus(values.Get(i).Number())
		id := fmt.Sprintf("o-status-%d", values.Get(i).Number())
		o := routed(id)
		o.Status = s
		if err := st.Record(ctx, o); err != nil {
			t.Fatalf("seed %s at %v: %v", id, s, err)
		}
		if Terminal(s) {
			seededTerminal++
		} else {
			want[id] = s
		}
	}
	if seededTerminal == 0 || len(want) == 0 {
		t.Fatalf("Terminal() classified %d of %d statuses as terminal — with all of them on one "+
			"side this test asserts nothing", seededTerminal, values.Len())
	}

	open, err := st.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := make(map[string]orderpb.OrderStatus, len(open))
	for _, o := range open {
		got[o.GetOrderId()] = o.GetStatus()
	}
	for id, s := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("Open omitted %s at %v, which orderview.Terminal does NOT call terminal. The "+
				"healing watchdog reconciles against exactly this set, so it is now blind to an "+
				"order still working at the exchange", id, s)
		}
	}
	for id, s := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("Open returned %s at %v, which orderview.Terminal DOES call terminal. The "+
				"reconciler will spend REST weight re-querying a finished order on every pass and "+
				"re-emit StateHealed for it forever (#904)", id, s)
		}
	}
}

// TestProgressToFilledLeavesOpenWhileGetStillAnswers — #904's write, on the
// durable store, where it lands on two columns instead of one map entry.
//
// Open filters in SQL on `status`; Get decodes `state`. Record writes both, from
// the same message, in one statement — and that is the thing being checked,
// because if it ever stopped doing so the two reads would disagree and nothing
// in Memory could reproduce it: there is one blob there and no column at all.
func TestProgressToFilledLeavesOpenWhileGetStillAnswers(t *testing.T) {
	f := newPGFixture(t)
	st, pool := f.storeAs(t, "acme")
	ctx := context.Background()

	if err := st.Record(ctx, routed("o-fill")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if open, oerr := st.Open(ctx); oerr != nil || len(open) != 1 {
		t.Fatalf("Open before the fill = %d orders (err %v), want 1", len(open), oerr)
	}

	if err := Progress(ctx, st, venueReport("o-fill", orderpb.OrderStatus_ORDER_STATUS_FILLED, 10, 0)); err != nil {
		t.Fatalf("Progress: %v", err)
	}

	got, ok, err := st.Get(ctx, "o-fill")
	if err != nil || !ok {
		t.Fatalf("Get after the fill: ok=%v err=%v — the user-data ingester can no longer enrich a "+
			"report for this order, and Execute's already-worked refusal fails OPEN", ok, err)
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Errorf("Get says %v after a venue-reported fill, want FILLED — the adapter's view "+
			"disagrees with the FACT it published", got.GetStatus())
	}
	open, err := st.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("Open still returns %d order(s) after the fill — #904's leak, on the store both "+
			"deployed adapters run on", len(open))
	}
	assertColumnMatchesBlob(t, pool, "o-fill")
}

// assertColumnMatchesBlob reads the row behind the store's back and checks that
// the denormalized status column says what the authoritative state blob says.
func assertColumnMatchesBlob(t *testing.T, pool *pgxpool.Pool, orderID string) {
	t.Helper()
	var (
		column int32
		blob   []byte
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT status, state FROM venue_orders WHERE order_id = $1`, orderID).Scan(&column, &blob); err != nil {
		t.Fatalf("read the raw row for %s: %v", orderID, err)
	}
	st, err := decode(blob, orderID)
	if err != nil {
		t.Fatalf("decode the stored blob for %s: %v", orderID, err)
	}
	if orderpb.OrderStatus(column) != st.GetStatus() {
		t.Errorf("venue_orders.status is %v but the state blob decodes to %v for %s. Open reads the "+
			"COLUMN and Get reads the BLOB, so the reconciler and the fill enricher now hold "+
			"different beliefs about the same order",
			orderpb.OrderStatus(column), st.GetStatus(), orderID)
	}
}

// TestProgressPreservesEveryTermTheVenueDidNotReport, through the proto round
// trip Memory does not have.
//
// Progress merges rather than replaces because both ingesters build their healed
// OrderState out of a venue report plus a handful of Kanz terms — a fraction of
// the message. On Memory the merge is a proto.Clone and stays in memory; here it
// goes out through proto.Marshal into BYTEA and back through decode, and the
// assertion is over the WHOLE message by descriptor, not over a list somebody
// would have to remember to extend.
func TestProgressPreservesEveryTermTheVenueDidNotReport(t *testing.T) {
	f := newPGFixture(t)
	st, _ := f.storeAs(t, "acme")
	ctx := context.Background()

	stored := routed("o-merge")
	if err := st.Record(ctx, stored); err != nil {
		t.Fatalf("Record: %v", err)
	}

	report := venueReport("o-merge", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 4, 6)
	report.AverageFillPrice = dec(6501234, -2)
	report.AsOf = timestamppb.New(time.Date(2026, 9, 1, 13, 30, 0, 0, time.UTC))
	if err := Progress(ctx, st, report); err != nil {
		t.Fatalf("Progress: %v", err)
	}

	got, ok, err := st.Get(ctx, "o-merge")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}

	// The order the store SHOULD now hold: everything the OMS gave us, with only
	// what the exchange observed taken from the report.
	want := proto.Clone(stored).(*orderpb.OrderState)
	want.Status = report.GetStatus()
	want.FilledQuantity = report.GetFilledQuantity()
	want.LeavesQuantity = report.GetLeavesQuantity()
	want.AverageFillPrice = report.GetAverageFillPrice()
	want.AsOf = report.GetAsOf()

	if diff := differingFields(got, want); len(diff) != 0 {
		t.Errorf("after a venue fill report the durable view disagrees with the merge on %v. Every "+
			"field there is one the OMS gave this adapter and the exchange never mentioned; losing "+
			"one is a term the system accepted and then quietly did not carry", diff)
	}
}

// TestAnotherTenantSeesNoneOfThisTenantsOrders — the tenant_isolation policy,
// exercised rather than read.
//
// Postgres.Get is `SELECT state FROM venue_orders WHERE order_id = $1` with NO
// tenant predicate: the whole of its scoping is the policy. So this is not a
// test of a WHERE clause, it is the only thing standing between one tenant's
// order id and another tenant's order.
func TestAnotherTenantSeesNoneOfThisTenantsOrders(t *testing.T) {
	f := newPGFixture(t)
	acme, acmePool := f.storeAs(t, "acme")
	beta, _ := f.storeAs(t, "beta")
	ctx := context.Background()

	if err := acme.Record(ctx, routed("o-shared-id")); err != nil {
		t.Fatalf("Record as acme: %v", err)
	}

	if _, ok, err := beta.Get(ctx, "o-shared-id"); err != nil || ok {
		t.Fatalf("tenant beta READ tenant acme's order by id (ok=%v err=%v). Get carries no tenant "+
			"predicate, so this is row-level security not isolating", ok, err)
	}
	if open, oerr := beta.Open(ctx); oerr != nil || len(open) != 0 {
		t.Fatalf("tenant beta's Open returned %d of acme's orders (err %v) — beta's reconciler "+
			"would query another tenant's orders at the exchange", len(open), oerr)
	}

	// AND THE SHARED PRIMARY KEY IS NOT A COLLISION. venue_orders is keyed
	// (tenant_id, order_id), so beta recording the same id must create beta's own
	// row and must not touch acme's — an upsert that keyed on order_id alone
	// would silently overwrite one tenant's order with another's.
	betaOrder := routed("o-shared-id")
	betaOrder.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
	betaOrder.PortfolioId = "pf-beta"
	if err := beta.Record(ctx, betaOrder); err != nil {
		t.Fatalf("Record as beta: %v", err)
	}

	back, ok, err := acme.Get(ctx, "o-shared-id")
	if err != nil || !ok {
		t.Fatalf("acme's own order vanished after beta recorded the same id: ok=%v err=%v", ok, err)
	}
	if back.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED || back.GetPortfolioId() != "pf-alpha" {
		t.Errorf("acme's order now reads %v/%s — tenant beta's write landed on acme's row",
			back.GetStatus(), back.GetPortfolioId())
	}
	var visible int
	if err := acmePool.QueryRow(ctx, `SELECT count(*) FROM venue_orders`).Scan(&visible); err != nil {
		t.Fatalf("count as acme: %v", err)
	}
	if visible != 1 {
		t.Errorf("acme's session sees %d rows in venue_orders, want 1 — two tenants each hold one "+
			"order under this id, and acme may see only its own", visible)
	}
}

// TestAnotherTenantsFillCannotAdvanceThisTenantsOrder.
//
// Progress is a read-modify-write across two Store calls, driven from the
// user-data websocket on every fill. Under RLS the READ is what scopes it: a
// mis-scoped session does not advance the wrong tenant's order, it finds nothing
// and REFUSES with ErrNotInView. That refusal direction is the safe one and it
// is the one asserted here — the alternative is one tenant's fill report writing
// a status onto another tenant's order.
func TestAnotherTenantsFillCannotAdvanceThisTenantsOrder(t *testing.T) {
	f := newPGFixture(t)
	acme, _ := f.storeAs(t, "acme")
	beta, _ := f.storeAs(t, "beta")
	ctx := context.Background()

	if err := acme.Record(ctx, routed("o-cross-fill")); err != nil {
		t.Fatalf("Record as acme: %v", err)
	}

	err := Progress(ctx, beta, venueReport("o-cross-fill", orderpb.OrderStatus_ORDER_STATUS_FILLED, 10, 0))
	if !errors.Is(err, ErrNotInView) {
		t.Fatalf("Progress on tenant beta's store for acme's order returned %v, want ErrNotInView — "+
			"a fill observed on one tenant's session must not reach another tenant's order", err)
	}

	back, ok, gerr := acme.Get(ctx, "o-cross-fill")
	if gerr != nil || !ok {
		t.Fatalf("Get as acme: ok=%v err=%v", ok, gerr)
	}
	if back.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Errorf("acme's order is now %v after beta reported a fill against the same id", back.GetStatus())
	}
	if open, oerr := acme.Open(ctx); oerr != nil || len(open) != 1 {
		t.Errorf("acme's Open = %d orders (err %v), want 1 — beta's report took acme's live order "+
			"out of its own reconciliation set", len(open), oerr)
	}
}

// TestAnUnscopedSessionCannotReadOrWriteTheOrderView — MT-01e.
//
// An unscoped read must ERROR, never return an empty answer: zero rows and
// "nobody said who is asking" are otherwise the same observable event, and the
// invisible one is the dangerous one. On this store it is worse than invisible —
// Seam.Lookup and Seam.OpenOrders are non-failing by design and degrade a store
// error to "I know nothing", so a silently-empty read here is an adapter that
// enriches no fills and reconciles no orders while reporting healthy.
func TestAnUnscopedSessionCannotReadOrWriteTheOrderView(t *testing.T) {
	f := newPGFixture(t)
	scoped, _ := f.storeAs(t, "acme")
	unscoped, _ := f.storeAs(t, "")
	ctx := context.Background()

	if err := scoped.Record(ctx, routed("o-unscoped")); err != nil {
		t.Fatalf("Record as acme: %v", err)
	}

	if _, _, err := unscoped.Get(ctx, "o-unscoped"); err == nil {
		t.Error("a session with no app.tenant_id got an ANSWER out of Get — an unscoped read " +
			"returned instead of failing, which is exactly the silent-empty defect MT-01e ends")
	} else if !strings.Contains(err.Error(), "tenant scope missing") {
		t.Errorf("Get errored with %v, want app_current_tenant()'s named refusal", err)
	}
	if _, err := unscoped.Open(ctx); err == nil {
		t.Error("a session with no app.tenant_id got an ANSWER out of Open — the reconciler would " +
			"conclude this adapter has no open orders and heal nothing")
	}
	if err := unscoped.Record(ctx, routed("o-unscoped-write")); err == nil {
		t.Error("a session with no app.tenant_id WROTE to venue_orders — the row would carry " +
			"whatever tenant the engine defaulted to, or none")
	}
}

// TestGetAnswersForATerminalOrder — the read Server.Execute's already-worked
// refusal rides on (#914/PR #918).
//
// Execute reads the view before it places anything and refuses AlreadyExists on
// a terminal order. That guard is exactly as good as this query: a Get that
// returned ok == false for a row that exists degrades the refusal to "not in the
// view" and the order is placed a second time — a refusal failing OPEN, which is
// the one direction it must not fail.
func TestGetAnswersForATerminalOrder(t *testing.T) {
	f := newPGFixture(t)
	st, _ := f.storeAs(t, "acme")
	ctx := context.Background()

	filled := routed("o-terminal")
	filled.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
	if err := st.Record(ctx, filled); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, ok, err := st.Get(ctx, "o-terminal")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get says a FILLED order is not in the view. Execute would read that as an order " +
			"this adapter has never worked and place it at the exchange again")
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Errorf("Get decoded %v for an order recorded FILLED", got.GetStatus())
	}
	// The row is KEPT. Postgres deliberately diverges from Memory here: venue_orders
	// is not pruned, and infra/dr counts on the row being there.
	if open, oerr := st.Open(ctx); oerr != nil || len(open) != 0 {
		t.Errorf("Open = %d orders (err %v) with only a terminal one recorded", len(open), oerr)
	}
}

// TestAStoreFailureIsNotAMiss.
//
// Execute refuses with Internal on err != nil and PROCEEDS on ok == false, so a
// Get that swallowed a database outage into (nil, false, nil) would convert it
// into a duplicate placement. The two answers must not be the same answer.
func TestAStoreFailureIsNotAMiss(t *testing.T) {
	f := newPGFixture(t)
	st, pool := f.storeAs(t, "acme")
	ctx := context.Background()

	if err := st.Record(ctx, routed("o-outage")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	pool.Close()

	got, ok, err := st.Get(ctx, "o-outage")
	if err == nil {
		t.Fatalf("Get against a closed pool returned ok=%v err=nil — an outage is indistinguishable "+
			"from an order this adapter never worked, and Execute places it again", ok)
	}
	if ok || got != nil {
		t.Errorf("Get returned ok=%v state=%v alongside an error — a caller that checks ok first "+
			"would act on it", ok, got)
	}
	if _, err := st.Open(ctx); err == nil {
		t.Error("Open against a closed pool returned no error — the reconciler would read an empty " +
			"result as 'nothing is open at the venue'")
	}
}

// TestDecodeRefusesAnUnreadableRowRatherThanReturningAnEmptyOrder.
//
// state is opaque BYTEA, so the engine cannot validate it. A blob that does not
// decode must surface as an ERROR: an empty OrderState returned in its place
// would reach the ingester as an order with no instrument, no side and no terms,
// and Seam.Lookup would hand that to a fill enricher as though it were real.
func TestDecodeRefusesAnUnreadableRowRatherThanReturningAnEmptyOrder(t *testing.T) {
	f := newPGFixture(t)
	st, pool := f.storeAs(t, "acme")
	ctx := context.Background()

	// A truncated varint: valid at no schema version, past or future.
	if _, err := pool.Exec(ctx,
		`INSERT INTO venue_orders (order_id, status, state) VALUES ($1, $2, $3)`,
		"o-corrupt", int32(orderpb.OrderStatus_ORDER_STATUS_ROUTED), []byte{0x08}); err != nil {
		t.Fatalf("seed the unreadable row: %v", err)
	}

	got, ok, err := st.Get(ctx, "o-corrupt")
	if err == nil {
		t.Fatalf("Get decoded an unreadable blob without complaint: ok=%v state=%v", ok, got)
	}
	if !strings.Contains(err.Error(), "o-corrupt") {
		t.Errorf("the decode failure does not name the order (%v) — an operator cannot tell which "+
			"row is unreadable", err)
	}
	if _, err := st.Open(ctx); err == nil {
		t.Error("Open decoded the unreadable row without complaint — one bad row must not be " +
			"reported as a working order")
	}
}

// TestARowFromANewerSchemaVersionSurvivesBeingReadAndRewritten.
//
// The two adapters roll independently of whatever wrote a row, so this store WILL
// read rows a newer OrderState produced. Two things must hold and only the second
// is obvious: decode must not reject the row, and Progress — which reads, merges
// and writes it BACK — must not strip the fields it could not name. proto keeps
// unknown fields on the message, and that is what makes the read-modify-write
// safe across versions; a decoder that discarded them would silently delete a
// term on the first fill.
func TestARowFromANewerSchemaVersionSurvivesBeingReadAndRewritten(t *testing.T) {
	f := newPGFixture(t)
	st, pool := f.storeAs(t, "acme")
	ctx := context.Background()

	stored := routed("o-newer")
	blob, err := proto.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Field 9999, varint 7 — a field this build's OrderState does not have.
	future := append(append([]byte{}, blob...), 0xF8, 0xF0, 0x04, 0x07)
	if _, err := pool.Exec(ctx,
		`INSERT INTO venue_orders (order_id, status, state) VALUES ($1, $2, $3)`,
		"o-newer", int32(stored.GetStatus()), future); err != nil {
		t.Fatalf("seed the newer-schema row: %v", err)
	}

	got, ok, err := st.Get(ctx, "o-newer")
	if err != nil || !ok {
		t.Fatalf("Get on a row written by a newer schema: ok=%v err=%v — this adapter would go "+
			"blind to every order the newer version wrote", ok, err)
	}
	if diff := differingFields(got, stored); len(diff) != 0 {
		t.Errorf("a newer-schema row lost %v on the way back — the fields this build DOES know "+
			"must survive one it does not", diff)
	}

	if err := Progress(ctx, st,
		venueReport("o-newer", orderpb.OrderStatus_ORDER_STATUS_FILLED, 10, 0)); err != nil {
		t.Fatalf("Progress over a newer-schema row: %v", err)
	}
	var rewritten []byte
	if err := pool.QueryRow(ctx,
		`SELECT state FROM venue_orders WHERE order_id = $1`, "o-newer").Scan(&rewritten); err != nil {
		t.Fatalf("read back: %v", err)
	}
	var back orderpb.OrderState
	if err := proto.Unmarshal(rewritten, &back); err != nil {
		t.Fatalf("decode the rewritten row: %v", err)
	}
	if len(back.ProtoReflect().GetUnknown()) == 0 {
		t.Error("the fill read the row, merged it and wrote it back WITHOUT the field this build " +
			"does not know. A term the newer version recorded is gone, deleted by an adapter that " +
			"never had a name for it")
	}
}

// TestBothDeployedAdaptersRunTheSameOrderViewSchema.
//
// Everything above is applied against venue-binance's migrations. venue-okx runs
// its own copy of the same DDL in its own database, so a divergence would make
// every assertion above a statement about one deployed adapter and a guess about
// the other. Comments are stripped before comparing — the prose differs by an
// issue number, and it is the DDL that has to match.
//
// Not gated: it reads two directories and needs no database.
func TestBothDeployedAdaptersRunTheSameOrderViewSchema(t *testing.T) {
	binance := migrationDDL(t, binanceMigrations)
	okx := migrationDDL(t, okxMigrations)

	if len(binance) == 0 {
		t.Fatalf("no migrations under %s", binanceMigrations)
	}
	if len(binance) != len(okx) {
		t.Fatalf("venue-binance ships %d migrations and venue-okx %d — one adapter's order view is "+
			"at a schema version the other is not", len(binance), len(okx))
	}
	for name, ddl := range binance {
		other, ok := okx[name]
		if !ok {
			t.Errorf("venue-okx has no %s — its venue_orders is not the table these tests proved", name)
			continue
		}
		if ddl != other {
			t.Errorf("%s differs between venue-binance and venue-okx once comments are stripped. "+
				"Every assertion in this file was made against binance's copy, so a divergence here "+
				"means the OKX adapter's durable order view is unproven", name)
		}
	}
}

// migrationDDL returns each migration's statements with comment lines and blank
// lines removed, keyed by file name.
func migrationDDL(t *testing.T, dir string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	out := make(map[string]string, len(files))
	for _, file := range files {
		body, rerr := os.ReadFile(file)
		if rerr != nil {
			t.Fatalf("read %s: %v", file, rerr)
		}
		var kept []string
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimRight(line, " \t\r")
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			kept = append(kept, line)
		}
		out[filepath.Base(file)] = strings.Join(kept, "\n")
	}
	return out
}
