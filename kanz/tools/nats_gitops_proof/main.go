// This diagnostic builds a synthetic canonical FACT and can publish only that
// prepared fixture from the owned SPIRE-authenticated test Pod.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

type preparedFact struct {
	Source                string            `json:"source"`
	Subject               string            `json:"subject"`
	Stream                string            `json:"stream"`
	EventID               string            `json:"eventID"`
	Frame                 string            `json:"frame"`
	SHA256                string            `json:"sha256"`
	Headers               map[string]string `json:"headers"`
	CanonicalValidated    bool              `json:"canonicalValidated"`
	NegativeControlPassed bool              `json:"negativeControlPassed"`
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "-publish" {
		if err := publish(os.Args[2]); err != nil {
			// Only our closed error categories leave this process. Library errors
			// may describe an SVID path or connection, and are not fixture evidence.
			_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"fixtureError": err.Error()})
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(os.Args[1]) {
		panic("one full source commit is required")
	}
	check(json.NewEncoder(os.Stdout).Encode(makeFixture(os.Args[1])))
}

func makeFixture(source string) preparedFact {
	id, err := uuid.NewV7()
	check(err)
	now := timestamppb.New(time.Now().UTC())
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: "NATS-GITOPS-SYNTHETIC", Symbol: "NATS-GITOPS-SYNTHETIC", Mic: "XOFF",
		EventTime: now, SourceSequence: 1,
		Data: &marketpb.MarketDataEvent_Quote{Quote: &marketpb.Quote{
			BidPrice: &commonpb.Decimal{Coefficient: 10001, Exponent: -2},
			AskPrice: &commonpb.Decimal{Coefficient: 10002, Exponent: -2},
			BidSize:  &commonpb.Decimal{Coefficient: 1}, AskSize: &commonpb.Decimal{Coefficient: 2},
		}},
	})
	check(err)
	env := &envelopepb.Envelope{
		EventId: id.String(), EventType: "market.crypto.quote", SchemaVersion: 1, EnvelopeVersion: 2,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "market", EventTime: now,
		IngestionTime: now, PublishTime: now, CorrelationId: id.String(), Source: "market-ingest/nats-gitops-fixture",
		ProducerVersion: source, PartitionKey: "NATS-GITOPS-SYNTHETIC", ProducerSequence: 1,
		IdempotencyKey: id.String(), QualityFlags: []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_SYNTHETIC},
		PayloadSchemaRef: "market.v1.MarketDataEvent:1", TenantId: bus.SystemTenant,
	}
	check(bus.Validate(env))
	negative := proto.Clone(env).(*envelopepb.Envelope)
	negative.IdempotencyKey = "conflicting-fact-id"
	if bus.Validate(negative) == nil {
		panic("canonical FACT validation failed its negative control")
	}
	frame, err := proto.Marshal(&envelopepb.EventFrame{Envelope: env, Payload: payload})
	check(err)
	decoded, body, err := bus.Unframe(frame)
	check(err)
	check(bus.Validate(decoded))
	var event marketpb.MarketDataEvent
	check(proto.Unmarshal(body, &event))
	if event.GetQuote() == nil || event.InstrumentId != env.PartitionKey || !proto.Equal(decoded, env) {
		panic("canonical FACT round trip differs")
	}
	hash := sha256.Sum256(frame)
	return preparedFact{
		Source: source, Subject: env.EventType, Stream: "MARKET", EventID: env.EventId,
		Frame: base64.StdEncoding.EncodeToString(frame), SHA256: hex.EncodeToString(hash[:]),
		Headers:            map[string]string{"Nats-Msg-Id": env.EventId, "X-Kanz-Fixture": "nats-gitops"},
		CanonicalValidated: true, NegativeControlPassed: true,
	}
}

func validatePrepared(fact preparedFact, source string) ([]byte, error) {
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(source) || fact.Source != source || fact.Subject != "market.crypto.quote" || fact.Stream != "MARKET" {
		return nil, fmt.Errorf("fixture-source-or-subject-refused")
	}
	frame, err := base64.StdEncoding.Strict().DecodeString(fact.Frame)
	if err != nil || len(frame) == 0 || len(frame) > 65536 {
		return nil, fmt.Errorf("fixture-frame-refused")
	}
	hash := sha256.Sum256(frame)
	if hex.EncodeToString(hash[:]) != fact.SHA256 {
		return nil, fmt.Errorf("fixture-frame-digest-refused")
	}
	env, payload, err := bus.Unframe(frame)
	if err != nil || bus.Validate(env) != nil || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetEventType() != fact.Subject || env.GetProducerVersion() != source || env.GetTenantId() != bus.SystemTenant || env.GetEventId() != fact.EventID {
		return nil, fmt.Errorf("fixture-envelope-refused")
	}
	if env.GetSource() != "market-ingest/nats-gitops-fixture" || env.GetDomain() != "market" || env.GetPayloadSchemaRef() != "market.v1.MarketDataEvent:1" || len(env.GetQualityFlags()) != 1 || env.GetQualityFlags()[0] != envelopepb.QualityFlag_QUALITY_FLAG_SYNTHETIC {
		return nil, fmt.Errorf("fixture-envelope-refused")
	}
	var event marketpb.MarketDataEvent
	if proto.Unmarshal(payload, &event) != nil || event.GetQuote() == nil || event.GetInstrumentId() != "NATS-GITOPS-SYNTHETIC" || event.GetInstrumentId() != env.GetPartitionKey() {
		return nil, fmt.Errorf("fixture-payload-refused")
	}
	if len(fact.Headers) != 2 || fact.Headers["Nats-Msg-Id"] != env.GetEventId() || fact.Headers["X-Kanz-Fixture"] != "nats-gitops" {
		return nil, fmt.Errorf("fixture-headers-refused")
	}
	return frame, nil
}

func publish(source string) error {
	var fact preparedFact
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&fact) != nil {
		return fmt.Errorf("fixture-document-refused")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("fixture-document-refused")
	}
	frame, err := validatePrepared(fact, source)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svid, err := transport.NewSource(ctx, "unix:///run/spiffe/spire-agent.sock")
	if err != nil {
		return fmt.Errorf("fixture-svid-unavailable")
	}
	defer func() { _ = svid.Close() }()
	identity, err := svid.GetX509SVID()
	if err != nil || identity.ID.String() != "spiffe://kanz.internal/ns/kanz-services/sa/market-ingest" {
		return fmt.Errorf("fixture-publisher-identity-refused")
	}
	broker, err := transport.ServiceID("kanz-messaging", "nats")
	if err != nil {
		return fmt.Errorf("fixture-broker-identity-refused")
	}
	conn, err := nats.Connect("nats://nats.kanz-messaging.svc:4222", nats.Name("nats-gitops-fixture"), nats.Timeout(5*time.Second), nats.NoReconnect(),
		nats.Secure(transport.ClientTLSConfig(svid, transport.AuthorizeServices(broker))))
	if err != nil {
		return fmt.Errorf("fixture-broker-connect-refused")
	}
	defer conn.Close()
	client, err := jetstream.New(conn)
	if err != nil {
		return fmt.Errorf("fixture-jetstream-client-refused")
	}
	msg := nats.NewMsg(fact.Subject)
	msg.Data = frame
	for key, value := range fact.Headers {
		msg.Header.Set(key, value)
	}
	// PublishMsg waits for the actual stream PubAck. It requires only the
	// unchanged market-ingest subject grant and reply inbox, no management API.
	ack, err := client.PublishMsg(ctx, msg)
	if err != nil || ack.Stream != "MARKET" || ack.Sequence != 1 || ack.Duplicate {
		return fmt.Errorf("fixture-publish-ack-refused")
	}
	if json.NewEncoder(os.Stdout).Encode(map[string]any{"source": source, "stream": ack.Stream, "sequence": ack.Sequence, "duplicate": ack.Duplicate, "acknowledged": true}) != nil {
		return fmt.Errorf("fixture-ack-output-refused")
	}
	return nil
}

func check(err error) {
	if err != nil {
		panic(fmt.Sprintf("canonical synthetic FACT refused: %v", err))
	}
}
