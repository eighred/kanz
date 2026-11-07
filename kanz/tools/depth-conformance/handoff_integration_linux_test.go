//go:build linux

package main

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

// Controlled stop/join/start is a rehearsal controller, not a distributed
// publisher fence. No live venue, production identity or deployment is used.
func TestFeedRuntimeControlledHandoff(t *testing.T) {
	goBinary := handoffBinary(t, "KANZ_MARKET_INGEST_BIN")
	binance := handoffBinary(t, "KANZ_BINANCE_DEPTH_BIN")
	okx := handoffBinary(t, "KANZ_OKX_DEPTH_BIN")
	version := handoffVersion(t)
	for _, venue := range []string{"binance", "okx"} {
		t.Run(venue, func(t *testing.T) {
			rustBinary := binance
			if venue == "okx" {
				rustBinary = okx
			}
			rustID := "spiffe://kanz.internal/ns/kanz-services/sa/" + venue + "-depth"
			broker := startHandoffBroker(t, rustID, false)
			exchange := startHandoffExchange(t, venue)
			instrument := "BTC-USD-HANDOFF"
			symbol := "BTCUSDT"
			if venue == "okx" {
				symbol = "BTC-USDT"
			}
			mic := strings.ToUpper(venue)
			subs := make(map[string]*nats.Subscription)
			for _, subject := range []string{"market.book.snapshot", "market.crypto.quote", "market.crypto.bar"} {
				consumer := "handoff-" + strings.ReplaceAll(subject, ".", "-")
				if _, err := broker.js.AddConsumer("HANDOFF", &nats.ConsumerConfig{Durable: consumer, FilterSubject: subject, DeliverPolicy: nats.DeliverAllPolicy, AckPolicy: nats.AckExplicitPolicy}); err != nil {
					t.Fatal("owned durable observer creation failed")
				}
				sub, err := broker.js.PullSubscribe(subject, consumer, nats.Bind("HANDOFF", consumer))
				if err != nil {
					t.Fatal("observer subscription failed")
				}
				subs[subject] = sub
				t.Cleanup(func() {
					if err := sub.Unsubscribe(); err != nil {
						t.Error("observer local subscription close failed")
					}
					if err := broker.js.DeleteConsumer("HANDOFF", consumer); err != nil {
						t.Error("owned observer deletion failed")
						return
					}
					if _, err := broker.js.ConsumerInfo("HANDOFF", consumer); !errors.Is(err, nats.ErrConsumerNotFound) {
						t.Error("owned observer absence unconfirmed")
					}
				})
			}
			goEnv := func(source string, external bool) []string {
				env := []string{"MARKET_INGEST_LISTEN=" + handoffAddress(t), "MARKET_INGEST_NATS_URL=" + broker.url, "MARKET_INGEST_SOURCE=" + source, "MARKET_INGEST_TENANT=handoff-tenant", "MARKET_INGEST_INSTRUMENTS=" + instrument, "MARKET_INGEST_" + mic + "_SYMBOLS=" + instrument + "=" + symbol}
				ws := strings.Replace(exchange.server.URL, "http://", "ws://", 1)
				if venue == "binance" {
					env = append(env, "MARKET_INGEST_BINANCE_WS_BASE="+ws, "MARKET_INGEST_BINANCE_REST_BASE="+exchange.server.URL)
				} else {
					env = append(env, "MARKET_INGEST_OKX_WS_URL="+ws+"/ws/v5/public")
				}
				if external {
					env = append(env, "MARKET_INGEST_"+mic+"_EXTERNAL_DEPTH_INSTRUMENTS="+instrument)
				}
				return env
			}
			rustAddress := handoffAddress(t)
			rustPrefix := mic + "_DEPTH_"
			rustSource := venue + "-depth/handoff"
			rustEnv := []string{rustPrefix + "DEVELOPMENT=true", rustPrefix + "INSTRUMENT_ID=" + instrument, rustPrefix + "SYMBOL=" + symbol, rustPrefix + "TENANT=handoff-tenant", rustPrefix + "SOURCE=" + rustSource, rustPrefix + "NATS_URL=" + broker.url, rustPrefix + "LISTEN=" + rustAddress}
			ws := strings.Replace(exchange.server.URL, "http://", "ws://", 1)
			if venue == "binance" {
				rustEnv = append(rustEnv, rustPrefix+"WS_BASE="+ws, rustPrefix+"REST_BASE="+exchange.server.URL)
			} else {
				rustEnv = append(rustEnv, rustPrefix+"WS_URL="+ws+"/ws/v5/public")
			}
			// Same full runtime configuration without the workload identity must fail
			// against this broker; DEVELOPMENT is not a broker authentication bypass.
			startHandoffProcess(t, broker, rustBinary, rustID, rustEnv, false).refused()
			t.Run("authenticated-publish-denied", func(t *testing.T) {
				deniedBroker := startHandoffBroker(t, rustID, true)
				beforeDenials := deniedBroker.denied.publish.Load()
				address := handoffAddress(t)
				env := append([]string(nil), rustEnv...)
				for i, value := range env {
					if strings.HasPrefix(value, rustPrefix+"NATS_URL=") {
						env[i] = rustPrefix + "NATS_URL=" + deniedBroker.url
					}
					if strings.HasPrefix(value, rustPrefix+"LISTEN=") {
						env[i] = rustPrefix + "LISTEN=" + address
					}
				}
				p := startHandoffProcess(t, deniedBroker, rustBinary, rustID, env, true)
				p.waitStarted(exchange, "denied-startup", func() bool { return len(deniedBroker.connections(rustID)) == 1 && exchange.count("depth") == 1 })
				handoffStatus(t, address, http.StatusServiceUnavailable)
				exchange.depth(true, 101, 100)
				handoffWait(t, 5*time.Second, "actual broker publish denial absent", func() bool { return deniedBroker.denied.publish.Load() > beforeDenials })
				handoffStatus(t, address, http.StatusServiceUnavailable)
				p.refused()
				info, err := deniedBroker.js.StreamInfo("HANDOFF")
				if err != nil || info.State.Msgs != 0 || info.State.LastSeq != 0 {
					t.Fatal("denied publisher produced accepted facts")
				}
				handoffWait(t, 5*time.Second, "denied exchange handler survived process join", func() bool { return exchange.count("depth") == 0 })
			})
			t.Run("duplicate-external-assignment-refused", func(t *testing.T) {
				beforeDepth, beforeTrade := exchange.connections("depth"), exchange.connections("trade")
				env := append(goEnv("go-invalid-assignment", false), "MARKET_INGEST_"+mic+"_EXTERNAL_DEPTH_INSTRUMENTS="+instrument+","+instrument)
				startHandoffProcess(t, broker, goBinary, feedGoID, env, true).refused()
				info, err := broker.js.StreamInfo("HANDOFF")
				if err != nil || info.State.Msgs != 0 || info.State.LastSeq != 0 || exchange.connections("depth") != beforeDepth || exchange.connections("trade") != beforeTrade {
					t.Fatal("invalid static assignment reached feed side effects")
				}
			})
			startGo := func(source string, external bool) *handoffProcess {
				p := startHandoffProcess(t, broker, goBinary, feedGoID, goEnv(source, external), true)
				p.waitStarted(exchange, "go-startup", func() bool {
					return len(broker.connections(feedGoID)) == 1 && exchange.count("trade") == 1 && (external || exchange.count("depth") == 1)
				})
				return p
			}
			carryover := make(map[string]time.Time)
			bar := func(source string, id int) {
				// Real trades in adjacent, already elapsed buckets close a bar through
				// the existing Go collector. No clock or derived event is injected.
				bucket := time.Now().UTC().Truncate(time.Minute).Add(time.Duration(id-12) * time.Minute)
				exchange.trade(bucket.Add(time.Second), id)
				exchange.trade(bucket.Add(time.Minute), id+1)
				expected := []time.Time{}
				if previous, ok := carryover[source]; ok {
					// The previous pair's second trade remains in its own bucket.
					// It must be validated before the new bucket, never skipped.
					expected = append(expected, previous)
				}
				expected = append(expected, bucket)
				for _, want := range expected {
					frame := handoffFrame(t, broker, subs["market.crypto.bar"], source, version, instrument)
					event := new(marketpb.MarketDataEvent)
					if proto.Unmarshal(frame.Payload, event) != nil || event.GetBar() == nil {
						t.Fatal("retained trade did not produce a canonical bar")
					}
					b := event.GetBar()
					if event.InstrumentId != instrument || event.Mic != mic || b.TradeCount != 1 || !b.OpenTime.AsTime().Equal(want) || !b.CloseTime.AsTime().Equal(want.Add(time.Minute)) || dec.FromProto(b.Volume).Cmp(big.NewRat(2, 1)) != 0 || dec.FromProto(b.Open).Cmp(big.NewRat(50000, 1)) != 0 || dec.FromProto(b.High).Cmp(big.NewRat(50000, 1)) != 0 || dec.FromProto(b.Low).Cmp(big.NewRat(50000, 1)) != 0 || dec.FromProto(b.Close).Cmp(big.NewRat(50000, 1)) != 0 {
						t.Fatal("retained trade/bar or exact carryover semantics differ")
					}
				}
				carryover[source] = bucket.Add(time.Minute)
			}
			// Advisory Symbol currently differs by owner; no producer is changed
			// or payload normalized here. InstrumentID and all economic values
			// retain common expectations. #1413 owns the remaining compatibility.
			previous := make(map[string]*handoffPrevious)
			depth := func(source, expectedSymbol string, sequence uint64, phase string) {
				switch phase {
				case "initial", "refresh", "reanchor", "reconnect", "rollback":
				default:
					t.Fatal("invalid depth phase")
				}
				for _, subject := range []string{"market.book.snapshot", "market.crypto.quote"} {
					observation := handoffObservation{deadline: time.Now().Add(10 * time.Second)}
					for {
						frame := handoffFrameBefore(t, broker, subs[subject], source, version, instrument, &observation)
						var actual uint64
						if subject == "market.book.snapshot" {
							b := new(marketpb.OrderBookSnapshot)
							if proto.Unmarshal(frame.Payload, b) != nil {
								t.Fatal("book decode refused")
							}
							actual = b.LastUpdateSequence
						} else {
							q := new(marketpb.MarketDataEvent)
							if proto.Unmarshal(frame.Payload, q) != nil {
								t.Fatal("quote decode refused")
							}
							actual = q.SourceSequence
						}
						var state *handoffPrevious
						if source == rustSource {
							state = previous[subject]
							if state == nil {
								state = &handoffPrevious{}
								previous[subject] = state
							}
							repeat, err := state.classify(frame, actual, sequence, time.Now(), observation.deadline)
							if err != nil {
								t.Fatalf("depth observation refused: phase=%s expected=%d actual=%d", phase, sequence, actual)
							}
							if repeat {
								continue
							}
						}
						if subject == "market.book.snapshot" {
							book := new(marketpb.OrderBookSnapshot)
							decoded := proto.Unmarshal(frame.Payload, book) == nil
							levels := len(book.Bids) == 1 && len(book.Asks) == 1
							economics := levels && dec.FromProto(book.Bids[0].Price).Cmp(big.NewRat(50000, 1)) == 0 && dec.FromProto(book.Bids[0].Size).Cmp(big.NewRat(4, 1)) == 0 && dec.FromProto(book.Asks[0].Price).Cmp(big.NewRat(50001, 1)) == 0 && dec.FromProto(book.Asks[0].Size).Cmp(big.NewRat(2, 1)) == 0
							if !decoded || book.InstrumentId != instrument || book.Mic != mic || book.Symbol != expectedSymbol || book.LastUpdateSequence != sequence || !economics {
								t.Fatalf("canonical handoff book differs: decoded=%t instrument=%t mic=%t symbol=%t sequence=%t levels=%t economics=%t", decoded, book.InstrumentId == instrument, book.Mic == mic, book.Symbol == expectedSymbol, book.LastUpdateSequence == sequence, levels, economics)
							}
						} else {
							event := new(marketpb.MarketDataEvent)
							decoded := proto.Unmarshal(frame.Payload, event) == nil
							if !decoded || event.InstrumentId != instrument || event.Mic != mic || event.Symbol != expectedSymbol || event.SourceSequence != sequence || event.GetQuote() == nil {
								t.Fatalf("canonical handoff quote differs: decoded=%t instrument=%t mic=%t symbol=%t sequence=%t quote=%t", decoded, event.InstrumentId == instrument, event.Mic == mic, event.Symbol == expectedSymbol, event.SourceSequence == sequence, event.GetQuote() != nil)
							}
							q := event.GetQuote()
							if dec.FromProto(q.BidPrice).Cmp(big.NewRat(50000, 1)) != 0 || dec.FromProto(q.BidSize).Cmp(big.NewRat(4, 1)) != 0 || dec.FromProto(q.AskPrice).Cmp(big.NewRat(50001, 1)) != 0 || dec.FromProto(q.AskSize).Cmp(big.NewRat(2, 1)) != 0 {
								t.Fatal("canonical handoff quote quantities differ")
							}
						}
						if !time.Now().Before(observation.deadline) {
							t.Fatal("depth validation deadline expired")
						}
						if state != nil {
							state.remember(frame, actual)
						}
						break
					}
				}
			}
			original := startGo("go-original", false)
			exchange.depth(true, 101, 100)
			depth("go-original", instrument, 101, "initial")
			bar("go-original", 1)
			original.stop()
			handoffWait(t, 5*time.Second, "old exchange sockets survived join", func() bool { return exchange.count("depth") == 0 && exchange.count("trade") == 0 })
			retained := startGo("go-retained", true)
			bar("go-retained", 3)
			if exchange.count("depth") != 0 {
				t.Fatal("external-depth Go retained a depth connection")
			}
			rust := startHandoffProcess(t, broker, rustBinary, rustID, rustEnv, true)
			rust.waitStarted(exchange, "rust-startup", func() bool { return len(broker.connections(rustID)) == 1 && exchange.count("depth") == 1 })
			handoffStatus(t, rustAddress, http.StatusServiceUnavailable)
			exchange.depth(true, 101, 100)
			depth(rustSource, symbol, 101, "initial")
			handoffStatus(t, rustAddress, http.StatusOK)
			bar("go-retained", 5)
			// Source age, not a healthy broker alone, controls readiness.
			handoffStatus(t, rustAddress, http.StatusServiceUnavailable)
			exchange.depth(false, 102, 101)
			depth(rustSource, symbol, 102, "refresh")
			handoffStatus(t, rustAddress, http.StatusOK)
			// Binance refreshes REST on the same stream; OKX opens a new stream.
			// Both must revoke readiness before a fresh anchor can be accepted.
			connections := exchange.connections("depth")
			if venue == "binance" {
				requests, release := exchange.holdAnchor(104)
				exchange.depth(false, 105, 104)
				handoffWait(t, 5*time.Second, "gap did not request a fresh REST anchor", func() bool { return exchange.anchorRequests() == requests+1 })
				handoffStatus(t, rustAddress, http.StatusServiceUnavailable)
				release()
				depth(rustSource, symbol, 105, "reanchor")
				if exchange.connections("depth") != connections || exchange.count("depth") != 1 {
					t.Fatal("Binance REST reanchor replaced the expected live stream")
				}
			} else {
				exchange.depth(false, 105, 104)
				handoffWait(t, 10*time.Second, "gap did not reanchor", func() bool { return exchange.connections("depth") > connections && exchange.count("depth") == 1 })
				handoffStatus(t, rustAddress, http.StatusServiceUnavailable)
				exchange.depth(true, 101, 100)
				depth(rustSource, symbol, 101, "reanchor")
			}
			handoffStatus(t, rustAddress, http.StatusOK)
			// Broker disconnect must be recovered with the same authenticated
			// principal. A subsequent source update supplies fresh readiness proof.
			ids := broker.connections(rustID)
			if len(ids) != 1 {
				t.Fatal("Rust principal inventory differs")
			}
			if broker.server.DisconnectClientByID(ids[0]) != nil {
				t.Fatal("controlled broker disconnect failed")
			}
			handoffWait(t, 10*time.Second, "broker reconnect absent", func() bool { now := broker.connections(rustID); return len(now) == 1 && now[0] != ids[0] })
			if venue == "binance" {
				exchange.depth(false, 106, 105)
				depth(rustSource, symbol, 106, "reconnect")
			} else {
				exchange.depth(false, 102, 101)
				depth(rustSource, symbol, 102, "reconnect")
			}
			handoffStatus(t, rustAddress, http.StatusOK)
			rust.stop()
			handoffWait(t, 5*time.Second, "Rust exchange connection survived join", func() bool { return exchange.count("depth") == 0 })
			retained.stop()
			handoffWait(t, 5*time.Second, "retained trades survived join", func() bool { return exchange.count("trade") == 0 })
			exchange.resetAnchor()
			rollback := startGo("go-rollback", false)
			exchange.depth(true, 101, 100)
			depth("go-rollback", instrument, 101, "rollback")
			bar("go-rollback", 7)
			rollback.stop()
			info, err := broker.js.StreamInfo("HANDOFF")
			if err != nil || info.State.FirstSeq != 1 || info.State.Msgs != info.State.LastSeq || info.State.LastSeq == 0 || info.State.LastSeq > 10000 || info.State.NumDeleted != 0 {
				t.Fatal("complete bounded final broker inventory unavailable")
			}
			for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
				message, err := broker.js.GetMsg("HANDOFF", sequence)
				if err != nil {
					t.Fatal("final broker inventory incomplete")
				}
				frame := new(envelopepb.EventFrame)
				if proto.Unmarshal(message.Data, frame) != nil {
					t.Fatal("final broker frame malformed")
				}
				if frame.GetEnvelope().GetSource() == "go-retained" && (message.Subject == "market.book.snapshot" || message.Subject == "market.crypto.quote") {
					t.Fatal("retained Go process published selected depth during handoff")
				}
			}
		})
	}
}

func handoffFrame(t *testing.T, broker *handoffBroker, sub *nats.Subscription, source, version, instrument string) *envelopepb.EventFrame {
	t.Helper()
	observation := handoffObservation{deadline: time.Now().Add(10 * time.Second)}
	return handoffFrameBefore(t, broker, sub, source, version, instrument, &observation)
}

func handoffFrameBefore(t *testing.T, broker *handoffBroker, sub *nats.Subscription, source, version, instrument string, observation *handoffObservation) *envelopepb.EventFrame {
	t.Helper()
	for observation.take(time.Now()) {
		msgs, err := sub.Fetch(1, nats.MaxWait(time.Until(observation.deadline)))
		if err != nil {
			_, consumerErr := sub.ConsumerInfo()
			info, streamErr := broker.js.StreamInfo("HANDOFF")
			var messages, first, last uint64
			if streamErr == nil {
				messages, first, last = info.State.Msgs, info.State.FirstSeq, info.State.LastSeq
			}
			t.Fatalf("canonical frame missing: fetch=%s consumer=%s stream=%s messages=%d first=%d last=%d", handoffBrokerError(err), handoffBrokerError(consumerErr), handoffBrokerError(streamErr), messages, first, last)
		}
		m := msgs[0]
		frame := new(envelopepb.EventFrame)
		if proto.Unmarshal(m.Data, frame) != nil || bus.Validate(frame.GetEnvelope()) != nil {
			t.Fatal("broker frame invalid")
		}
		e := frame.GetEnvelope()
		if m.AckSync() != nil {
			t.Fatal("observer acknowledgement failed")
		}
		if e.ProducerVersion != version || e.TenantId != "handoff-tenant" || e.PartitionKey != instrument || m.Header.Get("Kanz-Partition-Key") != instrument || m.Header.Get("Nats-Msg-Id") != e.IdempotencyKey {
			t.Fatal("source/tenant/version/header binding differs")
		}
		if !time.Now().Before(observation.deadline) {
			t.Fatal("frame observation deadline expired")
		}
		if e.Source != source {
			continue
		}
		return frame
	}
	t.Fatal("expected publisher frame absent")
	return nil
}

func handoffStatus(t *testing.T, address string, want int) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	handoffWait(t, 8*time.Second, fmt.Sprintf("readiness did not reach %d", want), func() bool {
		response, err := client.Get("http://" + address + "/readyz")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == want
	})
}

func handoffBrokerError(err error) string {
	if err == nil {
		return "ok"
	}
	for _, item := range []struct {
		err  error
		name string
	}{
		{nats.ErrConsumerNotFound, "consumer-not-found"}, {nats.ErrConsumerDeleted, "consumer-deleted"},
		{nats.ErrTimeout, "timeout"}, {nats.ErrConnectionClosed, "connection-closed"}, {nats.ErrPermissionViolation, "permission-denied"},
	} {
		if errors.Is(err, item.err) {
			return item.name
		}
	}
	var api *nats.APIError
	if errors.As(err, &api) {
		return fmt.Sprintf("api-%d-%d", api.Code, api.ErrorCode)
	}
	return "other"
}
