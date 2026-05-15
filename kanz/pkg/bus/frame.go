package bus

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// Unframe deserializes a bus.Message body into an Envelope and the opaque
// payload bytes. Consumers then look up the payload schema in the registry
// (EVT-16) and decode the payload against it. The Envelope is left as-is —
// callers that want defensive validation should run Validate themselves.
func Unframe(body []byte) (*envelopepb.Envelope, []byte, error) {
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(body, &frame); err != nil {
		return nil, nil, fmt.Errorf("frame unmarshal: %w", err)
	}
	if frame.Envelope == nil {
		return nil, nil, errors.New("frame missing envelope")
	}
	return frame.Envelope, frame.Payload, nil
}
