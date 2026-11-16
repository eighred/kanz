package projection

import (
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	tvsyncpb "github.com/eighred/kanz/kanz-schemas-go/tvsync/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Qualification refuses unsupported wire data instead of agreeing because both
// folds silently ignored it. This validates inputs; production fold owns meaning.
func validateReplayFact(f Fact) error {
	var message proto.Message
	switch f.EventType {
	case evtAccepted:
		message = &orderpb.OrderAccepted{}
	case evtRouted:
		message = &orderpb.OrderRouted{}
	case evtPartiallyFilled:
		message = &orderpb.OrderPartiallyFilled{}
	case evtFilled:
		message = &orderpb.OrderFilled{}
	case evtRejected:
		message = &orderpb.OrderRejected{}
	case evtCancelled:
		message = &orderpb.OrderCancelled{}
	case evtExpired:
		message = &orderpb.OrderExpired{}
	default:
		return ErrReplayInput
	}
	if len(f.Payload) == 0 || int64(len(f.Payload)) > ReplayMaxFactBytes {
		return ErrReplayInput
	}
	if err := (proto.UnmarshalOptions{RecursionLimit: 64}).Unmarshal(f.Payload, message); err != nil {
		return ErrReplayInput
	}
	if !replayMessageKnown(message.ProtoReflect(), 0) {
		return ErrReplayInput
	}
	if _, ok := dec.InDomainDeep(message); !ok {
		return ErrReplayInput
	}
	// A missing state/fill is unsupported evidence even if the live compatibility
	// fold treats it as a no-op. Never qualify a damaged history by that agreement.
	switch ev := message.(type) {
	case *orderpb.OrderAccepted:
		if ev.State == nil || ev.State.OrderId == "" || ev.State.PortfolioId == "" {
			return ErrReplayInput
		}
	case *orderpb.OrderFilled:
		if ev.State == nil || ev.State.OrderId == "" || ev.State.PortfolioId == "" || !replayFillKnown(ev.Fill) {
			return ErrReplayInput
		}
	case *orderpb.OrderPartiallyFilled:
		if ev.State == nil || ev.State.OrderId == "" || ev.State.PortfolioId == "" || !replayFillKnown(ev.Fill) {
			return ErrReplayInput
		}
	}
	return nil
}

func replayFillKnown(f *orderpb.Fill) bool {
	return f != nil && f.FillId != "" && f.OrderId != "" && f.InstrumentId != "" && f.Quantity != nil && f.Price != nil && f.ExecutedAt != nil && (f.Side == orderpb.Side_SIDE_BUY || f.Side == orderpb.Side_SIDE_SELL)
}

func validateReplayCheckpoint(snap replaySnapshot, tenant string, retention time.Duration) error {
	if len(snap.checkpoint) == 0 {
		return ErrReplayNoCheckpoint
	}
	if int64(len(snap.checkpoint)) > ReplayMaxCheckpointBytes {
		return ErrReplayLimit
	}
	var cp tvsyncpb.Checkpoint
	if err := (proto.UnmarshalOptions{RecursionLimit: 64}).Unmarshal(snap.checkpoint, &cp); err != nil {
		return ErrReplayInput
	}
	if !replayMessageKnown(cp.ProtoReflect(), 0) {
		return ErrReplayInput
	}
	if _, ok := dec.InDomainDeep(&cp); !ok {
		return ErrReplayInput
	}
	if cp.GetExactEncodingVersion() != 1 || cp.TenantId != tenant || cp.Seq != snap.checkpointSeq || cp.Seq <= 0 || cp.Seq > snap.watermark || cp.TakenAt == nil || cp.TakenAt.CheckValid() != nil || cp.TakenAt.AsTime().After(snap.at) {
		return ErrReplayInput
	}
	accounts := make(map[string]bool, len(cp.Accounts))
	for _, a := range cp.Accounts {
		if a == nil || a.TenantId != tenant || a.AccountId == "" || accounts[a.AccountId] {
			return ErrReplayInput
		}
		accounts[a.AccountId] = true
		for id, revisions := range a.Orders {
			if id == "" || revisions == nil || len(revisions.Revisions) == 0 {
				return ErrReplayInput
			}
			for _, revision := range revisions.Revisions {
				if revision == nil || revision.State == nil || revision.State.OrderId != id || revision.State.PortfolioId != a.AccountId || revision.Effective == nil || revision.Knowledge == nil {
					return ErrReplayInput
				}
			}
		}
		for _, execution := range a.Execs {
			if execution == nil || execution.Qty == nil || execution.Price == nil || execution.FillId == "" || execution.Effective == nil || execution.Knowledge == nil {
				return ErrReplayInput
			}
		}
		if a.RetainedFrom != nil && a.RetainedFrom.AsTime().After(snap.at.Add(-retention)) {
			return ErrReplayInput
		}
		seen := make(map[string]bool, len(a.SeenFills))
		for _, id := range a.SeenFills {
			if id == "" || seen[id] {
				return ErrReplayInput
			}
			seen[id] = true
		}
		for _, lot := range a.Baseline {
			if lot == nil {
				return ErrReplayInput
			}
			for _, s := range []string{lot.Qty, lot.AvgCost, lot.Realized} {
				if len(s) > 1024 {
					return ErrReplayLimit
				}
				// snapshotAccount emits RatString, never exponent notation. A short
				// adversarial exponent must not cause unbounded big.Rat allocation.
				if !boundedReplayRational(s) {
					return ErrReplayInput
				}
			}
		}
	}
	return nil
}

func boundedReplayRational(s string) bool {
	if s == "" {
		return true
	} // Existing canonical baseline compatibility.
	parts := strings.Split(s, "/")
	if len(parts) > 2 {
		return false
	}
	for i, p := range parts {
		if i == 0 && strings.HasPrefix(p, "-") {
			p = p[1:]
		}
		if p == "" {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
		if i == 1 && strings.Trim(p, "0") == "" {
			return false
		}
	}
	return true
}

// Bound recursion and reject unknown enum/wire fields recursively, including map
// values. Timestamp validation does not replace domain effective-time semantics.
func replayMessageKnown(m protoreflect.Message, depth int) bool {
	if !m.IsValid() || depth > 64 || len(m.GetUnknown()) != 0 {
		return false
	}
	if ts, ok := m.Interface().(*timestamppb.Timestamp); ok && ts.CheckValid() != nil {
		return false
	}
	valid := true
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		check := func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			if field.Kind() == protoreflect.MessageKind {
				return replayMessageKnown(value.Message(), depth+1)
			}
			if field.Kind() == protoreflect.EnumKind {
				return field.Enum().Values().ByNumber(value.Enum()) != nil
			}
			return true
		}
		switch {
		case fd.IsMap():
			v.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
				valid = check(fd.MapValue(), item)
				return valid
			})
		case fd.IsList():
			for i := 0; i < v.List().Len(); i++ {
				if !check(fd, v.List().Get(i)) {
					valid = false
					break
				}
			}
		default:
			valid = check(fd, v)
		}
		return valid
	})
	return valid
}
