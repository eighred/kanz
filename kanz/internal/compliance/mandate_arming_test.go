package compliance_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/bustest"
	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// TestARestartedGateArmsItselfWithEveryMandateInForce pins EXEC-M13.
//
// The OMS's PRE-TRADE compliance gate evaluates every order against the mandate in
// force for its portfolio, out of an in-memory registry fed from the bus. That
// registry was filled by a DURABLE CONSUMER GROUP, which resumes at its last ack —
// so a RESTARTED OMS came back with an EMPTY REGISTRY and passed every order. The
// control did not fail. It DISARMED, silently, on every rolling update, while the
// pod reported ready throughout.
//
// The test is therefore a RESTART, not a cold start: one gate process consumes and
// ACKS the mandates, and then a SECOND one boots — exactly the sequence a rolling
// update performs, and exactly the one that came back ungoverned.
//
// Both portfolios must be armed, not one. A gate armed for portfolio A and blind to
// portfolio B is not a gate — and "we replayed the last message" is precisely the
// half-fix that a single flat subject would have given us.
func TestARestartedGateArmsItselfWithEveryMandateInForce(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the mandate arming path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	// Bind the REAL MANDATE stream when the topology is provisioned (CI bootstraps
	// it); fall back to a scratch one on a bare broker. Its production shape is
	// MaxMsgsPerSubject=1 with NO max-age — keep exactly the mandate IN FORCE per
	// portfolio, and never age it out, because a mandate that expires off the stream
	// is a portfolio that has quietly become ungoverned.
	bustest.EnsureSubjects(t, ctx, js, "MANDATE_IT_"+suffix, []string{"compliance.mandate.>"})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "mandate-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "kanz-mandate", ProducerVersion: "it", Tenant: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}

	// UNIQUE per run: the MANDATE stream is compacted and PERSISTENT, so reusing
	// portfolio ids would let a previous run's mandates arm this one and hide the very
	// failure this test exists to catch.
	alpha, beta := "pf-alpha-"+suffix, "pf-beta-"+suffix

	// THE OPERATOR PUTS TWO PORTFOLIOS UNDER MANDATE — before anything is listening.
	pub := comp.NewPublisher(producer)
	for _, pf := range []string{alpha, beta} {
		m := &compliancepb.Mandate{
			MandateId:   "m-" + pf,
			TenantId:    "acme",
			PortfolioId: pf,
			Version:     1,
			EffectiveAt: timestamppb.New(time.Now().UTC()),
		}
		if err := pub.Publish(ctx, m, nil, "operator:test", "arming test"); err != nil {
			t.Fatalf("publish %s: %v", pf, err)
		}
	}

	// arm boots ONE gate process and reports which portfolios it ends up governing.
	arm := func() map[string]bool {
		registry := comp.NewMandateRegistry()
		mandateConsumer := comp.NewMandateConsumer(registry, nil)
		subCtx, stop := context.WithCancel(ctx)
		defer stop()

		var mu sync.Mutex
		armed := map[string]bool{}
		go func() {
			_ = consumer.SubscribeBroadcast(subCtx, comp.SubjectMandateAll,
				func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
					err := mandateConsumer.Handle(ctx, env, payload)
					mu.Lock()
					defer mu.Unlock()
					for _, pf := range []string{alpha, beta} {
						if _, ok, _ := registry.Mandate(ctx, pf, time.Now()); ok {
							armed[pf] = true
						}
					}
					return err
				})
		}()

		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(armed)
			mu.Unlock()
			if n == 2 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]bool, len(armed))
		for k, v := range armed {
			out[k] = v
		}
		return out
	}

	// FIRST BOOT — the gate arms and ACKS the mandates.
	if got := arm(); len(got) != 2 {
		t.Fatalf("first boot armed for %d of 2 portfolios (%v)", len(got), got)
	}

	// THE RESTART. This is the whole test: a durable consumer group would now resume
	// PAST the mandates it already acked, and this second process would come up
	// holding NOTHING.
	if got := arm(); len(got) != 2 {
		t.Fatalf("AFTER A RESTART the gate came up armed for %d of 2 portfolios (%v).\n"+
			"A pre-trade compliance gate that boots with an empty registry does not refuse orders — it PASSES them. "+
			"The control does not fail, it DISARMS, silently, while the pod reports ready.", len(got), got)
	}
}
