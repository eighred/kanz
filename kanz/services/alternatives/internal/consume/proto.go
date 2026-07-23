package consume

import (
	"fmt"
	"math/big"

	altpb "github.com/kanz-eng/kanz-schemas-go/alternatives/v1"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	alt "github.com/kanz-eng/kanz/internal/alternatives"
	"github.com/kanz-eng/kanz/internal/dec"
)

// DecodeProto returns the Decoder for one commitment-lifecycle subject. The
// composition root binds one per subscription, which is why this is a factory
// rather than a single function: the wire payload's TYPE is determined by the
// subject it arrived on, and nothing in the bytes themselves says which it is.
//
// It replaces DecodeJSON on the live path. DecodeJSON decodes the Go-native
// alternatives.Event as JSON, which is not what an EventFrame carries — it was
// a placeholder for exactly this, and consume.go's own doc says so.
func DecodeProto(eventType string) Decoder {
	return func(payload []byte) (*alt.Event, error) {
		switch eventType {
		case alt.SubjectCommitted:
			var m altpb.Commitment
			if err := proto.Unmarshal(payload, &m); err != nil {
				return nil, fmt.Errorf("consume: %s: %w", eventType, err)
			}
			return event(m.GetCommitmentId(), m.GetCommitmentId(), alt.EventCommit,
				m.GetCommittedAmount(), m.GetCommitmentDate())
		case alt.SubjectCalled:
			var m altpb.CapitalCall
			if err := proto.Unmarshal(payload, &m); err != nil {
				return nil, fmt.Errorf("consume: %s: %w", eventType, err)
			}
			return event(m.GetCallId(), m.GetCommitmentId(), alt.EventCall,
				m.GetAmount(), m.GetCallDate())
		case alt.SubjectDistributed:
			var m altpb.Distribution
			if err := proto.Unmarshal(payload, &m); err != nil {
				return nil, fmt.Errorf("consume: %s: %w", eventType, err)
			}
			return event(m.GetDistributionId(), m.GetCommitmentId(), alt.EventDistribution,
				m.GetAmount(), m.GetDistributionDate())
		case alt.SubjectMarked:
			var m altpb.NAVMark
			if err := proto.Unmarshal(payload, &m); err != nil {
				return nil, fmt.Errorf("consume: %s: %w", eventType, err)
			}
			return event(m.GetMarkId(), m.GetCommitmentId(), alt.EventNAVMark,
				m.GetNav(), m.GetAsOf())
		default:
			return nil, fmt.Errorf("consume: no decoder for event type %q — the "+
				"subject determines the payload type, so a subject this build does "+
				"not know is a wiring error, not a bad message", eventType)
		}
	}
}

// event builds the fold's Event, refusing anything it cannot represent exactly.
func event(id, commitmentID string, typ alt.EventType, amt *commonpb.Decimal, at *timestamppb.Timestamp) (*alt.Event, error) {
	if id == "" {
		return nil, fmt.Errorf("consume: event has no id")
	}
	if commitmentID == "" {
		return nil, fmt.Errorf("consume: event %s names no commitment", id)
	}
	// FromProtoChecked, not FromProto: this is untrusted wire input. An exponent
	// outside the representable range is not a small number — it is one this
	// platform cannot hold, and coercing it to zero would record a capital call
	// of nothing and report success.
	r, ok := dec.FromProtoChecked(amt)
	if !ok {
		return nil, fmt.Errorf("consume: event %s carries an amount this platform "+
			"cannot represent exactly; refusing rather than rounding it", id)
	}
	if r == nil {
		r = new(big.Rat)
	}
	return &alt.Event{
		EventID:      id,
		CommitmentID: commitmentID,
		Type:         typ,
		Amount:       r,
		Date:         at.AsTime().UTC(),
	}, nil
}
