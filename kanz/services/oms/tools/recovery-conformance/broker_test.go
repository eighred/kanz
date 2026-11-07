package recoveryconformance

import (
	"context"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

// Arm the actual JetStream pull before publication; the canonical stream may
// compact the last value of a subject, so post-hoc reads cannot prove every fill.
func (f *fixture) recovered(t *testing.T, count int) func() []*orderpb.ExecutionRecovered {
	return f.recoveredWithin(t, count, 35*time.Second)
}

func (f *fixture) recoveredWithin(t *testing.T, count int, wait time.Duration) func() []*orderpb.ExecutionRecovered {
	t.Helper()
	endpoint := brokerURL(t)
	if endpoint == "" {
		t.Skip("real JetStream unavailable")
	}
	nc, err := nats.Connect(endpoint)
	if err != nil {
		t.Fatal("fixture broker unavailable")
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, f.ctx, js, "RECOVERY_"+f.schema, []string{"order.>"})
	stream, err := bustest.StreamFor(f.ctx, js, fillfact.SubjectRecovered)
	if err != nil {
		t.Fatal(err)
	}
	consumerName := f.schema + "_" + f.tenant
	consumer, err := js.CreateConsumer(f.ctx, stream, jetstream.ConsumerConfig{Name: consumerName, FilterSubject: fillfact.SubjectRecovered, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := js.DeleteConsumer(cleanupCtx, stream, consumerName); err != nil {
			var api *jetstream.APIError
			if errors.As(err, &api) {
				t.Errorf("recovery consumer cleanup failed: api_code=%d error_code=%d", api.Code, api.ErrorCode)
			} else {
				t.Errorf("recovery consumer cleanup failed: context_canceled=%t deadline=%t", errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded))
			}
		}
	})
	batch, err := consumer.Fetch(count, jetstream.FetchMaxWait(wait))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := consumer.Info(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.NumWaiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery pull not armed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	type received struct {
		records []*orderpb.ExecutionRecovered
		err     error
	}
	done := make(chan received, 1)
	// Consume and acknowledge concurrently with publication: a large history
	// must not deadlock behind MaxAckPending or lose compacted stream entries.
	go func() {
		result := make([]*orderpb.ExecutionRecovered, 0, count)
		var failure error
		for message := range batch.Messages() {
			env, payload, err := bus.Unframe(message.Data())
			if err != nil || bus.Validate(env) != nil {
				failure = fmt.Errorf("actual recovery frame invalid")
				break
			}
			if env.TenantId != f.tenant || env.CausationId != "observation" || env.CorrelationId != "fixture-correlation" || env.EventType != fillfact.SubjectRecovered {
				failure = fmt.Errorf("actual recovery lineage differs")
				break
			}
			var record orderpb.ExecutionRecovered
			if proto.Unmarshal(payload, &record) != nil || record.Fill == nil || record.Fill.Recovery == nil {
				failure = fmt.Errorf("actual recovery payload missing")
				break
			}
			if message.DoubleAck(f.ctx) != nil {
				failure = fmt.Errorf("recovery broker acknowledgement failed")
				break
			}
			result = append(result, &record)
		}
		if failure == nil && (batch.Error() != nil || len(result) != count) {
			failure = fmt.Errorf("actual recovered FACT inventory incomplete")
		}
		done <- received{result, failure}
	}()
	return func() []*orderpb.ExecutionRecovered {
		t.Helper()
		select {
		case outcome := <-done:
			if outcome.err != nil {
				t.Fatal(outcome.err)
			}
			return outcome.records
		case <-f.ctx.Done():
			t.Fatal("recovered FACT observation deadline expired")
		}
		return nil
	}

}

func (f *fixture) ensureBroker(t *testing.T) {
	t.Helper()
	endpoint := brokerURL(t)
	if endpoint == "" {
		t.Skip("real JetStream unavailable")
	}
	nc, err := nats.Connect(endpoint)
	if err != nil {
		t.Fatal("fixture broker unavailable")
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, f.ctx, js, "RECOVERY_"+f.schema, []string{"order.>"})
	name, err := bustest.StreamFor(f.ctx, js, fillfact.SubjectRecovered)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(f.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(f.ctx)
	if err != nil || nc.MaxPayload() <= 0 {
		t.Fatal("actual broker limits unavailable")
	}
	// Numeric readback only: -1 means the stream defers to the server limit.
	t.Logf("actual recovery broker limits: server_max_payload=%d stream_max_msg_size=%d", nc.MaxPayload(), info.Config.MaxMsgSize)
}

func brokerURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_NATS_URL")
	if raw == "" {
		t.Skip("real JetStream unavailable")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "nats" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("literal credential-free fixture broker required")
	}
	host := parsed.Hostname()
	if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		t.Fatal("disposable loopback broker required")
	}
	return raw
}
