package orders

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/bus"
)

// AGAINST A REAL BROKER, BECAUSE THE FAKE CANNOT DISAGREE WITH ONE (#723).
//
// fakePub records the last Event and returns nil. It has no dedup window, no
// Nats-Msg-Id and no envelope validation — AGENTS.md's standing warning that "a
// green suite using it is not a broker proof". The unit tests in the sibling
// file can therefore prove that two attempts carry ONE key, but not that one key
// yields ONE ORDER: that happens inside JetStream, keyed on a header this
// package never reads back.
//
// So this asserts the thing that actually protects capital — after a client
// retries an ambiguous timeout, exactly one SubmitOrder is durable — and then
// that a submit which cannot be made at-most-once becomes durable not at all.
//
// It also pins the fix's REASON rather than its shape. Each attempt below mints
// a DIFFERENT order id; minting is untouched, deliberately. The broker still
// collapses them, because what it dedups on is the client's key. That is exactly
// why a submit with no key must not be minted: there would be nothing for the
// collapse to key on, and the retry would survive as a second live order.
//
// # IT PUBLISHES INTO THE PROVISIONED TOPOLOGY, AND CREATES NO STREAM OF ITS OWN
//
// An earlier draft created a stream bound to the gateway's subject. That works
// on a bare `nats-server -js` and fails on every broker this suite really runs
// against: infra/nats/bootstrap-job.yaml binds `tenant.*.order.>` to
// TENANT_ORDER and `order.>` to EXECUTION, so the create returns
// "subjects overlap with an existing stream" (verified: err_code=10065).
//
// Using the provisioned streams is also the stronger proof. Their 2m dedup
// window is the one ensure_stream configures for production (--dupe-window=2m),
// rather than one this test chose for itself — so what is measured here is the
// estate's real collapse behaviour.
//
// Requires a bootstrapped broker, which is already this repository's contract
// for TEST_NATS_URL — test/arch's TestEveryBootstrappedStreamIsFileBackedOnThe-
// Broker fails on a broker without the topology. Stand one up with
// test/backing/up.sh.
func TestSubmit_RetriedIntentIsOneOrderOnARealBroker(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}

	ctx := context.Background()
	// A UNIQUE TENANT PER RUN. These streams retain for 24h and are shared with
	// every other test that publishes an order, so a test that COUNTS messages
	// on them passes once and then fails forever with the count climbing —
	// a failure that reads as a broken fix rather than a dirty broker.
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	tenant := "t" + uniq
	intentKey := "intent-" + uniq

	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	// WHERE THE COMMAND LANDS IS THE BROKER'S DECISION, not this test's, and the
	// two brokers the suite runs against decide differently. A bootstrapped
	// broker carries the gateway's tenant-routed subject on TENANT_ORDER; CI's
	// test/backing/nats-dev.conf remaps `tenant.*.order.order.submit` to
	// `order.order.submit` at INGRESS (that single-account broker has no tenant
	// accounts to import the prefix), so the same publish lands on EXECUTION.
	// Watching only one of them is how a test passes on a laptop and proves
	// nothing in CI.
	streams := carryingStreams(t, ctx, js,
		bus.TenantRoutedSubject(tenant, subjectSubmit), subjectSubmit)
	before := sequences(t, ctx, js, streams)

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "gw-intent-test"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// The real Producer: it stamps and VALIDATES the envelope, so a COMMAND with
	// no idempotency_key or no tenant is refused here rather than accepted the
	// way fakePub accepts it.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "api-gateway",
		ProducerVersion: "test",
	})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	h := New(producer, "", halt.OpenGate(nil))
	mux := testMux()
	h.Routes(mux)

	post := func(t *testing.T, idemKey string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(submitBodyNoID))
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		req = authed(req, "alice", tenant)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}

	// ── One intent, two attempts: the client timed out and retried, with no way
	// to know whether the first attempt reached the platform.
	first := post(t, intentKey)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first attempt = %d, want 202 (body: %s)", first.Code, first.Body.String())
	}
	retry := post(t, intentKey)
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry = %d, want 202 (body: %s)", retry.Code, retry.Body.String())
	}
	// The two attempts really did mint different order ids. Without this, the
	// assertion below could be passing because nothing varied.
	if first.Body.String() == retry.Body.String() {
		t.Fatalf("both attempts returned the same order id, so nothing was collapsed by the "+
			"BROKER and this test proves nothing: %s", first.Body.String())
	}

	stored := newFramesForTenant(t, ctx, js, streams, before, tenant)
	if len(stored) != 1 {
		t.Fatalf("%d SubmitOrder COMMANDs are durable for ONE client intent, want 1 — the retry "+
			"was not collapsed, and internal/execution stamps order_id into the venue clOrdId, "+
			"so that is %d live orders at the exchange (#723)", len(stored), len(stored))
	}

	// ── The refusal, against the same real producer: nothing is published.
	refused := post(t, "")
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("submit with neither order_id nor Idempotency-Key = %d, want 400 (body: %s)",
			refused.Code, refused.Body.String())
	}
	// Re-counted from the SAME baseline. A refused submit that published anyway
	// would carry a freshly minted id as its key, so this must not filter on
	// intentKey — that is precisely how it would go unnoticed.
	if after := newFramesForTenant(t, ctx, js, streams, before, tenant); len(after) != 1 {
		t.Fatalf("the refused submit became durable anyway: %d commands for this tenant, want 1",
			len(after))
	}

	assertStoredCommand(t, stored[0], tenant, intentKey)
}

// carryingStreams resolves which streams carry the given subjects, deduped. It
// FAILS rather than skips when none do: a broker with no topology makes every
// assertion below vacuous, and "0 messages found" would read as a passing
// at-most-once proof.
func carryingStreams(t *testing.T, ctx context.Context, js jetstream.JetStream, subjects ...string) []string {
	t.Helper()

	seen := map[string]bool{}
	var names []string
	for _, subject := range subjects {
		name, err := js.StreamNameBySubject(ctx, subject)
		if err != nil || name == "" {
			continue // this broker does not route that subject; the other may
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatalf("no stream on this broker carries %v, so the gateway's publish would fail "+
			"outright and this test would prove nothing. Provision the topology with "+
			"test/backing/up.sh (it runs infra/nats/bootstrap-job.yaml's own script).", subjects)
	}
	return names
}

// sequences snapshots each stream's LastSeq, so the scan afterwards reads only
// what this test caused. These streams retain 24h of the whole suite's traffic;
// scanning them from the beginning would be both slow and wrong.
func sequences(t *testing.T, ctx context.Context, js jetstream.JetStream, streams []string) map[string]uint64 {
	t.Helper()

	at := make(map[string]uint64, len(streams))
	for _, name := range streams {
		s, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatalf("stream %s: %v", name, err)
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatalf("stream %s info: %v", name, err)
		}
		at[name] = info.State.LastSeq
	}
	return at
}

// newFramesForTenant returns every command stored since the baseline whose
// envelope names this tenant.
//
// FILTERED ON THE ENVELOPE'S TENANT, not on the subject: CI's mapping strips the
// tenant prefix before the message is stored, so the subject cannot identify
// these and the shared logical subject carries other tests' orders too. The
// tenant is unique to this run, and it is a field the broker does not rewrite.
func newFramesForTenant(t *testing.T, ctx context.Context, js jetstream.JetStream, streams []string, since map[string]uint64, tenant string) []*envelopepb.EventFrame {
	t.Helper()

	var found []*envelopepb.EventFrame
	for _, name := range streams {
		s, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatalf("stream %s: %v", name, err)
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatalf("stream %s info: %v", name, err)
		}
		for seq := since[name] + 1; seq <= info.State.LastSeq; seq++ {
			raw, err := s.GetMsg(ctx, seq)
			if err != nil {
				continue // a gap is an unrelated message aged out, not a failure
			}
			var frame envelopepb.EventFrame
			if err := proto.Unmarshal(raw.Data, &frame); err != nil {
				continue // another producer's payload shape; not this test's
			}
			env := frame.GetEnvelope()
			if env.GetTenantId() != tenant || env.GetEventType() != subjectSubmit {
				continue
			}
			if got := raw.Header.Get("Nats-Msg-Id"); got != env.GetIdempotencyKey() {
				t.Errorf("%s seq %d: Nats-Msg-Id = %q but envelope idempotency_key = %q — this "+
					"header IS the broker's dedup key, and a second spelling of it fails "+
					"silently: the publish succeeds and simply stops being deduplicated",
					name, seq, got, env.GetIdempotencyKey())
			}
			found = append(found, &frame)
		}
	}
	return found
}

// assertStoredCommand reads the surviving command back. Not ceremony: the count
// above is satisfied by any single message, and the fields below are the ones a
// broker mapping or a producer fallback would quietly change.
func assertStoredCommand(t *testing.T, frame *envelopepb.EventFrame, tenant, wantKey string) {
	t.Helper()

	env := frame.GetEnvelope()
	if env.GetTenantId() != tenant {
		t.Errorf("envelope tenant_id = %q, want %q — the subject prefix can be remapped away by "+
			"the broker, so the envelope is where the tenant has to be asserted",
			env.GetTenantId(), tenant)
	}
	if env.GetIdempotencyKey() != wantKey {
		t.Errorf("envelope idempotency_key = %q, want the client's key %q — the OMS consumer "+
			"claims on this value, so a minted id here would defeat the second dedup layer even "+
			"where the broker collapsed the first", env.GetIdempotencyKey(), wantKey)
	}

	var cmd orderpb.SubmitOrder
	if err := proto.Unmarshal(frame.GetPayload(), &cmd); err != nil {
		t.Fatalf("unmarshal SubmitOrder: %v", err)
	}
	if cmd.GetOrderId() == "" {
		t.Error("the stored command carries no order_id")
	}
	if got := cmd.GetMetadata().GetIssuer(); got != "user:alice" {
		t.Errorf("issuer = %q, want user:alice", got)
	}
}
