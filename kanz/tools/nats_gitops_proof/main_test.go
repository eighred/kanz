package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

func TestPreparedFixtureRefusesChangedSourceSubjectBytesAndHeaders(t *testing.T) {
	source := strings.Repeat("a", 40)
	if _, err := validatePrepared(makeFixture(source), source); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"source", "subject", "stream", "bytes", "digest", "headers"} {
		t.Run(name, func(t *testing.T) {
			fact := makeFixture(source)
			switch name {
			case "source":
				fact.Source = strings.Repeat("b", 40)
			case "subject":
				fact.Subject = "order.order.submit"
			case "stream":
				fact.Stream = "EXECUTION"
			case "bytes":
				fact.Frame = base64.StdEncoding.EncodeToString([]byte("not the prepared FACT"))
			case "digest":
				fact.SHA256 = strings.Repeat("0", 64)
			case "headers":
				fact.Headers["Nats-Msg-Id"] = "conflicting-id"
			}
			if _, err := validatePrepared(fact, source); err == nil {
				t.Fatal("changed prepared fixture was admitted before broker access")
			}
		})
	}
}

func TestPreparedFixtureRefusesAResignedNonFactEnvelope(t *testing.T) {
	source := strings.Repeat("a", 40)
	fact := makeFixture(source)
	frame, err := base64.StdEncoding.DecodeString(fact.Frame)
	if err != nil {
		t.Fatal(err)
	}
	env, payload, err := bus.Unframe(frame)
	if err != nil {
		t.Fatal(err)
	}
	env.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
	changed, err := proto.Marshal(&envelopepb.EventFrame{Envelope: env, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	fact.Frame = base64.StdEncoding.EncodeToString(changed)
	hash := sha256.Sum256(changed)
	fact.SHA256 = hex.EncodeToString(hash[:])
	if _, err := validatePrepared(fact, source); err == nil {
		t.Fatal("a recomputed checksum must not turn the fixture publisher into a command publisher")
	}
}
