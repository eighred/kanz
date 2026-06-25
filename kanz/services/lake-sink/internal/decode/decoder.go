package decode

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Decoder turns a raw payload + its schema ref into a JSON object, using the
// descriptor the Resolver fetched from the registry. Dynamic decode (not a
// compiled-in type) is the schema-evolution seam: a field added additively to a
// payload schema surfaces as a new JSON key — a new lake column — with no
// redeploy of this service.
type Decoder struct {
	resolver Resolver
	marshal  protojson.MarshalOptions
}

func NewDecoder(resolver Resolver) *Decoder {
	return &Decoder{
		resolver: resolver,
		// UseProtoNames keys columns by the proto field name the lakehouse
		// schema tracks; EmitDefaultValues keeps a column present even when the
		// field is at its zero on the wire, so the table schema is stable row to
		// row rather than sparse.
		marshal: protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true},
	}
}

// Decode resolves ref and unmarshals payload into a JSON object. An empty ref or
// payload yields (nil, nil) — envelope-only events (lifecycle FACTs, commands
// with no body) still land their envelope columns. A nil resolver means decode
// is disabled (envelope-only landing), also returning (nil, nil).
func (d *Decoder) Decode(ctx context.Context, ref string, payload []byte) (json.RawMessage, error) {
	if d.resolver == nil || ref == "" || len(payload) == 0 {
		return nil, nil
	}
	md, err := d.resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil, fmt.Errorf("ref %s: unmarshal payload: %w", ref, err)
	}
	b, err := d.marshal.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("ref %s: marshal json: %w", ref, err)
	}
	return json.RawMessage(b), nil
}
