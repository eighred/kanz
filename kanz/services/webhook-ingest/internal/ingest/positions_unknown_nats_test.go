package ingest

import (
	"context"
	"errors"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/signal/translate"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type closeSpinePublisher struct {
	producer *bus.Producer
	events   int
}

func (p *closeSpinePublisher) Publish(ctx context.Context, event bus.Event) error {
	p.events++
	return p.producer.Publish(ctx, event)
}

type closeSpineDelivery struct {
	env     *envelopepb.Envelope
	payload []byte
	err     error
}

// This uses the production broadcast path: malformed levels are NAKed, without
// DLQ routing. Compaction plus a fresh subscriber establishes recovery from the
// repaired retained level; no missing quantity may arm as a flat position.
func TestNATSCloseUnknownRetainedRepairAndRetry(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL with canonical POSITION and tenant command routing")
	}
	for name, badQuantity := range map[string]*commonpb.Decimal{"missing": nil, "exponent": {Coefficient: 1, Exponent: 2_000_000_000}} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			id := strings.ReplaceAll(uuid.NewString(), "-", "")
			tenant, fund := "close-"+id, "fund-"+id
			nc, err := nats.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(nc.Close)
			js, err := jetstream.New(nc)
			if err != nil {
				t.Fatal(err)
			}
			positionSubject := subject.VenuePositionFor(tenant, fund, "OKX", "BTC-USD")
			positionStreamName, err := js.StreamNameBySubject(ctx, positionSubject)
			if err != nil {
				t.Fatal(err)
			}
			positionStream, err := js.Stream(ctx, positionStreamName)
			if err != nil {
				t.Fatal(err)
			}
			streamInfo, err := positionStream.Info(ctx)
			if err != nil || positionStreamName != "POSITION" || streamInfo.Config.MaxMsgsPerSubject != 1 {
				t.Fatalf("fixture lacks actual compacted POSITION contract: %+v %v", streamInfo, err)
			}
			client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "close-unknown-" + id})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "close-unknown/test", ProducerVersion: "test", Tenant: tenant})
			if err != nil {
				t.Fatal(err)
			}
			publishPosition := func(venue string, quantity *commonpb.Decimal) {
				t.Helper()
				now := time.Now().UTC()
				state := &domainpb.PositionState{PortfolioId: fund, InstrumentId: "BTC-USD", Venue: venue, Quantity: quantity, AsOf: timestamppb.New(now)}
				if err := producer.Publish(ctx, bus.Event{Subject: subject.VenuePositionFor(tenant, fund, venue, "BTC-USD"), EventType: subject.VenuePositionChanged,
					EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "risk", EventTime: now,
					PartitionKey: fund, PayloadSchemaRef: "domain.v1.PositionState:1", Payload: state}); err != nil {
					t.Fatal(err)
				}
			}
			type ownedConsumer struct{ stream, name string }
			var ownedConsumers []ownedConsumer
			// Cleanup runs in reverse registration order: stop and join subscriptions,
			// delete their consumers, then close the control and bus connections.
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				for _, consumer := range ownedConsumers {
					if err := js.DeleteConsumer(cleanupCtx, consumer.stream, consumer.name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
						t.Errorf("delete test consumer %s/%s: %v", consumer.stream, consumer.name, err)
					}
				}
			})
			findConsumer := func(streamName, filter, group string, since time.Time) jetstream.Consumer {
				t.Helper()
				stream, err := js.Stream(ctx, streamName)
				if err != nil {
					t.Fatal(err)
				}
				listing := stream.ListConsumers(ctx)
				for info := range listing.Info() {
					match := group != "" && strings.HasPrefix(info.Name, group+"-")
					if group == "" {
						match = info.Config.Durable == "" && !info.Created.Before(since)
					}
					if match && info.Config.FilterSubject == filter {
						consumer, err := stream.Consumer(ctx, info.Name)
						if err != nil {
							t.Fatal(err)
						}
						ownedConsumers = append(ownedConsumers, ownedConsumer{streamName, info.Name})
						return consumer
					}
				}
				t.Fatalf("fixture consumer missing: %s %s %s %v", streamName, filter, group, listing.Err())
				return nil
			}
			waitAck := func(consumer jetstream.Consumer) {
				t.Helper()
				for {
					info, err := consumer.Info(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if info.NumPending == 0 && info.NumAckPending == 0 {
						return
					}
					select {
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal("source/output was not acknowledged")
					}
				}
			}
			startPositions := func(cache *PositionCache) (<-chan closeSpineDelivery, <-chan struct{}, func(), time.Time) {
				t.Helper()
				consumer, err := bus.NewConsumer(client)
				if err != nil {
					t.Fatal(err)
				}
				subCtx, stop := context.WithCancel(ctx)
				seen, ready, done := make(chan closeSpineDelivery, 32), make(chan struct{}), make(chan error, 1)
				since := time.Now().UTC()
				go func() {
					done <- consumer.SubscribeBroadcastReady(subCtx, subject.VenuePositionAll, func(c context.Context, env *envelopepb.Envelope, payload []byte) error {
						err := cache.Handle(c, env, payload)
						var state domainpb.PositionState
						if proto.Unmarshal(payload, &state) == nil && state.PortfolioId == fund {
							select {
							case seen <- closeSpineDelivery{env: proto.Clone(env).(*envelopepb.Envelope), payload: append([]byte(nil), payload...), err: err}:
							case <-subCtx.Done():
							}
						}
						return err
					}, func() { cache.Arm(); close(ready) })
				}()
				var once sync.Once
				join := func() {
					once.Do(func() {
						stop()
						select {
						case err := <-done:
							if err != nil && !errors.Is(err, context.Canceled) {
								t.Error(err)
							}
						case <-time.After(5 * time.Second):
							t.Error("position broadcast did not stop")
						}
					})
				}
				t.Cleanup(join)
				return seen, ready, join, since
			}
			waitReady := func(ready <-chan struct{}) {
				t.Helper()
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("repaired position replay never armed")
				}
			}
			nextBad := func(seen <-chan closeSpineDelivery) closeSpineDelivery {
				t.Helper()
				for {
					select {
					case d := <-seen:
						if d.err != nil {
							if bus.Validate(d.env) != nil {
								t.Fatal("malformed fixture envelope")
							}
							return d
						}
					case <-ctx.Done():
						t.Fatal("invalid canonical position never reached production handler")
					}
				}
			}
			publishPosition("BINANCE", &commonpb.Decimal{Coefficient: 6, Exponent: -1})
			publishPosition("OKX", &commonpb.Decimal{Coefficient: -4, Exponent: -1})
			live := NewPositionCache()
			seen, ready, stop, liveSince := startPositions(live)
			waitReady(ready)
			waitAck(findConsumer(positionStreamName, subject.VenuePositionAll, "", liveSince))
			nonces := NewMemoryNonces(time.Minute, 1000)
			pub := &closeSpinePublisher{producer: producer}
			authority, err := translate.NewFundAuthority(map[string]string{fund: tenant}, map[string][]string{"momentum": {fund}})
			if err != nil {
				t.Fatal(err)
			}
			pipeline := func(cache *PositionCache) *Pipeline {
				t.Helper()
				p, err := NewPipeline(Options{Auth: NewAuthenticator(StaticSecrets{"momentum": testSecret}, nil, time.Minute, time.Now, WithNonceStore(nonces)),
					Symbols: StaticSymbols{"BINANCE:BTCUSDT": "BTC-USD"}, Prices: StaticPrices{}, Equity: StaticEquity{}, Positions: cache,
					Alloc:     StaticAllocation{fund: {{Venue: "BINANCE", Weight: big.NewRat(3, 5)}, {Venue: "OKX", Weight: big.NewRat(2, 5)}}},
					Authority: authority, Publisher: pub, Gate: halt.OpenGate(nil)})
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			raw := strings.ReplaceAll(body("close", "1", "absolute_qty", "same-close"), "fund-alpha", fund)
			process := func(p *Pipeline, raw string) (*Result, error) {
				return p.Process(ctx, []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret))
			}
			publishPosition("OKX", badQuantity)
			nextBad(seen)
			if q, err := live.Position(ctx, fund, "OKX", "BTC-USD"); err == nil || q != nil {
				t.Fatalf("invalid live level retained old size: %v %v", q, err)
			}
			if _, err := process(pipeline(live), raw); err == nil || decided(err) || errors.Is(err, ErrReplayed) || pub.events != 0 {
				t.Fatalf("live unknown close had effects/burned nonce: %v events=%d", err, pub.events)
			}
			stop()
			cold := NewPositionCache()
			coldSeen, coldReady, stopCold, since := startPositions(cold)
			first, redelivery := nextBad(coldSeen), nextBad(coldSeen)
			if first.env.GetEventId() != redelivery.env.GetEventId() || !proto.Equal(first.env, redelivery.env) || string(first.payload) != string(redelivery.payload) {
				t.Fatal("broadcast did not NAK and redeliver original invalid bytes")
			}
			select {
			case <-coldReady:
				t.Fatal("unreadable retained holding armed as healthy/flat")
			default:
			}
			if cold.Armed() {
				t.Fatal("cold unreadable book armed")
			}
			blocked := findConsumer(positionStreamName, subject.VenuePositionAll, "", since)
			blockedInfo, err := blocked.Info(ctx)
			if err != nil || blockedInfo.NumAckPending == 0 {
				t.Fatalf("unreadable broadcast was acknowledged: %+v %v", blockedInfo, err)
			}
			if _, err := process(pipeline(cold), raw); err == nil || decided(err) || errors.Is(err, ErrReplayed) || pub.events != 0 {
				t.Fatalf("cold close had effects/burned nonce: %v events=%d", err, pub.events)
			}
			stopCold()
			publishPosition("OKX", &commonpb.Decimal{Coefficient: -4, Exponent: -1})
			retained, err := positionStream.GetLastMsgForSubject(ctx, positionSubject)
			if err != nil {
				t.Fatal(err)
			}
			_, retainedPayload, err := bus.Unframe(retained.Data)
			var repaired domainpb.PositionState
			if err != nil || proto.Unmarshal(retainedPayload, &repaired) != nil || repaired.Quantity == nil || dec.FromProto(repaired.Quantity).Cmp(big.NewRat(-2, 5)) != 0 {
				t.Fatal("compaction did not retain valid repair")
			}
			restarted := NewPositionCache()
			_, repairedReady, stopRepaired, repairedSince := startPositions(restarted)
			waitReady(repairedReady)
			waitAck(findConsumer(positionStreamName, subject.VenuePositionAll, "", repairedSince))
			// Real generated command/FACT consumers validate canonical envelopes and ACK
			// the existing routes. No venue or order execution is invoked by this fixture.
			outputs := map[string]chan closeSpineDelivery{}
			outputConsumers := []jetstream.Consumer{}
			for _, topic := range []string{translate.SubjectSignal, translate.SubjectSubmit} {
				ch := make(chan closeSpineDelivery, 8)
				outputs[topic] = ch
				consumer, err := bus.NewConsumer(client)
				if err != nil {
					t.Fatal(err)
				}
				group := "close-output-" + id + strings.ReplaceAll(topic, ".", "-")
				outputCtx, stopOutput := context.WithCancel(ctx)
				done, ready := make(chan error, 1), make(chan struct{})
				go func() {
					done <- consumer.SubscribeReady(outputCtx, topic, group, func(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
						if env.GetTenantId() == tenant {
							select {
							case ch <- closeSpineDelivery{env: proto.Clone(env).(*envelopepb.Envelope), payload: append([]byte(nil), payload...)}:
							case <-outputCtx.Done():
								return outputCtx.Err()
							}
						}
						return nil
					}, func() { close(ready) })
				}()
				t.Cleanup(func() {
					stopOutput()
					select {
					case err := <-done:
						if err != nil && !errors.Is(err, context.Canceled) {
							t.Error(err)
						}
					case <-time.After(5 * time.Second):
						t.Error("output consumer did not stop")
					}
				})
				waitReady(ready)
				streamName, err := js.StreamNameBySubject(ctx, topic)
				if err != nil {
					t.Fatal(err)
				}
				outputConsumers = append(outputConsumers, findConsumer(streamName, topic, group, time.Time{}))
			}
			readOutput := func(topic string) closeSpineDelivery {
				t.Helper()
				select {
				case d := <-outputs[topic]:
					if bus.Validate(d.env) != nil {
						t.Fatal("invalid emitted canonical envelope")
					}
					return d
				case <-ctx.Done():
					t.Fatal("expected canonical output never reached consumer")
				}
				return closeSpineDelivery{}
			}
			assertClose := func(cache *PositionCache, raw, nonce string, venues int) {
				t.Helper()
				result, err := process(pipeline(cache), raw)
				if err != nil || len(result.OrderIDs) != venues {
					t.Fatalf("repair did not permit same alert: %+v %v", result, err)
				}
				fact := readOutput(translate.SubjectSignal)
				var signal signalpb.StrategySignal
				if err := proto.Unmarshal(fact.payload, &signal); err != nil || signal.SignalId != translate.DeterministicID("momentum", nonce) || signal.FundId != fund || fact.env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
					t.Fatalf("signal lineage changed: %v %v", &signal, err)
				}
				seenVenues := map[string]bool{}
				for range venues {
					d := readOutput(translate.SubjectSubmit)
					var cmd orderpb.SubmitOrder
					if err := proto.Unmarshal(d.payload, &cmd); err != nil {
						t.Fatal(err)
					}
					want, side := big.NewRat(3, 5), orderpb.Side_SIDE_SELL
					if cmd.Venue == "OKX" {
						want, side = big.NewRat(2, 5), orderpb.Side_SIDE_BUY
					}
					if seenVenues[cmd.Venue] || (cmd.Venue != "BINANCE" && cmd.Venue != "OKX") || (venues == 1 && cmd.Venue != "BINANCE") || cmd.Side != side || cmd.PortfolioId != fund || dec.FromProto(cmd.Quantity).Cmp(want) != 0 || cmd.OrderId != translate.DeterministicID(signal.SignalId, cmd.Venue) || cmd.GetMetadata().GetIssuer() != "strategy:momentum" || d.env.GetIdempotencyKey() != cmd.OrderId || d.env.GetCorrelationId() != signal.SignalId {
						t.Fatalf("resolved CLOSE changed on actual command route: %v env=%v", &cmd, d.env)
					}
					seenVenues[cmd.Venue] = true
				}
				for _, consumer := range outputConsumers {
					waitAck(consumer)
				}
				before := pub.events
				if _, err := process(pipeline(cache), raw); !errors.Is(err, ErrReplayed) || pub.events != before {
					t.Fatalf("successful alert nonce was not committed: %v", err)
				}
			}
			assertClose(restarted, raw, "same-close", 2)
			if pub.events != 3 {
				t.Fatalf("unknown retry produced partial/duplicate publications: %d", pub.events)
			}
			stopRepaired()
			publishPosition("OKX", &commonpb.Decimal{})
			flat := NewPositionCache()
			_, flatReady, _, flatSince := startPositions(flat)
			waitReady(flatReady)
			waitAck(findConsumer(positionStreamName, subject.VenuePositionAll, "", flatSince))
			flatRaw := strings.ReplaceAll(body("close", "1", "absolute_qty", "flat-close"), "fund-alpha", fund)
			assertClose(flat, flatRaw, "flat-close", 1)
			if pub.events != 5 {
				t.Fatalf("explicit zero recovery effects=%d want5", pub.events)
			}
		})
	}
}
