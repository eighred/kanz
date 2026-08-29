package position

// #795'S "VERIFIED WHEN", AGAINST A REAL POSTGRES AND A REAL BROKER.
//
// # The defect
//
// Postgres.Apply takes a per-(tenant, portfolio, instrument) advisory
// TRANSACTION lock, folds, computes the cross-venue aggregate — and the COMMIT
// is what releases that lock. The announcement used to happen after Apply
// returned, which is after the lock was gone. Two fills on one instrument
// landing on the two pods (oms-deploy.yaml runs replicas: 2) therefore
// SERIALIZED THEIR FOLDS and RACED THEIR PUBLISHES.
//
// The retention is what makes it permanent rather than merely late: the POSITION
// stream is provisioned --max-msgs-per-subject=1, so if the pod that committed
// FIRST publishes LAST, the short number is what the subject KEEPS. Nothing
// corrects it — risk/state.ApplyPositionChanged applies unconditionally with no
// as_of guard, and no PortfolioSnapshot publisher exists in any Go service to
// re-baseline it. The rows in `positions` stay correct; only the number every
// consumer reads is wrong, which is why it does not surface as a failure
// anywhere.
//
// # Why this test needs both a database and a broker
//
// The ordering guarantee is a property of THREE things agreeing: the advisory
// lock (Postgres), the outbox's "same partition key publishes in id order"
// (Postgres), and the stream's per-subject compaction (NATS). A fake bus cannot
// show the third, and no unit test can show the first. Both arms below therefore
// read the number back OFF THE STREAM, which is what the risk engine, the
// compliance monitor and tv-sync actually see.
//
// Gated on TEST_POSTGRES_URL and TEST_NATS_URL; test/backing/up.sh stands both
// up with the estate's own 17-stream topology.
//
// EVERY RUN USES A FRESH ENTITY. The subject is compacted and the broker is
// long-lived, so a fixed portfolio/instrument would hand a later run the earlier
// run's retained message — the trap that once made a counting assertion in this
// repository pass on its first run and fail on every one after.

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/pkg/bus"
)

// positionStream is the compacted stream the FACT rides. Named rather than
// discovered so a topology that stopped provisioning it fails this test loudly
// instead of quietly proving nothing.
const positionStream = "POSITION"

// liveBroker dials the bootstrapped broker and returns a producer shaped like
// the OMS's own — NO ProducerConfig.Tenant, so the tenant can only come from the
// record, which is where outbox.From put it.
func liveBroker(t *testing.T) (*bus.Producer, jetstream.JetStream) {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to run the position publish-ordering integration test")
	}
	ctx := context.Background()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "position-order-it"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "it"})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	// A SEPARATE RAW CONNECTION FOR THE READ SIDE, so the assertion is made
	// through the broker's own API rather than through anything this code under
	// test touches.
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return producer, js
}

// retainedQuantity reads what the COMPACTED subject is holding — the number
// every downstream consumer sees, not the number the publisher believed.
func retainedQuantity(t *testing.T, js jetstream.JetStream, subj string) *big.Rat {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := js.Stream(ctx, positionStream)
	if err != nil {
		t.Fatalf("stream %s: %v — the estate topology is not provisioned, so this test would "+
			"prove nothing about what a consumer retains", positionStream, err)
	}
	msg, err := stream.GetLastMsgForSubject(ctx, subj)
	if err != nil {
		t.Fatalf("no retained message on %s: %v", subj, err)
	}
	env, payload, err := bus.Unframe(msg.Data)
	if err != nil {
		t.Fatalf("unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("the retained envelope does not validate: %v", err)
	}
	var st domainpb.PositionState
	if err := proto.Unmarshal(payload, &st); err != nil {
		t.Fatalf("decode PositionState: %v", err)
	}
	return dec.FromProto(st.GetQuantity())
}

// freshEntity returns a portfolio and instrument nothing has published for.
func freshEntity(t *testing.T) (string, string) {
	t.Helper()
	n := time.Now().UnixNano()
	return fmt.Sprintf("pf-795-%d", n), fmt.Sprintf("BTC795%d", n)
}

// THE FIX. Two pods fold the same instrument; the pod that committed SECOND
// flushes FIRST — the interleaving that used to leave the short number on the
// subject. The relay drains the key in id order, which under the fold's advisory
// lock IS commit order, so the last thing on the subject is the last thing
// committed.
func TestTheCompactedSubjectRetainsTheLastCommittedFold(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	producer, js := liveBroker(t)

	portfolio, instrument := freshEntity(t)
	subj := subject.PositionFor(testTenant, portfolio, instrument)
	key := subj
	now := time.Now().UTC()
	ctx := bus.WithTenantID(context.Background(), testTenant)

	podA := NewPostgres(pool, "USD")
	podB := NewPostgres(pool, "USD")
	projA, err := NewProjector(podA, producer, testTenant)
	if err != nil {
		t.Fatalf("projector A: %v", err)
	}
	projB, err := NewProjector(podB, producer, testTenant)
	if err != nil {
		t.Fatalf("projector B: %v", err)
	}

	// COMMIT ORDER: A (aggregate 1), then B (aggregate 3). The advisory lock is
	// what makes that an order at all rather than an interleaving.
	fillA := buy("f795a-"+instrument, instrument, "1", "50000", now)
	fillB := buy("f795b-"+instrument, instrument, "2", "50000", now)

	applyWith(t, ctx, podA, projA, portfolio, key, fillA, now)
	applied := applyWith(t, ctx, podB, projB, portfolio, key, fillB, now)
	if got := dec.FromProto(applied.Aggregate.GetQuantity()); got.Cmp(big.NewRat(3, 1)) != 0 {
		t.Fatalf("pod B committed aggregate %s, want 3 — the fixture is wrong and the ordering "+
			"assertion below would be meaningless", got.RatString())
	}

	// PUBLISH ORDER: the ADVERSARIAL one. B — which committed second — drains
	// first, and A drains after. Under the direct publish this replaces, A's
	// older aggregate would land last and the compacted subject would keep it.
	if _, err := projB.relay.Flush(ctx, key); err != nil {
		t.Fatalf("pod B flush: %v", err)
	}
	if _, err := projA.relay.Flush(ctx, key); err != nil {
		t.Fatalf("pod A flush: %v", err)
	}

	got := retainedQuantity(t, js, subj)
	if got.Cmp(big.NewRat(3, 1)) != 0 {
		t.Fatalf("the compacted subject retains %s, want 3.\n\n"+
			"This is #795: the fund holds 3 and every consumer of this subject — the risk engine, "+
			"the compliance post-trade monitor and tv-sync — reads %s. The rows in `positions` are "+
			"correct, so nothing anywhere reports a problem; the exposure is simply not being "+
			"measured. --max-msgs-per-subject=1 means the short number is KEPT, and nothing "+
			"re-baselines it until the next fill in this instrument.", got.RatString(), got.RatString())
	}
}

// THE DEFECT, REPRODUCED, so the arm above is known to be capable of failing.
//
// It drives the OLD shape deliberately: fold with NO announcer (the fold and its
// announcement decoupled, as they were), then publish DIRECTLY in the order the
// released lock permitted — the pod that committed second publishing first. The
// compacted subject then keeps the LOWER number.
//
// WITHOUT THIS ARM the test above is a green that proves nothing: an ordering
// assertion that has never been seen to fail is indistinguishable from one whose
// harness cannot express the failure.
func TestTheDirectPublishThisReplacesRetainsTheShortNumber(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	producer, js := liveBroker(t)

	portfolio, instrument := freshEntity(t)
	subj := subject.PositionFor(testTenant, portfolio, instrument)
	now := time.Now().UTC()
	ctx := bus.WithTenantID(context.Background(), testTenant)

	podA := NewPostgres(pool, "USD")
	podB := NewPostgres(pool, "USD")

	// Same commit order, same numbers — and nil announcer, so nothing is
	// enqueued and the fold is once again a thing that happens without saying so.
	appliedA, err := podA.Apply(ctx, portfolio, buy("f795c-"+instrument, instrument, "1", "50000", now), now, nil)
	if err != nil {
		t.Fatalf("pod A apply: %v", err)
	}
	appliedB, err := podB.Apply(ctx, portfolio, buy("f795d-"+instrument, instrument, "2", "50000", now), now, nil)
	if err != nil {
		t.Fatalf("pod B apply: %v", err)
	}

	// The publishes race, and A's — older, smaller — wins.
	publishDirect(t, ctx, producer, subj, appliedB.Aggregate)
	publishDirect(t, ctx, producer, subj, appliedA.Aggregate)

	got := retainedQuantity(t, js, subj)
	if got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("the direct publish retained %s, want 1 — this arm exists to REPRODUCE #795, and "+
			"if it cannot, the arm that proves the fix is not known to be capable of failing",
			got.RatString())
	}
	t.Logf("reproduced: the fund holds 3 and the compacted subject retains %s", got.RatString())
}

// applyWith folds one fill through the projector's own announcer, so the records
// under test are exactly the ones Handle would have committed — and returns the
// fold, without flushing. Separating the commit from the flush is the whole
// point: it is what lets the test choose an adversarial publish order.
func applyWith(t *testing.T, ctx context.Context, store *Postgres, p *Projector,
	portfolio, key string, fill *orderpb.Fill, now time.Time) *Applied {
	t.Helper()
	applied, err := store.Apply(ctx, portfolio, fill, now,
		func(ctx context.Context, a *Applied) ([]outbox.Record, error) {
			return p.records(ctx, key, portfolio, fill, a)
		})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return applied
}

// publishDirect is the publish handleFill used to do — straight to the broker,
// outside any transaction and outside the lock that made the number true.
func publishDirect(t *testing.T, ctx context.Context, prod *bus.Producer, subj string, st *domainpb.PositionState) {
	t.Helper()
	if err := prod.Publish(ctx, bus.Event{
		Subject:          subj,
		EventType:        positionEventChanged,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "risk",
		EventTime:        st.GetAsOf().AsTime(),
		PartitionKey:     subj,
		PayloadSchemaRef: "domain.v1.PositionState:1",
		Payload:          st,
	}); err != nil {
		t.Fatalf("direct publish: %v", err)
	}
}
