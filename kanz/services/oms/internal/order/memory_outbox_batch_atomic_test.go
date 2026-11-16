package order

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func memoryBatchState(id string, status orderpb.OrderStatus, filled int64) *orderpb.OrderState {
	st := &orderpb.OrderState{
		OrderId: id, Status: status, PortfolioId: "batch-portfolio", InstrumentId: "batch-asset",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC, LimitPrice: &commonpb.Decimal{Coefficient: 100},
		OrderedQuantity: &commonpb.Decimal{Coefficient: 2},
		FilledQuantity:  &commonpb.Decimal{Coefficient: filled}, AsOf: timestamppb.New(t0),
	}
	if filled > 0 {
		st.AverageFillPrice = &commonpb.Decimal{Coefficient: 100}
	}
	return st
}

func memoryBatchFill(t *testing.T, orderID, fillID string) *orderpb.Fill {
	t.Helper()
	fill := &orderpb.Fill{
		OrderId: orderID, FillId: fillID, InstrumentId: "batch-asset", Side: orderpb.Side_SIDE_BUY,
		Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 100},
		Venue: "BINANCE", VenueAccountId: "batch-account", VenueExecutionId: "venue-" + fillID,
		Fee:        &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1, Exponent: -2}, CurrencyCode: "USD"},
		ExecutedAt: timestamppb.New(t0),
	}
	if err := fillfact.Validate(fill); err != nil {
		t.Fatalf("fill fixture: %v", err)
	}
	if _, ok := dec.InDomainDeep(fill); !ok || fillfact.ExecutionKey(fill) == fillID {
		t.Fatal("fill fixture must be in-domain and have a scoped execution key")
	}
	return fill
}

// Use actual typed capture builders. There is exactly one execution in a Save
// batch, so the journal helper can succeed before Append reaches its refusal.
func memoryBatchRecords(t *testing.T, st *orderpb.OrderState, fill *orderpb.Fill) []outbox.Record {
	t.Helper()
	ctx := bus.WithTenantID(context.Background(), testTenant)
	emitter := NewEmitter(nil)
	first, err := emitter.AcceptedFact(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	middle, err := emitter.RoutedFact(ctx, st.OrderId, "BINANCE", "batch-venue-order", t0)
	if err != nil {
		t.Fatal(err)
	}
	status := commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_ACCEPTED
	if fill != nil {
		first = middle
		middle, err = emitter.FillFact(ctx, fill, st)
		if err != nil {
			t.Fatal(err)
		}
		status = commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED
	}
	last, err := emitter.OutcomeFact(ctx, st.OrderId, status, "batch capture", "", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	records := []outbox.Record{first, middle, last}
	for _, record := range records {
		if _, err := record.Event(); err != nil {
			t.Fatalf("typed record fixture: %v", err)
		}
	}
	return records
}

func memoryBatchQueue(t *testing.T, store *MemoryStore) []outbox.Pending {
	t.Helper()
	ctx := context.Background()
	keys, err := store.outbox.PendingKeys(ctx, 32)
	if err != nil {
		t.Fatal(err)
	}
	var rows []outbox.Pending
	for _, key := range keys {
		pending, err := store.outbox.Pending(ctx, key, 32)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, pending...)
	}
	if len(rows) != store.outbox.PendingCount() {
		t.Fatal("queue snapshot is incomplete")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

func memoryBatchAppended(t *testing.T, store *MemoryStore, before []outbox.Pending, records []outbox.Record) []outbox.Pending {
	t.Helper()
	rows := memoryBatchQueue(t, store)
	if len(rows) != len(before)+len(records) || !reflect.DeepEqual(rows[:len(before)], before) {
		t.Fatalf("queue retained a refused prefix or changed preexisting records: got %d, want %d", len(rows), len(before)+len(records))
	}
	for i, record := range records {
		if !reflect.DeepEqual(rows[len(before)+i].Record, record) {
			t.Fatalf("appended record %d differs", i)
		}
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].ID <= rows[i-1].ID {
			t.Fatal("queue IDs must preserve monotonic enqueue order")
		}
	}
	return rows
}

func memoryBatchClaims(t *testing.T, store *MemoryStore, want map[string]memoryFillClaim) {
	t.Helper()
	if len(store.appliedFills) != len(want) {
		t.Fatalf("fill claims = %d, want %d", len(store.appliedFills), len(want))
	}
	for key, expected := range want {
		got, ok := store.appliedFills[key]
		if !ok || got.orderID != expected.orderID || got.rawID != expected.rawID || !proto.Equal(got.fill, expected.fill) {
			t.Fatalf("fill claim %q changed", key)
		}
	}
}

func TestMemoryStoreCreateRejectsEntireOutboxBatch(t *testing.T) {
	for _, invalid := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("invalid=%d", invalid), func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryStore()
			prior := memoryBatchState("batch-existing", orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW, 0)
			if err := store.Create(ctx, prior, memoryBatchRecords(t, prior, nil)[:1]); err != nil {
				t.Fatal(err)
			}
			before := memoryBatchQueue(t, store)
			st := memoryBatchState("batch-create", orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW, 0)
			valid := memoryBatchRecords(t, st, nil)
			mixed := append([]outbox.Record(nil), valid...)
			mixed[invalid].TenantID = ""
			if err := store.Create(ctx, st, mixed); err == nil || err.Error() != "outbox: record with no tenant" {
				t.Fatalf("Create failed for a different reason: %v", err)
			}
			if _, _, err := store.Load(ctx, st.OrderId); !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused order exists: %v", err)
			}
			if got, version, err := store.Load(ctx, prior.OrderId); err != nil || version != 0 || !proto.Equal(got, prior) {
				t.Fatalf("preexisting order changed: %v version=%d err=%v", got, version, err)
			}
			memoryBatchClaims(t, store, nil)
			memoryBatchAppended(t, store, before, nil)
			if err := store.Create(ctx, st, valid); err != nil {
				t.Fatalf("valid Create retry: %v", err)
			}
			if got, version, err := store.Load(ctx, st.OrderId); err != nil || version != 0 || !proto.Equal(got, st) {
				t.Fatalf("retry order differs: %v version=%d err=%v", got, version, err)
			}
			after := memoryBatchAppended(t, store, before, valid)
			if err := store.Create(ctx, st, valid); !errors.Is(err, ErrExists) {
				t.Fatalf("duplicate Create = %v, want ErrExists", err)
			}
			memoryBatchAppended(t, store, after, nil)
			memoryBatchClaims(t, store, nil)
		})
	}
}

func TestMemoryStoreSaveRejectsEntireOutboxBatchBeforeFillClaim(t *testing.T) {
	for _, invalid := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("invalid=%d", invalid), func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryStore()
			initial := memoryBatchState("batch-save", orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW, 0)
			if err := store.Create(ctx, initial, memoryBatchRecords(t, initial, nil)[:1]); err != nil {
				t.Fatal(err)
			}
			prior := memoryBatchState(initial.OrderId, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 1)
			priorFill := memoryBatchFill(t, initial.OrderId, "prior-fill")
			if err := store.Save(ctx, prior, 0, memoryBatchRecords(t, prior, priorFill)[1:2], priorFill.FillId); err != nil {
				t.Fatal(err)
			}
			_, version, err := store.Load(ctx, prior.OrderId)
			if err != nil || version != 1 {
				t.Fatalf("precondition version=%d err=%v", version, err)
			}
			claims := map[string]memoryFillClaim{fillfact.ExecutionKey(priorFill): {orderID: prior.OrderId, rawID: priorFill.FillId, fill: priorFill}}
			before := memoryBatchQueue(t, store)
			st := memoryBatchState(prior.OrderId, orderpb.OrderStatus_ORDER_STATUS_FILLED, 2)
			fill := memoryBatchFill(t, st.OrderId, "new-fill")
			valid := memoryBatchRecords(t, st, fill)
			mixed := append([]outbox.Record(nil), valid...)
			mixed[invalid].TenantID = ""
			// Discriminate the target error from an earlier journal/claim refusal.
			if decoded, err := announcedExecution(st.OrderId, fill.FillId, mixed); err != nil || !proto.Equal(decoded, fill) {
				t.Fatalf("journal precondition: fill=%v err=%v", decoded, err)
			}
			if err := store.Save(ctx, st, version, mixed, fill.FillId); err == nil || err.Error() != "outbox: record with no tenant" {
				t.Fatalf("Save failed for a different reason: %v", err)
			}
			if got, after, err := store.Load(ctx, st.OrderId); err != nil || after != version || !proto.Equal(got, prior) {
				t.Fatalf("refused Save changed state/version: %v version=%d err=%v", got, after, err)
			}
			memoryBatchClaims(t, store, claims)
			memoryBatchAppended(t, store, before, nil)
			if err := store.Save(ctx, st, version, valid, fill.FillId); err != nil {
				t.Fatalf("valid Save retry: %v", err)
			}
			if got, after, err := store.Load(ctx, st.OrderId); err != nil || after != version+1 || !proto.Equal(got, st) {
				t.Fatalf("retry state/version differs: %v version=%d err=%v", got, after, err)
			}
			claims[fillfact.ExecutionKey(fill)] = memoryFillClaim{orderID: st.OrderId, rawID: fill.FillId, fill: fill}
			memoryBatchClaims(t, store, claims)
			after := memoryBatchAppended(t, store, before, valid)
			// Use the current version: only the already-committed fill may refuse.
			if err := store.Save(ctx, st, version+1, valid, fill.FillId); !errors.Is(err, ErrFillApplied) {
				t.Fatalf("duplicate Save = %v, want ErrFillApplied", err)
			}
			if got, after, err := store.Load(ctx, st.OrderId); err != nil || after != version+1 || !proto.Equal(got, st) {
				t.Fatalf("duplicate Save changed state/version: %v version=%d err=%v", got, after, err)
			}
			memoryBatchAppended(t, store, after, nil)
			memoryBatchClaims(t, store, claims)
		})
	}
}
