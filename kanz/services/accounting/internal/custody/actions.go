package custody

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrInvalidAction      = errors.New("custody: invalid action")
	ErrStaleRevision      = errors.New("custody: stale break revision")
	ErrActionConflict     = errors.New("custody: request id belongs to another action")
	ErrDurableActionStore = errors.New("custody: operator actions require durable evidence storage")
)

// Action carries gateway-bound identity separately from the client's decision.
// Assignment is self-claim: no free-text target can impersonate another person.
type Action struct {
	Tenant           string
	Actor            string
	RequestID        string
	BreakID          string
	Kind             string
	Explanation      string
	ExpectedRevision int64
}

type ActionState struct {
	Status      string `json:"status"`
	Assignee    string `json:"assignee"`
	Explanation string `json:"explanation"`
	Revision    int64  `json:"revision,string"`
}

type ActionEvidence struct {
	RequestID  string        `json:"request_id"`
	BreakID    string        `json:"break_id"`
	Actor      string        `json:"actor"`
	Action     string        `json:"action"`
	Before     ActionState   `json:"before"`
	After      ActionState   `json:"after"`
	RecordedAt time.Time     `json:"recorded_at"`
	Values     *ActionValues `json:"values,omitempty"`
}

type ActionValues struct {
	IBOR       string `json:"ibor"`
	Custodian  string `json:"custodian"`
	Difference string `json:"difference"`
}

func (a Action) validate() error {
	if strings.TrimSpace(a.Tenant) == "" || strings.TrimSpace(a.Actor) == "" || a.Actor != strings.TrimSpace(a.Actor) || len(a.Actor) > 256 ||
		strings.TrimSpace(a.RequestID) == "" || len(a.RequestID) > 128 || len(a.BreakID) > 1024 || a.BreakID == "" || a.ExpectedRevision <= 0 || a.ExpectedRevision == math.MaxInt64 {
		return ErrInvalidAction
	}
	if a.Kind == "claim" && a.Explanation == "" {
		return nil
	}
	if a.Kind == "explain" && strings.TrimSpace(a.Explanation) != "" && len(a.Explanation) <= 4096 {
		return nil
	}
	return ErrInvalidAction
}

func (a Action) digest() string {
	data, _ := json.Marshal(a)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func actionState(b Break) ActionState {
	return ActionState{Status: b.Status.String(), Assignee: b.Assignee, Explanation: b.Explanation, Revision: b.Revision}
}

func applyAction(a Action, before Break, now time.Time) (Break, ActionEvidence, error) {
	if before.Revision != a.ExpectedRevision {
		return Break{}, ActionEvidence{}, ErrStaleRevision
	}
	after := before
	var err error
	if a.Kind == "claim" {
		err = after.Assign(a.Actor, now)
	} else {
		err = after.Explain(a.Explanation, now)
	}
	if err != nil {
		return Break{}, ActionEvidence{}, err
	}
	after.Revision++
	return after, ActionEvidence{RequestID: a.RequestID, BreakID: a.BreakID, Actor: a.Actor, Action: a.Kind, Before: actionState(before), After: actionState(after), RecordedAt: now.UTC()}, nil
}

const SubjectActionRecorded = "accounting.custody.action_recorded"

func actionFact(ctx context.Context, tenant string, e ActionEvidence) (outbox.Record, error) {
	var values *accountingpb.CustodyActionValues
	if e.Values != nil {
		values = &accountingpb.CustodyActionValues{Ibor: e.Values.IBOR, Custodian: e.Values.Custodian, Difference: e.Values.Difference}
	}
	state := func(s ActionState) *accountingpb.CustodyActionState {
		return &accountingpb.CustodyActionState{Status: wireStatus(parseStatus(s.Status)), Assignee: s.Assignee, Explanation: s.Explanation, Revision: s.Revision}
	}
	return outbox.From(ctx, bus.Event{Subject: SubjectActionRecorded, EventType: SubjectActionRecorded,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting", TenantID: tenant,
		EventTime: e.RecordedAt, CorrelationID: e.RequestID, PartitionKey: e.BreakID,
		PayloadSchemaRef: "accounting.v1.CustodyActionRecorded:1", Payload: &accountingpb.CustodyActionRecorded{
			RequestId: e.RequestID, BreakId: e.BreakID, Actor: e.Actor, Action: e.Action, Before: state(e.Before), After: state(e.After), RecordedAt: timestamppb.New(e.RecordedAt), Values: values},
	})
}

// A volatile store cannot acknowledge a human investigation as durable evidence.
func (m *MemoryStore) ApplyAction(_ context.Context, a Action) (ActionEvidence, error) {
	if err := a.validate(); err != nil {
		return ActionEvidence{}, err
	}
	return ActionEvidence{}, ErrDurableActionStore
}
func (m *MemoryStore) ActionsEnabled() bool { return false }
func (p *Postgres) ActionsEnabled() bool    { return true }
