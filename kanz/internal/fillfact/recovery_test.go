package fillfact

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type recoveryCapture struct{ messages []bus.Message }

func (c *recoveryCapture) Publish(_ context.Context, m bus.Message) error {
	c.messages = append(c.messages, m)
	return nil
}
func (*recoveryCapture) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not a consumer")
}
func (*recoveryCapture) Close() error { return nil }

func TestRecoveryAcknowledgementProvesItsEnvelopeAndExactExecution(t *testing.T) {
	ctx := bus.WithTenantID(context.Background(), "acme")
	now := time.Unix(1700000000, 0)
	f := &orderpb.Fill{OrderId: "order", FillId: "alias", Venue: "venue", VenueAccountId: "account", VenueExecutionId: "execution", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 100}, ExecutedAt: timestamppb.New(now)}
	digest, err := ExecutionDigest(f)
	if err != nil {
		t.Fatal(err)
	}
	f.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case", MappingVersion: "version", SourceCursor: "cursor", PayloadDigest: strings.Repeat("a", 64), ExecutionDigest: digest, ObservedAt: timestamppb.New(now)}
	capture := &recoveryCapture{}
	producer, err := bus.NewProducer(capture, bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{RecoveryPositionApplied, RecoveryLedgerApplied} {
		record, err := RecoveryAck(ctx, subject, f, now)
		if err != nil {
			t.Fatal(err)
		}
		event, err := record.Event()
		if err != nil {
			t.Fatal(err)
		}
		if err := producer.Publish(ctx, event); err != nil {
			t.Fatal(err)
		}
		wire := capture.messages[len(capture.messages)-1]
		env, payload, err := bus.Unframe(wire.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		var ack orderpb.ExecutionRecoveryApplied
		if err := proto.Unmarshal(payload, &ack); err != nil || ack.ExecutionDigest != digest || ack.ExecutionKey != ExecutionKey(f) || env.GetTenantId() != "acme" || env.GetEventType() != subject {
			t.Fatalf("ack=%v env=%v err=%v", &ack, env, err)
		}
	}
	changed := proto.Clone(f).(*orderpb.Fill)
	changed.Price.Coefficient++
	if _, err := RecoveryAck(ctx, RecoveryLedgerApplied, changed, now); err == nil {
		t.Fatal("changed economics acknowledged")
	}
	changed = proto.Clone(f).(*orderpb.Fill)
	changed.Recovery.MappingVersion = ""
	if _, err := RecoveryAck(ctx, RecoveryLedgerApplied, changed, now); err == nil {
		t.Fatal("unmapped execution acknowledged")
	}
	if _, err := RecoveryAck(context.Background(), RecoveryLedgerApplied, f, now); err == nil {
		t.Fatal("untenanted acknowledgement created")
	}
}
