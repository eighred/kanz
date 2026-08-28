package bustest_test

import (
	"context"
	"os"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// HOW DOES A DENIED SUBSCRIPTION ACTUALLY FAIL? (#788)
//
// test/arch/nats_subscribe_permissions_test.go rests on one claim: that a
// subject missing from a service's tenancy.yaml subscribe allow-list produces a
// pod which authenticates, reports Ready, logs that it subscribed, and then
// RECEIVES NOTHING — silently, forever. That claim is the whole reason the guard
// is worth having, and it was asserted rather than measured.
//
// It is measurable. This drives the real pkg/bus against a real nats-server
// whose config denies one subject, and reports which way it fails.
//
// IT IS GATED AND SKIPS BY DEFAULT, which in this repository is normally a trap —
// so it is deliberately NOT counted as suite evidence. It is a PROBE: run it by
// hand, read the result, and the answer belongs in the comment of whatever rests
// on it (today: test/arch/nats_subscribe_permissions_test.go).
//
// MEASURED 2026-08-28: the connection is ACCEPTED, SubscribeBroadcast does not
// return within 10s, and nothing is delivered. A denied subscription is SILENT —
// so a composition root that treats a Subscribe error as fatal never fires, and
// the pod stays up folding nothing.
//
// TO REPRODUCE, with nats-server on PATH (no Docker needed):
//
//	# perm.conf
//	port: 4232
//	jetstream { store_dir: "/tmp/natsperm/js" }
//	accounts { A: { jetstream: enabled, users: [
//	  { user: admin, password: pw,
//	    permissions: { publish: { allow: [">"] }, subscribe: { allow: [">"] } } },
//	  { user: svc, password: pw,
//	    permissions: { publish:   { allow: ["$JS.API.>", "$JS.ACK.>", ">"] },
//	                   subscribe: { allow: ["allowed.>", "_INBOX.>", "$JS.API.>"] } } },
//	] } }
//
//	nats-server -c perm.conf &
//	nats --server "nats://admin:pw@127.0.0.1:4232" stream add DENIED //	     --subjects "denied.>" --storage file --defaults
//	TEST_NATS_PERM_URL="nats://svc:pw@127.0.0.1:4232" //	     go test ./internal/bustest/ -run TestHowADeniedSubscriptionFails -v
//
// The svc user is allowed to drive JetStream and receive on its own inbox, and
// is denied subscribe on denied.> — exactly the shape a service has when
// tenancy.yaml omits one subject from its subscribe allow-list.
func TestHowADeniedSubscriptionFails(t *testing.T) {
	url := os.Getenv("TEST_NATS_PERM_URL")
	if url == "" {
		t.Skip("set TEST_NATS_PERM_URL to a broker whose user is denied subscribe on denied.>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url})
	if err != nil {
		t.Fatalf("dial: %v — the DENIAL IS AT CONNECT TIME, not at subscribe", err)
	}
	defer func() { _ = client.Close() }()
	t.Log("RESULT: the connection was ACCEPTED — authentication succeeds with a subject denied")

	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}

	got := make(chan struct{}, 1)
	subErr := make(chan error, 1)
	go func() {
		subErr <- consumer.SubscribeBroadcast(ctx, "denied.thing", func(context.Context, *envelopepb.Envelope, []byte) error {
			select {
			case got <- struct{}{}:
			default:
			}
			return nil
		})
	}()

	select {
	case err := <-subErr:
		t.Logf("RESULT: SubscribeBroadcast RETURNED: err=%v", err)
		if err != nil {
			t.Log("=> A DENIAL SURFACES AS AN ERROR. The composition roots treat that as fatal " +
				"(once.Do{firstErr; cancel()}), so the pod EXITS rather than folding nothing. " +
				"The guard's premise — silent, Ready, receiving nothing — is WRONG for this path.")
		}
	case <-time.After(10 * time.Second):
		t.Log("RESULT: SubscribeBroadcast did NOT return within 10s and delivered nothing. " +
			"=> A DENIAL IS SILENT. The pod stays up, reports Ready, and folds nothing forever. " +
			"The guard's premise holds.")
	}

	select {
	case <-got:
		t.Fatal("a message arrived on a subject this user is denied — the fixture is not denying anything")
	default:
	}
}
