package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

// syntheticCommand builds a stand-in concrete command — a message embedding
// CommandMetadata as field 1, exactly the command.v1 layout — without a
// generated command body (none exists yet). It proves the issuer extraction is
// generic over any concrete command, not tied to a specific schema.
func syntheticCommand(t *testing.T, issuer, target string) proto.Message {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("buscmd/test_command.proto"),
		Package:    proto.String("buscmd"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"command/v1/command.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("TestCommand"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:     proto.String("metadata"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String(".command.v1.CommandMetadata"),
				},
				{
					Name:   proto.String("note"),
					Number: proto.Int32(2),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	md := fd.Messages().Get(0)
	msg := dynamicpb.NewMessage(md)
	meta := &commandpb.CommandMetadata{Issuer: issuer, TargetId: target}
	msg.Set(md.Fields().ByName("metadata"), protoreflect.ValueOfMessage(meta.ProtoReflect()))
	msg.Set(md.Fields().ByName("note"), protoreflect.ValueOfString("hello"))
	return msg
}

func TestExtractCommandIssuer(t *testing.T) {
	b, err := proto.Marshal(syntheticCommand(t, "user:akif", "pf-1"))
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := extractCommandIssuer(b)
	if err != nil {
		t.Fatal(err)
	}
	if issuer != "user:akif" {
		t.Fatalf("issuer=%q want user:akif", issuer)
	}
}

func TestExtractCommandIssuer_NoMetadata(t *testing.T) {
	// A payload whose field 1 is absent ⇒ empty issuer, no error (the caller
	// decides a missing issuer is a defect).
	var b []byte
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, 7)
	issuer, err := extractCommandIssuer(b)
	if err != nil || issuer != "" {
		t.Fatalf("issuer=%q err=%v, want empty/nil", issuer, err)
	}
}

func TestExtractCommandIssuer_Malformed(t *testing.T) {
	if _, err := extractCommandIssuer([]byte{0xff, 0xff}); err == nil {
		t.Fatal("want error on malformed payload")
	}
}

type nopClient struct{ published bool }

func (c *nopClient) Publish(context.Context, Message) error                   { c.published = true; return nil }
func (c *nopClient) Subscribe(context.Context, string, string, Handler) error { return nil }
func (c *nopClient) Close() error                                             { return nil }

func commandEvent(payload proto.Message) Event {
	et := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	return Event{
		Subject:          "risk.portfolio.rebalance.cmd",
		EventType:        "risk.portfolio.rebalance.cmd",
		EventClass:       envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:    1,
		Domain:           "risk",
		EventTime:        et,
		PartitionKey:     "pf-1",
		IdempotencyKey:   "cmd-key-1", // COMMAND requires a caller-supplied key
		PayloadSchemaRef: "buscmd.TestCommand:1",
		Payload:          payload,
	}
}

func TestProducerCommandIssuerGuard(t *testing.T) {
	verify := func(_ context.Context, issuer string) error {
		if issuer == "user:akif" {
			return nil
		}
		return errors.New("forged")
	}
	cc := &nopClient{}
	p, err := NewProducer(cc, ProducerConfig{Source: "s/1", ProducerVersion: "v1", Tenant: "acme", VerifyCommandIssuer: verify})
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Publish(context.Background(), commandEvent(syntheticCommand(t, "user:akif", "pf-1"))); err != nil {
		t.Fatalf("allowed command should publish: %v", err)
	}
	if !cc.published {
		t.Fatal("allowed command did not reach the client")
	}

	cc.published = false
	if err := p.Publish(context.Background(), commandEvent(syntheticCommand(t, "user:eve", "pf-1"))); err == nil {
		t.Fatal("forged issuer should be rejected")
	}
	if cc.published {
		t.Fatal("forged command must not reach the client")
	}
}

func TestProducerCommandMissingIssuer(t *testing.T) {
	cc := &nopClient{}
	p, _ := NewProducer(cc, ProducerConfig{Source: "s/1", ProducerVersion: "v1", Tenant: "acme",
		VerifyCommandIssuer: func(context.Context, string) error { return nil }})
	if err := p.Publish(context.Background(), commandEvent(syntheticCommand(t, "", "pf-1"))); !errors.Is(err, ErrMissingCommandIssuer) {
		t.Fatalf("got %v, want ErrMissingCommandIssuer", err)
	}
}

func TestProducerNonCommandSkipsIssuerCheck(t *testing.T) {
	called := false
	cc := &nopClient{}
	p, _ := NewProducer(cc, ProducerConfig{Source: "s/1", ProducerVersion: "v1", Tenant: "acme",
		VerifyCommandIssuer: func(context.Context, string) error { called = true; return errors.New("should not run") }})
	et := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	fact := Event{
		Subject: "x.y.z", EventType: "x.y.z", EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: "x", EventTime: et, PayloadSchemaRef: "x:1", Payload: timestamppb.New(et),
	}
	if err := p.Publish(context.Background(), fact); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("verifier must not run for non-COMMAND events")
	}
}
