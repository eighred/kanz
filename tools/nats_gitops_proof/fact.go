// This diagnostic builds and validates one synthetic canonical FACT. It never
// connects to a broker or loads credentials.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func main() {
	if len(os.Args) != 2 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(os.Args[1]) {
		panic("one full source commit is required")
	}
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
		ProducerVersion: os.Args[1], PartitionKey: "NATS-GITOPS-SYNTHETIC", ProducerSequence: 1,
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
	check(json.NewEncoder(os.Stdout).Encode(map[string]any{
		"source": os.Args[1], "subject": env.EventType, "stream": "MARKET", "eventID": env.EventId,
		"frame": base64.StdEncoding.EncodeToString(frame), "sha256": hex.EncodeToString(hash[:]),
		"headers":            map[string]string{"Nats-Msg-Id": env.EventId, "X-Kanz-Fixture": "nats-gitops"},
		"canonicalValidated": true, "negativeControlPassed": true,
	}))
}

func check(err error) {
	if err != nil {
		panic(fmt.Sprintf("canonical synthetic FACT refused: %v", err))
	}
}
