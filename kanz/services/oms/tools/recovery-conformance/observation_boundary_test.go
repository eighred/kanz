package recoveryconformance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Only StateHealed's recovery observation has this4MiB domain. Deliberately
// encode the unknown field tag noncanonically: successful decode/re-encode is
// not permission to replace the original evidence bytes or their SHA256.
func paddedObservation(t *testing.T, observation *orderpb.StateHealed, size int) []byte {
	t.Helper()
	base := wire(t, observation)
	tag := protowire.AppendTag(nil, 2000, protowire.BytesType)
	tag[len(tag)-1] |= 0x80
	tag = append(tag, 0)
	padding := size - len(base) - len(tag)
	for range 8 {
		next := size - len(base) - len(tag) - protowire.SizeVarint(uint64(padding))
		if next == padding {
			break
		}
		padding = next
	}
	if padding < 0 {
		t.Fatal("observation padding underflow")
	}
	raw := append(base, tag...)
	raw = protowire.AppendBytes(raw, make([]byte, padding))
	if len(raw) != size {
		t.Fatal("observation boundary fixture has incorrect exact size")
	}
	var decoded orderpb.StateHealed
	if proto.Unmarshal(raw, &decoded) != nil || decoded.OrderId != observation.OrderId {
		t.Fatal("observation boundary fixture is not valid canonical message shape")
	}
	if bytes.Equal(raw, wire(t, &decoded)) {
		t.Fatal("raw evidence fixture does not distinguish decode/re-encode")
	}
	return raw
}

func TestRecoveryObservationPaddingOracle(t *testing.T) {
	st := initial()
	for _, size := range []int{(1 << 20) + 17, 4 << 20, (4 << 20) + 1} {
		_ = paddedObservation(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st}, size)
	}
}

func TestRustRecoveryObservationPayloadBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int
		accepted bool
	}{
		{"above-standard-message-bound", (1 << 20) + 17, true},
		{"exact-four-mib", 4 << 20, true},
		{"one-byte-over-four-mib", (4 << 20) + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, owner := range []string{"go", "rust"} {
				f := newFixture(t)
				st := initial()
				store := order.NewPostgres(f.pool)
				if store.Create(f.ctx, st, nil) != nil {
					t.Fatal("seed order failed")
				}
				v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: history(st)}
				tr := startTransport(t, v)
				env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
				payload := paddedObservation(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st}, tc.size)
				var receive func() []*orderpb.ExecutionRecovered
				if tc.accepted {
					receive = f.recovered(t, 2)
				}
				if f.invoke(t, owner, tr, "Observation", env, payload) != tc.accepted {
					t.Fatal("typed StateHealed observation boundary differs")
				}
				got, version, err := store.Load(f.ctx, st.OrderId)
				if err != nil {
					t.Fatal("order read failed")
				}
				if !tc.accepted {
					if version != 0 || !proto.Equal(got, st) {
						t.Fatal("oversized observation changed order")
					}
					for _, table := range []string{"execution_recovery_cases", "execution_account_mappings", "execution_recovery_history", "order_fills", "order_executions", "outbox"} {
						var n int
						if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 0 {
							t.Fatal("oversized observation wrote durable evidence")
						}
					}
					if v.queries.Load() != 0 || v.writes.Load() != 0 {
						t.Fatal("oversized observation reached history or trading")
					}
					continue
				}
				c, err := store.RecoveryCase(f.ctx, env.EventId)
				digest := sha256.Sum256(payload)
				if err != nil || c.Digest != hex.EncodeToString(digest[:]) || !bytes.Equal(c.Evidence, payload) || c.Checkpoint != 2 || c.Status != "investigating" {
					t.Fatal("accepted observation did not preserve exact original bytes/SHA/lifecycle")
				}
				if version != 1 || got.Status != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || dec.Cmp(got.FilledQuantity, &commonpb.Decimal{Coefficient: 2}) != 0 || dec.Cmp(got.AverageFillPrice, &commonpb.Decimal{Coefficient: 1005, Exponent: -1}) != 0 {
					t.Fatal("accepted observation failed independent recovery arithmetic")
				}
				records := receive()
				for _, record := range records {
					if record.Fill.Recovery.PayloadDigest != hex.EncodeToString(digest[:]) || !proto.Equal(record.State, got) {
						t.Fatal("recovered FACT not bound to original observation bytes")
					}
				}
				if !f.invoke(t, owner, tr, "Observation", env, payload) {
					t.Fatal("exact large observation redelivery failed")
				}
				unchanged, err := store.RecoveryCase(f.ctx, env.EventId)
				if err != nil || unchanged.Version != c.Version || !bytes.Equal(unchanged.Evidence, payload) || v.queries.Load() != 1 {
					t.Fatal("large observation replay changed evidence or re-queried venue")
				}
				var decoded orderpb.StateHealed
				if proto.Unmarshal(payload, &decoded) != nil {
					t.Fatal("fixture decode failed")
				}
				if f.invoke(t, owner, tr, "Observation", env, wire(t, &decoded)) {
					t.Fatal("normalized different bytes reused original evidence identity")
				}
				final, err := store.RecoveryCase(f.ctx, env.EventId)
				if err != nil || final.Version != c.Version || final.Digest != c.Digest || !bytes.Equal(final.Evidence, payload) {
					t.Fatal("different raw observation rewrote original evidence")
				}
			}
		})
	}
}
