// The signal→order path against a REAL spine.
//
// Every other test in this package injects a fake Publisher that satisfies the
// interface and never validates an envelope — which is exactly why both publishes
// here shipped without a payload_schema_ref and were rejected by the first real
// broker, and why nobody noticed that no JetStream stream was even bound to
// strategy.> or order.>. A fake Publisher cannot fail the way a broker fails.
//
// This test is the guard: it drives Emit through a real bus.Producer over real
// NATS/JetStream and reads both events back off the wire.
//
//	docker run -d --name kanz-nats -p 4222:4222 nats:2 -js
//	TEST_NATS_URL=nats://localhost:4222 go test ./internal/signal/translate/...
package translate_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/signal/translate"
	"github.com/eighred/kanz/pkg/bus"
)

func TestIntegration_EmitReachesTheWire(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the signal→order path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mirrors infra/nats/bootstrap-job.yaml: the EXECUTION stream carries
	// execution.>, strategy.> AND order.>. Before that, neither subject was bound to
	// any stream, and bus.Publish is a JetStream publish — so these events had
	// nowhere to land.
	setupConn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("setup connect: %v", err)
	}
	// t.Cleanup, NOT defer. A deferred Close runs BEFORE the registered cleanups, so
	// the DeleteStream below was firing on a connection this line had already torn
	// down: the stream survived the test, kept its messages, and poisoned the next
	// run — the test passed on a fresh broker and failed on every repeat. Cleanups
	// run last-registered-first, so registering the close FIRST makes it run LAST.
	t.Cleanup(setupConn.Close)
	js, err := jetstream.New(setupConn)
	if err != nil {
		t.Fatalf("setup jetstream: %v", err)
	}
	// These are the REAL subjects — that is the whole point of this test, since the
	// EXEC-M7a bug was that no stream was bound to them at all. When the production
	// topology is provisioned (CI bootstraps the same script prod applies), this
	// binds to the real EXECUTION stream and proves the real binding.
	//
	// `tenant.>` is here because the ORDER COMMAND IS TENANT-ROUTED on the wire
	// (MT-02, #358/#360): translate publishes it on
	// bus.TenantRoutedSubject(tenant, SubjectSubmit), which does NOT match `order.>`.
	// Without this the JetStream publish has no stream to land on and Emit fails —
	// the exact class of defect this test was written for, one prefix later. The real
	// topology carries it too (infra/nats/bootstrap-job.yaml provisions TENANT_ORDER
	// for "tenant.*.order.>").
	bustest.EnsureSubjects(t, ctx, js, "EXECUTION_IT", []string{"execution.>", "strategy.>", "order.>", "tenant.>"})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "translate-it"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "translate-it", ProducerVersion: "test", Tenant: "eighred",
	})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	tr, err := translate.New(translate.Options{
		Prices:    translate.StaticPrices{"BTC-USD": big.NewRat(50000, 1)},
		Equity:    translate.StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1)},
		Positions: translate.StaticPositions{},
		Alloc: translate.StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(1, 1)},
		}},
		Publisher: producer,
		Gate:      translate.OpenGate(nil),
		Authority: itAuthority(t),
	})
	if err != nil {
		t.Fatalf("translate.New: %v", err)
	}

	// A FRESH signal id per run. The order command's idempotency key is derived from
	// it and is therefore deterministic — which is the point in production, and a
	// trap here: the real EXECUTION stream carries a 2-minute duplicate window
	// (infra/nats/bootstrap-job.yaml), so re-running this test with a fixed id had
	// the broker SILENTLY DEDUPLICATE the SubmitOrder. The publish "succeeded", the
	// command never landed, and the test timed out looking for it. The old test
	// deleted and recreated its own stream each run, which wiped the dedup state and
	// hid this entirely. Exactly-once is working; the test was replaying a command.
	signalID := translate.DeterministicID(fmt.Sprintf("it-signal-%d", time.Now().UnixNano()))
	res, err := tr.Emit(ctx, translate.Intent{
		SignalID:     signalID,
		StrategyID:   "momentum",
		FundID:       "fund-alpha",
		InstrumentID: "BTC-USD",
		Action:       signalpb.SignalAction_SIGNAL_ACTION_BUY,
		Size:         big.NewRat(1, 1),
		SizeType:     signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY,
		OrderType:    orderpb.OrderType_ORDER_TYPE_MARKET,
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_IOC,
		Source:       signalpb.SignalSource_SIGNAL_SOURCE_NATIVE_ENGINE,
	})
	if err != nil {
		// This is the failure the whole test exists to catch: an envelope the broker
		// rejects, or a subject with no stream to land on.
		t.Fatalf("Emit over a real spine = %v — the signal→order path does not reach the wire", err)
	}
	if len(res.OrderIDs) != 1 {
		t.Fatalf("OrderIDs = %v, want 1", res.OrderIDs)
	}

	// WHERE THE COMMAND LANDED IS A FACT ABOUT THIS BROKER, SO IT IS MEASURED, NOT
	// ASSUMED. See commandDeliverySubject: the wire subject is tenant-routed, and
	// whether the broker keeps that prefix or rewrites it away depends on its
	// tenancy arrangement. Guessing is what made this test pass on a bare local
	// broker and time out in CI with no explanation.
	wireSubject := bus.TenantRoutedSubject(itTenant, translate.SubjectSubmit)
	cmdSubject := commandDeliverySubject(t, ctx, js, wireSubject, translate.SubjectSubmit, signalID)

	// SUBSCRIBING AFTER THE PUBLISH IS DELIBERATE AND SAFE. bus.Subscribe creates a
	// durable with the default DeliverAll policy, so it reads the stream from its
	// first retained message rather than from now. Subscribing first meant racing a
	// bare `time.After(time.Second)` that was commented "both subscriptions are up"
	// and was in fact a guess — under -race on a loaded runner a slow
	// CreateOrUpdateConsumer would have made this test fail as a timeout with the
	// real error still sitting unread in a channel.
	var mu sync.Mutex
	got := map[string]*envelopepb.Envelope{}
	// COLLECT ONLY THIS RUN'S EVENTS. correlation_id is the signal id (Emit sets it
	// on both), and CI points every package at ONE broker whose EXECUTION stream
	// already holds other tests' order commands. Keying on event_type alone would
	// let a foreign SubmitOrder satisfy this test — and would make the tenant
	// assertion below read someone else's envelope.
	collect := func(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
		if env.GetCorrelationId() != signalID {
			return nil // another test's traffic on a shared spine; ack and ignore
		}
		mu.Lock()
		defer mu.Unlock()
		got[env.GetEventType()] = env
		return nil
	}
	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	// A Subscribe error must not be swallowed. A subscription that never started is
	// indistinguishable from a subject nobody publishes to — which is the entire
	// class of bug this test exists to catch. It is read INSIDE the wait loop below,
	// not once at a fixed deadline, so an error arriving late still fails the test
	// as itself rather than as a timeout.
	subErr := make(chan error, 2)
	for _, sub := range []struct{ subject, group string }{
		{translate.SubjectSignal, "translate-it-sig"},
		{cmdSubject, "translate-it-cmd"},
	} {
		go func() {
			err := consumer.Subscribe(subCtx, sub.subject, sub.group, collect)
			if err != nil && subCtx.Err() == nil {
				subErr <- fmt.Errorf("subscribe %s: %w", sub.subject, err)
			}
		}()
	}

	waitFor(t, subErr, func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(got) == 2 {
			return ""
		}
		// NAME THE HALF THAT IS MISSING. "timed out waiting for both events to land"
		// was the entire diagnosis this test offered for a real routing defect, and
		// it did not say which of the two subjects went quiet.
		var missing []string
		for et, subject := range map[string]string{
			translate.SubjectSignal: translate.SubjectSignal,
			translate.SubjectSubmit: cmdSubject,
		} {
			if _, ok := got[et]; !ok {
				missing = append(missing, et+" (subscribed on "+subject+")")
			}
		}
		sort.Strings(missing)
		return "never arrived: " + strings.Join(missing, ", ")
	})

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []struct{ eventType, schemaRef string }{
		{translate.SubjectSignal, "signal.v1.StrategySignal:1"},
		{translate.SubjectSubmit, "order.v1.SubmitOrder:1"},
	} {
		env, ok := got[want.eventType]
		if !ok {
			t.Fatalf("%s never arrived", want.eventType)
		}
		// Derived by the producer from the payload — Validate requires it, and no
		// caller on this path sets it.
		if env.GetPayloadSchemaRef() != want.schemaRef {
			t.Errorf("%s payload_schema_ref = %q, want %q",
				want.eventType, env.GetPayloadSchemaRef(), want.schemaRef)
		}
		// THE TENANT CAME FROM CONFIGURATION, READ BACK OFF A REAL BROKER (#632).
		//
		// This is the assertion that makes the test evidence rather than a delivery
		// check. `acme` appears nowhere in the Intent; `fund-alpha` does. Until the
		// FundAuthority existed the tenant WAS the fund id, so an envelope carrying
		// "fund-alpha" here is the defect, live, on the wire.
		//
		// IT IS ON THE ENVELOPE, NOT THE SUBJECT, ON PURPOSE. A broker that maps the
		// routing prefix away — which both the dev/CI broker and production's
		// __system__ account do — erases the tenant from the subject by design. The
		// envelope field survives every mapping, so it is the one place this property
		// can be checked on any broker this test is pointed at.
		if env.GetTenantId() != itTenant {
			t.Errorf("%s envelope tenant_id = %q, want %q — the tenant must come from the "+
				"strategy/fund binding, never from the caller-supplied fund_id (#632)",
				want.eventType, env.GetTenantId(), itTenant)
		}
	}
}

// commandDeliverySubject returns the subject the order command was actually
// STORED under, having asked the broker.
//
// THE TEST CANNOT ASSUME THIS, AND ASSUMING IT IS WHAT BROKE CI. translate
// publishes the command on bus.TenantRoutedSubject(tenant, SubjectSubmit). What
// happens next depends on the broker's tenancy arrangement, and the two this
// repository actually runs disagree:
//
//   - test/backing/nats-dev.conf — the dev rig's broker, and the one CI starts —
//     has a single account and carries `mappings` that rewrite
//     `tenant.*.order.order.submit` to `order.order.submit` AT INGRESS, before
//     stream matching. The command is stored in EXECUTION under the LOGICAL name
//     and TENANT_ORDER stays empty. Production's __system__ account does the same
//     thing for its own prefix (infra/nats/tenancy.yaml's `mappings` block).
//   - a bare `nats-server -js`, which a developer runs locally, has no mappings.
//     The prefix survives and TENANT_ORDER (or a bustest scratch stream) holds it.
//
// A test hard-coded to either one is a test that passes in one environment and
// times out in the other with no explanation — which is exactly what happened.
// Both destinations are correct; which one is in play is a property of the
// deployment, so it is measured here and named in the failure if neither has it.
//
// IT MATCHES ON THIS RUN'S correlation_id, not on "a message exists". These
// streams retain for 24h, so on any broker that is not freshly created — a
// developer's, or the dev rig's — a PREVIOUS run's command sits on the other
// candidate subject and would select a route this run never published to. That
// is the same ambient-state dependence that let the original defect hide;
// answering it with "some message is there" would only move it.
func commandDeliverySubject(t *testing.T, ctx context.Context, js jetstream.JetStream, wire, logical, signalID string) string {
	t.Helper()
	for _, candidate := range []string{wire, logical} {
		stream, err := bustest.StreamFor(ctx, js, candidate)
		if err != nil {
			continue // no stream carries it at all
		}
		st, err := js.Stream(ctx, stream)
		if err != nil {
			continue
		}
		msg, err := st.GetLastMsgForSubject(ctx, candidate)
		if err != nil {
			continue // nothing has ever been stored on this subject
		}
		env, _, err := bus.Unframe(msg.Data)
		if err != nil {
			continue // not one of ours; a foreign publisher on a shared spine
		}
		if env.GetCorrelationId() == signalID {
			return candidate
		}
	}
	t.Fatalf("this run's order command (correlation_id %s) reached NEITHER %q nor %q, on a "+
		"publish the broker acked.\n\n"+
		"Emit returned success, so a stream accepted the message — it is stored somewhere "+
		"neither the tenant-routed wire subject nor the logical subject reaches. Check this "+
		"broker's `mappings` against the two arrangements in this function's doc.",
		signalID, wire, logical)
	return ""
}

// itTenant is the tenant that owns fund-alpha in this test's binding table. It is
// deliberately NOT "fund-alpha": before #632 the tenant WAS the fund id, so a
// test using the same string for both would pass whether or not the identity
// default came back.
const itTenant = "acme"

func itAuthority(t *testing.T) translate.FundAuthority {
	t.Helper()
	a, err := translate.NewFundAuthority(
		map[string]string{"fund-alpha": itTenant},
		map[string][]string{"momentum": {"fund-alpha"}},
	)
	if err != nil {
		t.Fatalf("NewFundAuthority: %v", err)
	}
	return a
}

// waitFor polls until settled returns "" (satisfied), failing with whatever
// reason it last reported.
//
// IT TAKES THE SUBSCRIPTION ERROR CHANNEL because a subscription that failed to
// start produces the same silence as a subject nothing publishes to, and the
// previous shape read that channel exactly once, at a one-second deadline. An
// error arriving at 1.1s was never read: the test ran its full ten seconds and
// reported a timeout while the real cause sat in a buffered channel.
func waitFor(t *testing.T, subErr <-chan error, settled func() string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	reason := "nothing observed yet"
	for time.Now().Before(deadline) {
		select {
		case err := <-subErr:
			t.Fatalf("a subscription failed: %v", err)
		default:
		}
		if reason = settled(); reason == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-subErr:
		t.Fatalf("a subscription failed: %v", err)
	default:
	}
	t.Fatalf("timed out after 10s — %s", reason)
}
