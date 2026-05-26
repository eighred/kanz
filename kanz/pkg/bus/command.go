package bus

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
)

// CommandIssuerFunc verifies that a command's declared issuer is one the caller
// (carried on ctx) is allowed to issue under — the AUTH-01c forged-issuer
// guard. It returns a non-nil error to REJECT the publish. It is injected via
// ProducerConfig.VerifyCommandIssuer so the bus stays decoupled from the auth
// layer (the same stance as the injected *tls.Config in SEC-01c): transport
// enforces "a command must carry a verifiable issuer" without knowing how
// identity works. auth.VerifyCommandIssuer satisfies this signature.
type CommandIssuerFunc func(ctx context.Context, issuer string) error

// commandMetadataField is the field number CommandMetadata occupies in EVERY
// concrete command payload (command.v1: "embedded as field 1 of every concrete
// command"). The bus reads the issuer structurally from that field, so it
// enforces the issuer invariant across all command types while staying
// payload-blind (EVT-17b) — it never links the concrete command schema.
const commandMetadataField = 1

// ErrMissingCommandIssuer rejects a COMMAND whose payload carries no
// CommandMetadata.issuer — an unauthenticated command is a defect (the issuer
// is "Required" per command.v1 / event-class-rules §2).
var ErrMissingCommandIssuer = errors.New("bus: COMMAND payload missing CommandMetadata.issuer")

// extractCommandIssuer pulls CommandMetadata.issuer from a marshaled command
// payload by reading field 1 (the embedded CommandMetadata) generically, with
// no dependency on the concrete command type. Returns "" when there is no
// field-1 message (the caller decides whether that is a defect).
func extractCommandIssuer(payload []byte) (string, error) {
	meta, ok, err := fieldBytes(payload, commandMetadataField)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	var cm commandpb.CommandMetadata
	if err := proto.Unmarshal(meta, &cm); err != nil {
		return "", fmt.Errorf("bus: decode CommandMetadata: %w", err)
	}
	return cm.GetIssuer(), nil
}

// fieldBytes scans the top-level protobuf fields for field number `field` with
// the length-delimited wire type and returns its raw bytes. It does not decode
// any other field, so an unknown/forward-compatible command body is skipped
// rather than rejected.
func fieldBytes(b []byte, field protowire.Number) ([]byte, bool, error) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false, errors.New("bus: malformed command payload")
		}
		b = b[n:]
		if num == field && typ == protowire.BytesType {
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, false, errors.New("bus: malformed CommandMetadata field")
			}
			return v, true, nil
		}
		skip := protowire.ConsumeFieldValue(num, typ, b)
		if skip < 0 {
			return nil, false, errors.New("bus: malformed command payload")
		}
		b = b[skip:]
	}
	return nil, false, nil
}
