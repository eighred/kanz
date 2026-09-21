package fillfact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func ExecutionDigest(fill *orderpb.Fill) (string, error) {
	if fill == nil {
		return "", errors.New("missing execution")
	}
	copy := proto.Clone(fill).(*orderpb.Fill)
	copy.Recovery = nil
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// RequireRecoveryTenant deliberately excludes the legacy shared-bucket tenant
// exception. Automatic correction requires exact attribution, even on a system
// deployment; a shared consumer cannot establish another tenant's book scope.
func RequireRecoveryTenant(env *envelopepb.Envelope, serving string) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), serving); err != nil {
		return err
	}
	if env.GetTenantId() != serving || serving == "" {
		return errors.New("recovery requires exact tenant attribution")
	}
	return nil
}

const (
	SubjectRecovered        = "order.order.execution_recovered"
	RecoveryPositionApplied = "order.order.recovery_position_applied"
	RecoveryLedgerApplied   = "order.order.recovery_ledger_applied"
)

// DecodeRecovery is shared by the two financial books so both require the
// same proven execution and the same portfolio attribution before writing.
func DecodeRecovery(payload []byte) (*orderpb.Fill, string, error) {
	if len(payload) > 1<<20 {
		return nil, "", errors.New("recovered execution exceeds payload bound")
	}
	var event orderpb.ExecutionRecovered
	if err := proto.Unmarshal(payload, &event); err != nil {
		return nil, "", err
	}
	f, st := event.GetFill(), event.GetState()
	if f.GetRecovery() == nil || st.GetPortfolioId() == "" || f.GetOrderId() != st.GetOrderId() || f.GetVenue() != st.GetVenue() || f.GetVenueAccountId() != st.GetVenueAccountId() || f.GetInstrumentId() != st.GetInstrumentId() || f.GetSide() != st.GetSide() {
		return nil, "", errors.New("recovered execution has inconsistent book attribution")
	}
	// Domain validation precedes every decimal comparison performed by a book.
	if _, ok := dec.InDomainDeep(&event); !ok {
		return nil, "", errors.New("recovered execution has invalid decimal domain")
	}
	if err := Validate(f); err != nil {
		return nil, "", err
	}
	digest, err := ExecutionDigest(f)
	if err != nil {
		return nil, "", err
	}
	if digest != f.GetRecovery().GetExecutionDigest() {
		return nil, "", errors.New("recovered execution digest mismatch")
	}
	if f.GetRecovery().GetFeeApproval() != nil {
		if _, err := FeeRevisionTerms(f); err != nil {
			return nil, "", err
		}
	}
	return f, st.GetPortfolioId(), nil
}

// RecoveryAck is enqueued by the book's existing transaction announcer. Publishing
// it directly after a fold would reopen the commit/publish gap recovery closes.
func RecoveryAck(ctx context.Context, subject string, fill *orderpb.Fill, now time.Time) (*outbox.Record, error) {
	p := fill.GetRecovery()
	if p == nil {
		return nil, nil
	}
	if subject != RecoveryPositionApplied && subject != RecoveryLedgerApplied {
		return nil, errors.New("unknown recovery acknowledgement book")
	}
	digest, err := hex.DecodeString(p.GetPayloadDigest())
	if err != nil || len(digest) != 32 || p.GetCaseId() == "" || p.GetMappingVersion() == "" || p.GetSourceCursor() == "" || fill.GetOrderId() == "" || ExecutionKey(fill) == fill.GetFillId() {
		return nil, errors.New("recovered execution has incomplete provenance")
	}
	executionDigest, err := ExecutionDigest(fill)
	if err != nil || executionDigest != p.GetExecutionDigest() {
		return nil, errors.New("recovered execution does not match its evidence digest")
	}
	ack := &orderpb.ExecutionRecoveryApplied{CaseId: p.CaseId, OrderId: fill.OrderId, ExecutionKey: ExecutionKey(fill), PayloadDigest: p.PayloadDigest, ExecutionDigest: executionDigest, RecordedAt: timestamppb.New(now.UTC())}
	record, err := outbox.From(ctx, bus.Event{Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1,
		Domain: "order", EventTime: now, PartitionKey: fill.OrderId, PayloadSchemaRef: "order.v1.ExecutionRecoveryApplied:1", Payload: ack})
	if err != nil {
		return nil, err
	}
	return &record, nil
}
