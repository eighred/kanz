package grpcsrv_test

import (
	"context"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
	orderstore "github.com/eighred/kanz/services/oms/internal/order"
)

func stateReadFixture(t *testing.T) (*orderstore.MemoryStore, *orderpb.OrderState) {
	t.Helper()
	store := orderstore.NewMemoryStore()
	row := &orderpb.OrderState{OrderId: "O", PortfolioId: "P", InstrumentId: "I", Status: orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED,
		OrderedQuantity: &commonpb.Decimal{Coefficient: 9007199254740993, Exponent: -11}, FilledQuantity: &commonpb.Decimal{Coefficient: 2, Exponent: -11},
		LeavesQuantity: &commonpb.Decimal{Coefficient: 9007199254740991, Exponent: -11}, AsOf: timestamppb.New(time.Now().UTC()),
		VenueAccountId: "private-account", Venue: "private-venue"}
	row.OrderedQuantity.ProtoReflect().SetUnknown([]byte{0x1a, 0x01, 'x'})
	row.AsOf.ProtoReflect().SetUnknown([]byte{0x1a, 0x01, 'x'})
	if err := store.Create(context.Background(), row, nil); err != nil {
		t.Fatal(err)
	}
	return store, row
}

func TestStateReadProjectsExactRecordedStateWithoutPrivateFields(t *testing.T) {
	store, row := stateReadFixture(t)
	readAt := time.Now().UTC().Add(time.Second)
	resp, err := grpcsrv.NewStateRead(context.Background(), store, "t1", func() time.Time { return readAt }).OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetVersion() != 0 || resp.GetOwnerTenant() != "t1" || resp.GetRecordedStatus() != row.GetStatus().String() || !resp.GetReadAt().AsTime().Equal(readAt) {
		t.Fatalf("recorded state binding changed: %v", resp)
	}
	if q := resp.GetOrderedQuantity(); q.GetKnowledge() != querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED || q.GetValue().GetCoefficient() != 9007199254740993 || q.GetValue().GetExponent() != -11 {
		t.Fatalf("exact quantity lost: %v", q)
	}
	if len(resp.GetOrderedQuantity().GetValue().ProtoReflect().GetUnknown()) != 0 || len(resp.GetAsOf().ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("opaque unselected metadata crossed the read-only wire projection")
	}
	if text := protojson.Format(resp); strings.Contains(text, "private-") || strings.Contains(text, "venue_account") || strings.Contains(text, "approval") {
		t.Fatalf("private state escaped projection: %s", text)
	}
}

func TestStateReadMissingAndForeignPortfolioAreIndistinguishable(t *testing.T) {
	store, _ := stateReadFixture(t)
	s := grpcsrv.NewStateRead(context.Background(), store, "t1", nil)
	_, missing := s.OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "absent"})
	_, foreign := s.OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "other", OrderId: "O"})
	if status.Code(missing) != codes.NotFound || status.Code(foreign) != codes.NotFound || status.Convert(missing).Message() != status.Convert(foreign).Message() {
		t.Fatalf("existence oracle: %v / %v", missing, foreign)
	}
}

func TestStateReadQuarantineAndUnknownStatusWithholdValues(t *testing.T) {
	for _, quarantined := range []bool{false, true} {
		store, row := stateReadFixture(t)
		if quarantined {
			row.Quarantine = &orderpb.OrderQuarantine{}
		} else {
			row.Status = orderpb.OrderStatus(999)
		}
		if err := store.Save(context.Background(), row, 0, nil, ""); err != nil {
			t.Fatal(err)
		}
		resp, err := grpcsrv.NewStateRead(context.Background(), store, "t1", nil).OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetRecordedStatus() != "UNKNOWN" || resp.GetStatusKnowledge() != querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_UNKNOWN || resp.GetOrderedQuantity().GetValue() != nil || resp.GetFilledQuantity().GetValue() != nil || resp.GetLeavesQuantity().GetValue() != nil {
			t.Fatalf("unresolved state became usable: %v", resp)
		}
	}
}

type successfulAfterCancelReader struct {
	row     *orderpb.OrderState
	entered chan struct{}
}

func (r *successfulAfterCancelReader) Load(ctx context.Context, _ string) (*orderpb.OrderState, int64, error) {
	close(r.entered)
	<-ctx.Done()
	return r.row, 0, nil
}

func TestStateReadRejectsLateSuccessfulLoadAfterLifecycleCancellation(t *testing.T) {
	_, row := stateReadFixture(t)
	reader := &successfulAfterCancelReader{row: row, entered: make(chan struct{})}
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	s := grpcsrv.NewStateRead(root, reader, "t1", nil)
	done := make(chan error, 1)
	go func() {
		_, err := s.OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
		done <- err
	}()
	select {
	case <-reader.entered:
	case <-time.After(time.Second):
		t.Fatal("read did not enter loader")
	}
	cancelRoot()
	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("late successful read erased lifecycle cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled read did not join")
	}
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoin()
	if err := s.JoinReads(joinCtx); err != nil {
		t.Fatal(err)
	}
}
