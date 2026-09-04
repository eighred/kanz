package bus_test

// A BROADCAST SUBSCRIPTION CAN SAY HOW FAR BEHIND IT IS, AND ITS DELIVERY
// CONTRACT IS CHOSEN BY SUBJECT (#1009).
//
// Both halves are asserted HERE, against a real broker, and not only in the unit
// tests — because both defects were at a CALL SITE rather than in a function.
//
//   - `tuningFor` resolving market.* to tickTuning is a property of the resolver.
//     That the ephemeral path CALLS the resolver is a property of
//     subscribeEphemeral, and reverting that one line leaves every resolver unit
//     test green. Measured: it does. So the contract is read back off the
//     BROKER's own ConsumerInfo, which is the only place the call site's choice
//     becomes visible.
//
//   - `kanz_bus_pending_messages` existing for a broadcast subscription is a
//     property of the whole chain — subscribeEphemeral reporting the generated
//     name, Consumer starting the poller, NATSClient answering by that name. A
//     test double for any link would prove the link it replaced.
//
// Gated on TEST_NATS_URL. A JetStream broker needs no Docker.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// noopHandler acks whatever arrives: these tests are about the CONSUMER the
// broker created and the series it publishes, not about what a handler does.
func noopHandler(context.Context, *envelopepb.Envelope, []byte) error { return nil }

func broadcastRig(t *testing.T) (string, jetstream.JetStream) {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive a broadcast subscription over a real broker")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return url, js
}

// consumerOn returns the ephemeral consumer THIS TEST caused to be created:
// bound to stream, filtering subject, and created at or after `since`.
//
// THE `since` FENCE IS LOAD-BEARING, AND WAS ADDED BECAUSE ITS ABSENCE HID A
// MUTATION. subscribeEphemeral sets InactiveThreshold 5m, so a consumer from a
// previous run of this test is still on the broker and still matches the
// subject. Without the fence, a run that reverted the tuning read the PREVIOUS
// run's correctly-tuned consumer and passed — a test that measured the broker's
// memory of the last green run rather than this build.
func consumerOn(
	t *testing.T, ctx context.Context, js jetstream.JetStream, stream, subject string, since time.Time,
) *jetstream.ConsumerInfo {
	t.Helper()
	st, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatalf("stream %s: %v", stream, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for info := range st.ListConsumers(ctx).Info() {
			if info.Config.FilterSubject == subject && info.Config.Durable == "" &&
				!info.Created.Before(since) {
				return info
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no ephemeral consumer on %s filtering %q created since %s appeared within 15s",
		stream, subject, since.Format(time.RFC3339Nano))
	return nil
}

// THE CONTRACT THE BROKER ACTUALLY GAVE THE CONSUMER.
//
// Read off ConsumerInfo rather than asserted against the resolver, because the
// defect was that the ephemeral path never called the resolver. A unit test on
// tuningFor stays green through that revert; this does not.
func TestABroadcastConsumersContractIsChosenBySubject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	url, js := broadcastRig(t)

	for _, tc := range []struct {
		name          string
		subject       string
		stream        string
		maxDeliver    int
		maxAckPending int
		why           string
	}{
		{
			name: "a tick gets the tick contract", subject: "market.BROADCASTIT.trade", stream: "MARKET",
			maxDeliver: 5, maxAckPending: 512,
			why: "compliance's price spine and the OMS's are broadcast subscriptions on market.*; the " +
				"control class gives them MaxAckPending 16 and unbounded redelivery of a stale quote",
		},
		{
			name: "a control signal keeps the control contract", subject: "platform.mode.changed", stream: "PLATFORM",
			maxDeliver: -1, maxAckPending: 16,
			why: "a bounded MaxDeliver lets the broker stop re-offering a control message the pod could " +
				"not apply — \"I could not read the brake signal\" becoming \"carry on\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			since := time.Now().UTC()
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "broadcast-tuning-it"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			consumer, err := bus.NewConsumer(client)
			if err != nil {
				t.Fatal(err)
			}

			subCtx, stop := context.WithCancel(ctx)
			defer stop()
			go func() {
				_ = consumer.SubscribeBroadcast(subCtx, tc.subject, noopHandler)
			}()

			info := consumerOn(t, ctx, js, tc.stream, tc.subject, since)
			if info.Config.MaxDeliver != tc.maxDeliver {
				t.Errorf("MaxDeliver = %d, want %d — %s", info.Config.MaxDeliver, tc.maxDeliver, tc.why)
			}
			if info.Config.MaxAckPending != tc.maxAckPending {
				t.Errorf("MaxAckPending = %d, want %d — %s", info.Config.MaxAckPending, tc.maxAckPending, tc.why)
			}
		})
	}
}

// THE SERIES THAT DID NOT EXIST.
//
// Every in-process control fold in the estate is armed by a broadcast
// subscription, and before this change none of them could report a backlog:
// Backlog resolves a consumer through durableName(group, subject), and an
// ephemeral consumer has no durable name. A control that had silently fallen
// behind and one that was current exported identical metrics.
func TestABroadcastSubscriptionPublishesABacklogSeries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	url, _ := broadcastRig(t)

	reg := prometheus.NewRegistry()
	metrics := bus.NewBusMetrics(reg)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "broadcast-backlog-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(metrics))
	if err != nil {
		t.Fatal(err)
	}

	const subject = "platform.mode.changed"
	subCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = consumer.SubscribeBroadcast(subCtx, subject, noopHandler)
	}()

	// The poller reads BEFORE its first tick, so the series appears within a poll
	// timeout rather than a poll interval.
	deadline := time.Now().Add(45 * time.Second)
	for {
		labels, found := findBacklogSeries(t, reg, "kanz_bus_pending_messages", subject)
		if found {
			if labels["delivery"] != "broadcast" {
				t.Errorf("delivery = %q, want \"broadcast\" — a per-pod ephemeral consumer and a shared "+
					"queue-group durable must not be summable, and the estate's KEDA queries select an "+
					"explicit group=", labels["delivery"])
			}
			if labels["group"] != "" {
				t.Errorf("group = %q, want empty — a broadcast consumer has no group", labels["group"])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("kanz_bus_pending_messages carries NO series for a broadcast subscription after 45s.\n\n" +
				"That is the whole of #1009: every in-process control fold in the estate — the compliance " +
				"position book, both mandate registries, the OMS's mark and cash folds, the halt gate — is " +
				"armed by one of these, and a fold that has silently fallen behind exports exactly the " +
				"metrics of one that is current.")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// findBacklogSeries returns the label set of the first sample of name whose `subject`
// label matches, reading the REGISTRY rather than a counter this test kept — the
// series is the artefact under test.
func findBacklogSeries(t *testing.T, reg *prometheus.Registry, name, subject string) (map[string]string, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["subject"] == subject {
				return labels, true
			}
		}
	}
	return nil, false
}
