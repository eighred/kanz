package position

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/subject"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// A bounded burst enters on the canonical fill subject. Observe both live
// position subjects before compaction, then read the durable book independently.
// Hold one acknowledgement across projector reconstruction to force redelivery.
func TestBrokerPositionFoldContentionProfile(t *testing.T) {
	for _, keys := range []int{1, 4} {
		t.Run(fmt.Sprintf("%d_keys", keys), func(t *testing.T) { profileBrokerFold(t, keys) })
	}
}

type submittedFill struct {
	start      time.Time
	instrument string
}
type positionDelivery struct {
	msg *nats.Msg
	at  time.Time
}

func profileBrokerFold(t *testing.T, keys int) {
	const workers, fills = 4, 48
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	producer, js := liveBroker(t)
	tenant := fmt.Sprintf("capacity-%d", time.Now().UnixNano())
	const portfolio = "fund"
	ctx = bus.WithTenantID(ctx, tenant)
	pool := newTracedPool(t, tenant, foldTimings{})
	var privileged bool
	if err := pool.QueryRow(ctx, "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&privileged); err != nil || privileged {
		t.Fatalf("restricted PostgreSQL role required: privileged=%t err=%v", privileged, err)
	}
	freshSchema(t, pool)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: os.Getenv("TEST_NATS_URL"), Name: "position-capacity"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tuning := client.DurableConsumerTuning(orderEventFilled)
	stream, err := js.StreamNameBySubject(ctx, orderEventFilled)
	if err != nil {
		t.Fatal(err)
	}
	input, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable: tenant, FilterSubject: orderEventFilled, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: tuning.AckWait, MaxAckPending: tuning.MaxAckPending, MaxDeliver: tuning.MaxDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := js.DeleteConsumer(context.Background(), stream, tenant); err != nil {
			t.Error(err)
		}
	})
	position, err := js.Stream(ctx, positionStream)
	if err != nil {
		t.Fatal(err)
	}
	observations := make(chan positionDelivery, 2*(fills+1)+1)
	for key := range keys {
		inst := fmt.Sprintf("ASSET-%d", key)
		for _, subj := range []string{subject.PositionFor(tenant, portfolio, inst), subject.VenuePositionFor(tenant, portfolio, "XBIN", inst)} {
			sub, err := js.Conn().Subscribe(subj, func(msg *nats.Msg) {
				select {
				case observations <- positionDelivery{msg, time.Now()}:
				case <-ctx.Done():
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := sub.Unsubscribe(); err != nil {
					t.Error(err)
				}
				if err := position.Purge(context.Background(), jetstream.WithPurgeSubject(subj)); err != nil {
					t.Error(err)
				}
			})
		}
	}
	if err := js.Conn().FlushWithContext(ctx); err != nil {
		t.Fatal(err)
	}

	inputs := make(map[string]submittedFill)
	var retryID string
	begin := time.Now()
	for i := range fills {
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatal(err)
		}
		inst := fmt.Sprintf("ASSET-%d", i%keys)
		at := time.Now()
		f := buy(id.String(), inst, "1", "100", at)
		f.OrderId = "order-" + f.FillId
		fact := &orderpb.OrderFilled{OrderId: f.OrderId, Fill: f, State: &orderpb.OrderState{OrderId: f.OrderId, PortfolioId: portfolio, InstrumentId: inst, Venue: f.Venue, Status: orderpb.OrderStatus_ORDER_STATUS_FILLED}}
		inputs[id.String()] = submittedFill{at, inst}
		if i == 0 {
			retryID = id.String()
		}
		if err := producer.Publish(ctx, bus.Event{EventID: id.String(), Subject: orderEventFilled, EventType: orderEventFilled, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "order", SchemaVersion: 1, PartitionKey: f.OrderId, EventTime: at, Payload: fact}); err != nil {
			t.Fatal(err)
		}
	}
	backlog, err := input.Info(ctx)
	if err != nil || backlog.NumPending != fills {
		t.Fatalf("initial backlog=%v err=%v", backlog, err)
	}
	newProjector := func() *Projector {
		p, err := NewProjector(profileStore{NewPostgres(pool, "USD")}, producer, tenant)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	projector := newProjector()
	type handled struct {
		id     string
		msg    jetstream.Msg
		sample foldSample
		err    error
	}
	var locks, transactions []time.Duration
	var withheld jetstream.Msg
	for offset := 0; offset < fills; offset += workers {
		results := make(chan handled, workers)
		var messages [workers]jetstream.Msg
		for i := range messages {
			msg, err := input.Next(jetstream.FetchMaxWait(3 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			messages[i] = msg
		}
		for _, msg := range messages {
			go func() {
				var sample foldSample
				ctx := context.WithValue(ctx, sampleKey{}, &sample)
				id, err := handleProfileFill(ctx, projector, msg, 1, inputs)
				if err == nil && id != retryID {
					err = msg.DoubleAck(ctx)
				}
				results <- handled{id, msg, sample, err}
			}()
		}
		for range workers {
			h := <-results
			if h.err != nil {
				t.Error(h.err)
			}
			if h.sample.lock <= 0 || h.sample.transaction < h.sample.lock {
				t.Errorf("incomplete fold timing: %+v", h.sample)
			}
			locks = append(locks, h.sample.lock)
			transactions = append(transactions, h.sample.transaction)
			if h.id == retryID {
				withheld = h.msg
			}
		}
		if t.Failed() {
			t.FailNow()
		}
	}
	beforeRetry, err := input.Info(ctx)
	if err != nil || withheld == nil || beforeRetry.NumPending != 0 || beforeRetry.NumAckPending != 1 {
		t.Fatalf("before retry: %v err=%v", beforeRetry, err)
	}
	pool.Close()
	pool = newTracedPool(t, tenant, foldTimings{})
	nakAt := time.Now()
	if err := withheld.Nak(); err != nil {
		t.Fatal(err)
	}
	retry, err := input.Next(jetstream.FetchMaxWait(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	retryWait := time.Since(nakAt)
	id, err := handleProfileFill(ctx, newProjector(), retry, 2, inputs)
	if err != nil || id != retryID {
		t.Fatalf("redelivery %q err=%v", id, err)
	}
	if err := retry.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
	duration := time.Since(begin)
	retryToAck := time.Since(nakAt)
	end, err := input.Info(ctx)
	if err != nil || end.NumPending != 0 || end.NumAckPending != 0 || end.AckFloor.Consumer != fills+1 {
		t.Fatalf("final consumer: %v err=%v", end, err)
	}

	// Flush/Barrier fences the observer callbacks before we count output. A
	// repeated legitimate input produces a fresh absolute reannouncement.
	if err := js.Conn().FlushWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	if err := js.Conn().Barrier(func() { close(drained) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if len(observations) != 2*(fills+1) {
		t.Fatalf("got %d position FACTs, want %d", len(observations), 2*(fills+1))
	}
	latency := verifyProfileOutputs(t, observations, inputs, tenant, portfolio, retryID, keys)
	var claims, undelivered int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM position_fills), (SELECT count(*) FROM outbox WHERE published_at IS NULL)").Scan(&claims, &undelivered); err != nil || claims != fills || undelivered != 0 {
		t.Fatalf("claims=%d undelivered=%d err=%v", claims, undelivered, err)
	}
	book, err := NewPostgres(pool, "USD").Snapshot(ctx, portfolio, time.Now())
	if err != nil || len(book.GetPositions()) != keys {
		t.Fatalf("durable positions=%d err=%v", len(book.GetPositions()), err)
	}
	for _, holding := range book.Positions {
		if dec.FromProto(holding.Quantity).Cmp(big.NewRat(int64(fills/keys), 1)) != 0 || dec.FromProto(holding.AveragePrice).Cmp(big.NewRat(100, 1)) != 0 {
			t.Fatalf("unexpected durable holding %v", holding)
		}
	}
	t.Logf("workers=%d keys=%d fills=%d redeliveries=1 FACTs=%d backlog=%d final_pending=%d ack_floor=%d AckWait=%s MaxAckPending=%d MaxDeliver=%d fills/s=%.1f", workers, keys, claims, 2*(fills+1), backlog.NumPending, end.NumPending, end.AckFloor.Consumer, end.Config.AckWait, end.Config.MaxAckPending, end.Config.MaxDeliver, float64(fills)/duration.Seconds())
	t.Logf("explicit_NAK_to_redelivery=%s explicit_NAK_to_confirmed_ACK=%s", retryWait, retryToAck)
	for _, series := range []struct {
		name   string
		values []time.Duration
	}{{"submit_to_aggregate_FACT", latency}, {"advisory_lock_query", locks}, {"apply_transaction", transactions}} {
		if len(series.values) != fills {
			t.Fatalf("%s has %d samples, want %d", series.name, len(series.values), fills)
		}
		slices.Sort(series.values)
		q := func(percent int) time.Duration { return series.values[(len(series.values)*percent+99)/100-1] }
		t.Logf("%s n=%d p50=%v p95=%v p99=%v max=%v", series.name, len(series.values), q(50), q(95), q(99), series.values[len(series.values)-1])
	}
}

type profileStore struct{ Store }

func (s profileStore) Apply(ctx context.Context, portfolio string, fill *orderpb.Fill, at time.Time, announce Announcer) (*Applied, error) {
	start := time.Now()
	result, err := s.Store.Apply(ctx, portfolio, fill, at, announce)
	if sample, _ := ctx.Value(sampleKey{}).(*foldSample); sample != nil {
		sample.transaction = time.Since(start)
	}
	return result, err
}

func verifyProfileOutputs(t *testing.T, observations <-chan positionDelivery, inputs map[string]submittedFill, tenant, portfolio, retryID string, keys int) []time.Duration {
	t.Helper()
	fills := len(inputs)
	bySubject := map[string]int{}
	byCause := map[[2]string]int{}
	var latency []time.Duration
	for len(observations) > 0 {
		obs := <-observations
		env, payload, err := bus.Unframe(obs.msg.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		in, ok := inputs[env.CausationId]
		if !ok || env.TenantId != tenant || env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
			t.Fatalf("unexpected position lineage %s", env.CausationId)
		}
		var state domainpb.PositionState
		if err := proto.Unmarshal(payload, &state); err != nil {
			t.Fatal(err)
		}
		subj := obs.msg.Subject
		bySubject[subj]++
		count := min(bySubject[subj], fills/keys)
		if state.PortfolioId != portfolio || state.InstrumentId != in.instrument || dec.FromProto(state.GetQuantity()).Cmp(big.NewRat(int64(count), 1)) != 0 {
			t.Fatalf("subject %s observation %d state %v", subj, bySubject[subj], &state)
		}
		byCause[[2]string{env.CausationId, subj}]++
		if subj == subject.PositionFor(tenant, portfolio, in.instrument) && byCause[[2]string{env.CausationId, subj}] == 1 {
			latency = append(latency, obs.at.Sub(in.start))
		}
	}
	for id, in := range inputs {
		want := 1
		if id == retryID {
			want++
		}
		for _, subj := range []string{subject.PositionFor(tenant, portfolio, in.instrument), subject.VenuePositionFor(tenant, portfolio, "XBIN", in.instrument)} {
			if got := byCause[[2]string{id, subj}]; got != want {
				t.Fatalf("input %s subject %s: got %d want %d", id, subj, got, want)
			}
		}
	}
	return latency
}

func handleProfileFill(ctx context.Context, p *Projector, msg jetstream.Msg, delivery uint64, inputs map[string]submittedFill) (string, error) {
	meta, err := msg.Metadata()
	if err != nil {
		return "", err
	}
	if meta.NumDelivered != delivery {
		return "", fmt.Errorf("delivery count %d, want %d", meta.NumDelivered, delivery)
	}
	env, payload, err := bus.Unframe(msg.Data())
	if err != nil {
		return "", err
	}
	if err := bus.Validate(env); err != nil {
		return "", err
	}
	if _, ok := inputs[env.EventId]; !ok {
		return "", fmt.Errorf("unknown input %s", env.EventId)
	}
	ctx = bus.WithCausationID(bus.WithCorrelationID(ctx, env.CorrelationId), env.EventId)
	return env.EventId, p.Handle(ctx, env, payload)
}
