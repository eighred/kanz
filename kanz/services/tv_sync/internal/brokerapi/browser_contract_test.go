package brokerapi

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/tv_sync/internal/projection"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBrowserAccountReadContract(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	projector := projection.New(func() time.Time { return now }, nil)
	mux := http.NewServeMux()
	New(projector).Routes(mux)
	quantity := dec(9007199254740993)
	price := &commonpb.Decimal{Coefficient: 325, Exponent: -2}
	fill := &orderpb.Fill{
		FillId: "fill-1", OrderId: "order-1", InstrumentId: "X", Side: orderpb.Side_SIDE_BUY,
		Quantity: quantity, Price: price, Venue: "XNAS", ExecutedAt: timestamppb.New(now),
		Fee: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 125, Exponent: -3}, CurrencyCode: "USD"},
	}
	event, err := proto.Marshal(&orderpb.OrderFilled{OrderId: fill.OrderId, Fill: fill, State: &orderpb.OrderState{
		OrderId: fill.OrderId, PortfolioId: "fund-a", InstrumentId: fill.InstrumentId, Side: fill.Side,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: quantity, FilledQuantity: quantity,
		LeavesQuantity: dec(0), AverageFillPrice: price, LimitPrice: dec(4), ParentOrderId: "parent-1",
		Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, AsOf: timestamppb.New(now),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := projector.Handle(t.Context(), &envelopepb.Envelope{TenantId: "tenant-a", EventType: "order.order.filled"}, event); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/account_read.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]any)
	for _, resource := range []string{"accounts", "state", "positions", "orders", "executions"} {
		path := "/broker/accounts"
		if resource != "accounts" {
			path += "/fund-a/" + resource
			if rec := get(t, mux, path, "other-tenant"); rec.Code != http.StatusNotFound {
				t.Fatalf("other tenant %s: status %d", path, rec.Code)
			}
		}
		response := get(t, mux, path, "tenant-a")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, response.Code)
		}
		var body any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		got[resource] = body
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("browser fixture differs from served broker responses: %v", got)
	}
}
