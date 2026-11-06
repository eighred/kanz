package storeconformance

import (
	"context"
	"errors"
	"github.com/eighred/kanz/internal/bustest"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"math/big"
	"os"
	"reflect"
	"testing"
	"time"

	"bytes"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/platform/subject"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/position"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func positionFill(id string, side orderpb.Side, quantity, price int64, when time.Time) *orderpb.Fill {
	f := fill()
	f.FillId, f.VenueExecutionId, f.Side = id, id, side
	f.Quantity, f.Price = &commonpb.Decimal{Coefficient: quantity}, &commonpb.Decimal{Coefficient: price}
	f.ExecutedAt = timestamppb.New(when)
	return f
}

// Decode independently of internal/dec and the costbasis implementation. The
// expected outcomes below are fixed rational accounting examples, not Go output.
func positionRat(d *commonpb.Decimal) *big.Rat {
	if d == nil {
		return nil
	}
	n := big.NewRat(d.Coefficient, 1)
	e := int64(d.Exponent)
	if e < -64 || e > 64 {
		return nil
	}
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(absPositionExponent(e)), nil)
	if e < 0 {
		return n.Quo(n, new(big.Rat).SetInt(power))
	}
	return n.Mul(n, new(big.Rat).SetInt(power))
}
func absPositionExponent(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func positionNumbers(t *testing.T, state *domainpb.PositionState, want ...string) {
	t.Helper()
	fields := []*commonpb.Decimal{state.GetQuantity(), state.GetAveragePrice(), state.GetRealizedPnl().GetAmount(), state.GetMarketValue().GetAmount(), state.GetUnrealizedPnl().GetAmount()}
	if len(want) != len(fields) {
		t.Fatal("oracle shape")
	}
	for i, d := range fields {
		expected, ok := new(big.Rat).SetString(want[i])
		actual := positionRat(d)
		if !ok || actual == nil || actual.Cmp(expected) != 0 {
			t.Fatalf("position field %d differs from independent rational oracle", i)
		}
	}
	for _, money := range []*commonpb.Money{state.MarketValue, state.RealizedPnl, state.UnrealizedPnl} {
		if money == nil || money.CurrencyCode != "USD" || money.Amount == nil {
			t.Fatal("missing canonical reporting currency/amount")
		}
	}
}

type positionExample struct {
	fill    *orderpb.Fill
	numbers []string
}

func positionExamples() []positionExample {
	return []positionExample{
		{positionFill("a", orderpb.Side_SIDE_BUY, 2, 100, at), []string{"2", "100", "0", "200", "0"}},
		{positionFill("b", orderpb.Side_SIDE_BUY, 1, 130, at.Add(time.Second)), []string{"3", "110", "0", "390", "60"}},
		{positionFill("c", orderpb.Side_SIDE_SELL, 1, 140, at.Add(2*time.Second)), []string{"2", "110", "30", "280", "60"}},
		{positionFill("d", orderpb.Side_SIDE_SELL, 4, 90, at.Add(3*time.Second)), []string{"-2", "90", "-10", "-180", "0"}},
		{positionFill("e", orderpb.Side_SIDE_BUY, 1, 80, at.Add(4*time.Second)), []string{"-1", "90", "0", "-80", "10"}},
		{positionFill("f", orderpb.Side_SIDE_BUY, 1, 100, at.Add(5*time.Second)), []string{"0", "0", "-10", "0", "0"}},
	}
}

func TestPositionIndependentAccountingOracle(t *testing.T) {
	book := position.NewBook("USD")
	cash := new(big.Rat)
	for _, c := range positionExamples() {
		a, err := book.Apply(context.Background(), "portfolio", c.fill, c.fill.ExecutedAt.AsTime(), nil)
		if err != nil {
			t.Fatal(err)
		}
		positionNumbers(t, a.Venue, c.numbers...)
		trade := new(big.Rat).Mul(positionRat(c.fill.Quantity), positionRat(c.fill.Price))
		if c.fill.Side == orderpb.Side_SIDE_BUY {
			trade.Neg(trade)
		}
		cash.Add(cash, trade)
		wealth := new(big.Rat).Add(cash, positionRat(a.Venue.MarketValue.Amount))
		pnl := new(big.Rat).Add(positionRat(a.Venue.RealizedPnl.Amount), positionRat(a.Venue.UnrealizedPnl.Amount))
		if wealth.Cmp(pnl) != 0 {
			t.Fatal("gross cash/mark conservation failed")
		}
	}
}

func positionRecords(t *testing.T, tenant, portfolio string, f *orderpb.Fill, a *position.Applied) []outbox.Record {
	t.Helper()
	key := subject.PositionFor(tenant, portfolio, f.InstrumentId)
	var records []outbox.Record
	for i, state := range []*domainpb.PositionState{a.Aggregate, a.Venue} {
		subj, eventType := key, "risk.position.changed"
		if i == 1 {
			subj, eventType = subject.VenuePositionFor(tenant, portfolio, f.Venue, f.InstrumentId), subject.VenuePositionChanged
		}
		r, err := outbox.From(bus.WithTenantID(context.Background(), tenant), bus.Event{Subject: subj, EventType: eventType, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "risk", EventTime: state.AsOf.AsTime(), PartitionKey: key, PayloadSchemaRef: "domain.v1.PositionState:1", Payload: state})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

func positionRequest(t *testing.T, f *fixture, portfolio string, fill *orderpb.Fill, when time.Time, announce bool) map[string]any {
	t.Helper()
	r := map[string]any{"mode": "position-apply", "schema": f.schema, "tenant": f.tenant, "portfolio_id": portfolio, "fill": wire(t, fill), "at": clock(when), "base_currency": "USD"}
	if announce {
		placeholder := &domainpb.PositionState{AsOf: timestamppb.New(when)}
		records := positionRecords(t, f.tenant, portfolio, fill, &position.Applied{Venue: placeholder, Aggregate: placeholder})
		r["records"] = []map[string]any{factJSON(records[0]), factJSON(records[1])}
	}
	return r
}

func positionResult(t *testing.T, r fixtureResult) *position.Applied {
	t.Helper()
	expect(t, r.Status, "applied")
	a := &position.Applied{Venue: &domainpb.PositionState{}, Aggregate: &domainpb.PositionState{}}
	if len(r.Venue) == 0 || len(r.Aggregate) == 0 || proto.Unmarshal(r.Venue, a.Venue) != nil || proto.Unmarshal(r.Aggregate, a.Aggregate) != nil {
		t.Fatal("missing typed position result")
	}
	return a
}

func positionParity(t *testing.T, got, want *position.Applied) {
	t.Helper()
	if !proto.Equal(got.Venue, want.Venue) || !proto.Equal(got.Aggregate, want.Aggregate) {
		t.Fatal("complete canonical position state differs from Go")
	}
}

func positionDurable(t *testing.T, f *fixture) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"positions", "position_fills", "position_execution_history", "position_execution_basis", "execution_fee_revisions", "outbox"} {
		var rows string
		if err := f.pool.QueryRow(f.ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM "+table+" t").Scan(&rows); err != nil {
			t.Fatal("durable position fixture read failed")
		}
		out[table] = rows
	}
	return out
}

func TestRustPositionStoreTransactions(t *testing.T) {
	t.Run("rational-close-flip-restart-and-snapshot", func(t *testing.T) {
		f := newFixture(t)
		portfolio := "portfolio"
		oracle := position.NewBook("USD")
		for _, c := range positionExamples() {
			when := c.fill.ExecutedAt.AsTime()
			want, err := oracle.Apply(f.ctx, portfolio, c.fill, when, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := positionResult(t, runResult(t, positionRequest(t, f, portfolio, c.fill, when, true)))
			positionParity(t, got, want)
			positionNumbers(t, got.Venue, c.numbers...)
			read, err := position.NewPostgres(f.open(f.tenant), "USD").Snapshot(f.ctx, portfolio, when)
			if err != nil {
				t.Fatal(err)
			}
			r := runResult(t, map[string]any{"mode": "position-snapshot", "schema": f.schema, "tenant": f.tenant, "portfolio_id": portfolio, "at": clock(when)})
			expect(t, r.Status, "snapshot")
			var snap domainpb.PortfolioSnapshot
			if proto.Unmarshal(r.State, &snap) != nil || !proto.Equal(read, &snap) {
				t.Fatal("restart Snapshot differs from canonical Go read")
			}
		}

		unknown := positionFill("unknown", orderpb.Side_SIDE_BUY, 1, 100, at.Add(6*time.Second))
		unknown.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 199, protowire.VarintType), 7))
		unknown.Price.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 198, protowire.BytesType), []byte("preserved")))
		positionResult(t, runResult(t, positionRequest(t, f, portfolio, unknown, unknown.ExecutedAt.AsTime(), false)))
		var journal []byte
		if err := f.pool.QueryRow(f.ctx, "SELECT fill FROM position_execution_history WHERE raw_fill_id=$1", unknown.FillId).Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(journal, wire(t, unknown)) {
			t.Fatal("canonical root/nested unknown journal bytes changed")
		}

		var count int
		if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox").Scan(&count) != nil || count != 12 {
			t.Fatal("both announcements must commit for every fold")
		}
	})
	t.Run("go-rust-cross-venue-and-duplicate", func(t *testing.T) {
		f := newFixture(t)
		g := position.NewPostgres(f.pool, "USD")
		portfolio := "portfolio"
		a := positionFill("a", orderpb.Side_SIDE_BUY, 10, 100, at)
		if _, err := g.Apply(f.ctx, portfolio, a, at, nil); err != nil {
			t.Fatal(err)
		}
		b := positionFill("b", orderpb.Side_SIDE_SELL, 10, 120, at.Add(time.Second))
		b.Venue = "OTHER"
		got := positionResult(t, runResult(t, positionRequest(t, f, portfolio, b, b.ExecutedAt.AsTime(), true)))
		positionNumbers(t, got.Aggregate, "0", "110", "0", "0", "0")
		positionNumbers(t, got.Venue, "-10", "120", "0", "-1200", "0")
		duplicate := positionResult(t, runResult(t, positionRequest(t, f, portfolio, b, b.ExecutedAt.AsTime(), true)))
		positionParity(t, duplicate, got)
		var count int
		if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM position_fills").Scan(&count) != nil || count != 2 {
			t.Fatal("duplicate claimed twice")
		}
		before := positionDurable(t, f)
		b.Price.Coefficient++
		expect(t, run(t, positionRequest(t, f, portfolio, b, at, true)), "IdentityConflict")
		if !reflect.DeepEqual(before, positionDurable(t, f)) {
			t.Fatal("conflict changed durable owners")
		}
	})
	t.Run("announce-and-commit-failure-rollback", func(t *testing.T) {
		f := newFixture(t)
		base := positionDurable(t, f)
		r := positionRequest(t, f, "portfolio", fill(), at, true)
		r["other"] = "announce-fail"
		expect(t, run(t, r), "InvalidInput")
		if !reflect.DeepEqual(base, positionDurable(t, f)) {
			t.Fatal("announcement failure leaked writes")
		}
		if _, err := f.pool.Exec(f.ctx, `CREATE FUNCTION refuse_position_commit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'fixture'; END$$; CREATE CONSTRAINT TRIGGER refuse_position_commit AFTER INSERT ON outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_position_commit()`); err != nil {
			t.Fatal(err)
		}
		r["other"] = ""
		expect(t, run(t, r), "CommitUnconfirmed")
		if !reflect.DeepEqual(base, positionDurable(t, f)) {
			t.Fatal("commit refusal leaked any owner")
		}
		if _, err := f.pool.Exec(f.ctx, "DROP TRIGGER refuse_position_commit ON outbox"); err != nil {
			t.Fatal(err)
		}
		positionResult(t, runResult(t, r))
	})
	t.Run("backdated-complete-history-and-incomplete-refusal", func(t *testing.T) {
		f := newFixture(t)
		oracle := position.NewBook("USD")
		first := positionFill("first", orderpb.Side_SIDE_BUY, 1, 100, at)
		last := positionFill("last", orderpb.Side_SIDE_SELL, 1, 150, at.Add(2*time.Second))
		late := positionFill("late", orderpb.Side_SIDE_BUY, 1, 200, at.Add(time.Second))
		late.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case"}
		for _, v := range []*orderpb.Fill{first, last, late, late} {
			want, err := oracle.Apply(f.ctx, "portfolio", v, v.ExecutedAt.AsTime(), nil)
			if err != nil {
				t.Fatal(err)
			}
			got := positionResult(t, runResult(t, positionRequest(t, f, "portfolio", v, v.ExecutedAt.AsTime(), true)))
			positionParity(t, got, want)
		}
		final, err := oracle.Apply(f.ctx, "portfolio", late, late.ExecutedAt.AsTime(), nil)
		if err != nil {
			t.Fatal(err)
		}
		positionNumbers(t, final.Venue, "1", "150", "0", "150", "0")
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO positions (tenant_id,portfolio_id,venue,instrument_id,quantity,average_price,realized_pnl) VALUES ($1,'legacy','VENUE','instrument','1','100','0')`, f.tenant); err != nil {
			t.Fatal(err)
		}
		unknown := proto.Clone(late).(*orderpb.Fill)
		unknown.FillId, unknown.VenueExecutionId = "legacy-history", "legacy-history"
		unknown.InstrumentId = "instrument"
		unknown.Venue = "VENUE"
		before := positionDurable(t, f)
		if _, err := position.NewPostgres(f.pool, "USD").Apply(f.ctx, "legacy", unknown, at, nil); !errors.Is(err, position.ErrPositionHistoryUnknown) {
			t.Fatalf("canonical Go legacy history refusal: %v", err)
		}
		expect(t, run(t, positionRequest(t, f, "legacy", unknown, at, true)), "PositionHistoryUnknown")
		if !reflect.DeepEqual(before, positionDurable(t, f)) {
			t.Fatal("unknown history partially committed")
		}
	})
	t.Run("approved-fee-head-replay-and-rollback", func(t *testing.T) {
		f := newFixture(t)
		original := fill()
		oracle := position.NewBook("USD")
		first := revision(t, original, "fee-first", &commonpb.Decimal{Coefficient: 2})
		second := revision(t, first, "fee-second", &commonpb.Decimal{Coefficient: 1})
		for _, v := range []*orderpb.Fill{original, first, second, first, second, original} {
			want, err := oracle.Apply(f.ctx, "portfolio", v, at, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := positionResult(t, runResult(t, positionRequest(t, f, "portfolio", v, at, true)))
			positionParity(t, got, want)
			positionNumbers(t, got.Venue, "2", "125", "0", "250", "0")
		}
		before := positionDurable(t, f)
		stale := revision(t, first, "fee-stale", &commonpb.Decimal{Coefficient: 3})
		expect(t, run(t, positionRequest(t, f, "portfolio", stale, at, true)), "FeeRevision")
		if !reflect.DeepEqual(before, positionDurable(t, f)) {
			t.Fatal("stale fee changed durable state")
		}
		next := revision(t, second, "fee-next", &commonpb.Decimal{Coefficient: 4})
		r := positionRequest(t, f, "portfolio", next, at, true)
		r["other"] = "announce-fail"
		expect(t, run(t, r), "InvalidInput")
		if !reflect.DeepEqual(before, positionDurable(t, f)) {
			t.Fatal("fee journal escaped failed announcement")
		}
		r["other"] = ""
		positionResult(t, runResult(t, r))
	})
	t.Run("tenant-same-identities-positive-isolation", func(t *testing.T) {
		f := newFixture(t)
		a := positionFill("same", orderpb.Side_SIDE_BUY, 2, 100, at)
		ga := positionResult(t, runResult(t, positionRequest(t, f, "portfolio", a, at, true)))
		before := positionDurable(t, f)
		b := *f
		b.tenant = "tenant-b"
		b.pool = f.open(b.tenant)
		other := proto.Clone(a).(*orderpb.Fill)
		other.Quantity.Coefficient = 3
		other.Price.Coefficient = 200
		gb := positionResult(t, runResult(t, positionRequest(t, &b, "portfolio", other, at, true)))
		positionNumbers(t, ga.Venue, "2", "100", "0", "200", "0")
		positionNumbers(t, gb.Venue, "3", "200", "0", "600", "0")
		if !reflect.DeepEqual(before, positionDurable(t, f)) {
			t.Fatal("tenant B changed tenant A")
		}
		for _, owner := range []*fixture{f, &b} {
			var claims, announcements int
			if err := owner.pool.QueryRow(owner.ctx, `SELECT (SELECT count(*) FROM position_fills),(SELECT count(*) FROM outbox WHERE tenant_id=$1)`, owner.tenant).Scan(&claims, &announcements); err != nil || claims != 1 || announcements != 2 {
				t.Fatal("tenant-local claims/announcements not independent")
			}
		}
	})
	t.Run("go-rust-cross-venue-lock-and-atomic-announcement-order", func(t *testing.T) {
		f := newFixture(t)
		portfolio := "portfolio"
		a := positionFill("go", orderpb.Side_SIDE_BUY, 1, 100, at)
		b := positionFill("rust", orderpb.Side_SIDE_BUY, 2, 100, at)
		b.Venue = "OTHER"
		entered, release := make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		goDone := make(chan error, 1)
		records := positionRecords(t, f.tenant, portfolio, a, &position.Applied{Aggregate: &domainpb.PositionState{AsOf: timestamppb.New(at)}, Venue: &domainpb.PositionState{AsOf: timestamppb.New(at)}})
		go func() {
			_, err := position.NewPostgres(f.pool, "USD").Apply(f.ctx, portfolio, a, at, func(ctx context.Context, applied *position.Applied) ([]outbox.Record, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				for i, state := range []*domainpb.PositionState{applied.Aggregate, applied.Venue} {
					payload, err := proto.Marshal(state)
					if err != nil {
						return nil, err
					}
					records[i].Payload = payload
					records[i].EventTime = state.AsOf.AsTime()
				}
				return records, nil
			})
			goDone <- err
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("Go transaction did not reach announcement")
		}
		request := positionRequest(t, f, portfolio, b, at, true)
		rustDone := make(chan fixtureResult, 1)
		go func() { defer close(rustDone); rustDone <- runResult(t, request) }()
		waiting := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%app_current_tenant%'`).Scan(&n)
			if err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				waiting = true
				break
			}
			select {
			case <-rustDone:
				t.Fatal("Rust bypassed live Go holding lock")
			case <-time.After(20 * time.Millisecond):
			}
		}
		if !waiting {
			t.Fatal("actual cross-process advisory wait not observed")
		}
		close(release)
		if err := <-goDone; err != nil {
			t.Fatal(err)
		}
		result, ok := <-rustDone
		if !ok {
			t.Fatal("Rust process failed before result")
		}
		got := positionResult(t, result)
		positionNumbers(t, got.Aggregate, "3", "100", "0", "300", "0")
		key := subject.PositionFor(f.tenant, portfolio, a.InstrumentId)
		pending := pendingRelay(t, f, key)
		if len(pending) != 4 {
			t.Fatal("two folds did not atomically announce both projections")
		}
		for i, want := range []int64{1, 3} {
			var state domainpb.PositionState
			if proto.Unmarshal(pending[2*i].Record.Payload, &state) != nil || positionRat(state.Quantity).Cmp(big.NewRat(want, 1)) != 0 {
				t.Fatal("aggregate announcement order regressed")
			}
		}
	})

	t.Run("whole-result-capacity-corruption-and-unsafe-rls", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			rows, width int
			want        string
		}{{"row-cap", 10001, 0, "Capacity"}, {"byte-cap", 3000, 380, "Capacity"}, {"corrupt-rational", 1, 0, "InvalidInput"}} {
			t.Run(tc.name, func(t *testing.T) {
				f := newFixture(t)
				if _, err := f.pool.Exec(f.ctx, `INSERT INTO positions (portfolio_id,venue,instrument_id,quantity,average_price,realized_pnl) SELECT 'portfolio','VENUE',repeat('x',$1)||i::text,'1','100','0' FROM generate_series(1,$2) i`, tc.width, tc.rows); err != nil {
					t.Fatal(err)
				}
				if tc.name == "corrupt-rational" {
					if _, err := f.pool.Exec(f.ctx, "UPDATE positions SET quantity='not-a-rational'"); err != nil {
						t.Fatal(err)
					}
				}
				before := positionDurable(t, f)
				result := runResult(t, map[string]any{"mode": "position-snapshot", "schema": f.schema, "tenant": f.tenant, "portfolio_id": "portfolio", "at": clock(at), "base_currency": "USD"})
				expect(t, result.Status, tc.want)
				if len(result.State) != 0 {
					t.Fatal("refused snapshot exposed partial state")
				}
				if !reflect.DeepEqual(before, positionDurable(t, f)) {
					t.Fatal("failed snapshot changed storage")
				}
			})
		}
		f := newFixture(t)
		before := positionDurable(t, f)
		if _, err := f.pool.Exec(f.ctx, "ALTER TABLE positions NO FORCE ROW LEVEL SECURITY"); err != nil {
			t.Fatal(err)
		}
		expect(t, run(t, positionRequest(t, f, "portfolio", fill(), at, true)), "UnsafeRole")
		if _, err := f.pool.Exec(f.ctx, "ALTER TABLE positions FORCE ROW LEVEL SECURITY"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, positionDurable(t, f)) {
			t.Fatal("unsafe RLS refusal changed durable owners")
		}
	})

	t.Run("large-and-fractional-canonical-output", func(t *testing.T) {
		for _, quantity := range []*commonpb.Decimal{{Coefficient: 200000000000}, {Coefficient: 1, Exponent: -10}, {Coefficient: 123456789, Exponent: -9}} {
			f := newFixture(t)
			v := fill()
			v.Quantity = quantity
			oracle := position.NewBook("USD")
			want, err := oracle.Apply(f.ctx, "portfolio", v, at, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := positionResult(t, runResult(t, positionRequest(t, f, "portfolio", v, at, true)))
			positionParity(t, got, want)
			var stored string
			if err := f.pool.QueryRow(f.ctx, "SELECT quantity FROM positions").Scan(&stored); err != nil {
				t.Fatal(err)
			}
			exact, ok := new(big.Rat).SetString(stored)
			if !ok || exact.Cmp(positionRat(quantity)) != 0 {
				t.Fatal("durable rational quantity rounded")
			}
		}
	})

}

// Actual broker proof begins from Rust-written transactional outbox records and
// compares their full frames with the established independent Go Producer oracle.
func TestRustPositionStorePostgresNATS(t *testing.T) {
	f := newFixture(t)
	address := os.Getenv("TEST_NATS_URL")
	if address == "" {
		t.Skip("TEST_NATS_URL unavailable: actual position broker required")
	}
	nc, err := nats.Connect(address, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatal("broker unavailable")
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	v := fill()
	portfolio := f.schema
	key := subject.PositionFor(f.tenant, portfolio, v.InstrumentId)
	subjects := []string{key, subject.VenuePositionFor(f.tenant, portfolio, v.Venue, v.InstrumentId)}
	bustest.EnsureSubjects(t, f.ctx, js, "POSITION_"+f.schema, subjects)
	consumers := make([]jetstream.Consumer, 2)
	for i, subj := range subjects {
		stream, err := bustest.StreamFor(f.ctx, js, subj)
		if err != nil {
			t.Fatal(err)
		}
		name := "position-" + f.schema
		if i == 1 {
			name += "-venue"
		}
		consumers[i], err = js.CreateConsumer(f.ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: subj, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if js.DeleteConsumer(ctx, stream, name) != nil {
				t.Error("position consumer cleanup failed")
			}
		})
	}
	got := positionResult(t, runResult(t, positionRequest(t, f, portfolio, v, at, true)))
	pending := pendingRelay(t, f, key)
	if len(pending) != 2 {
		t.Fatal("atomic position announcements absent")
	}
	for i, state := range []*domainpb.PositionState{got.Aggregate, got.Venue} {
		var stored domainpb.PositionState
		if proto.Unmarshal(pending[i].Record.Payload, &stored) != nil || !proto.Equal(state, &stored) || !pending[i].Record.EventTime.Equal(state.AsOf.AsTime()) {
			t.Fatal("outbox payload/time differs from committed position")
		}
	}
	since := time.Now()
	relayResult(t, runResult(t, relayRequest(f, "flush", key, 8)), 2, "")
	for i, c := range consumers {
		b := relayBroker{consumer: c}
		verifyRelayFrame(t, pending[i].Record, b.next(t, key), 1, since)
	}
	if len(pendingRelay(t, f, key)) != 0 {
		t.Fatal("published position facts remain pending")
	}
}
