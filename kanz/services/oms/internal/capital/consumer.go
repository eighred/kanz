package capital

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/eighred/kanz/internal/cashview"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// ApplyEnvelope is the durable accounting consumer. Its subscriber must use the
// tenant's authenticated broker connection and accounting-only publish grants;
// the self-declared Source field is not an authentication mechanism. A foreign
// tenant cannot write or quarantine this pool. Contradictory evidence inside
// the authenticated tenant does quarantine it before the handler returns.
func ApplyEnvelope(ctx context.Context, pool *pgxpool.Pool, tenant string, env *envelopepb.Envelope, payload []byte) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), tenant); err != nil {
		return err
	}
	if bus.TenantIDFromContext(ctx) != tenant {
		return fmt.Errorf("capital: delivery context is not scoped to the accounting tenant")
	}
	valid := env.GetEventClass() == envelopepb.EventClass_EVENT_CLASS_FACT &&
		env.GetEventType() == cashview.Subject && env.GetDomain() == "accounting" &&
		env.GetSchemaVersion() == 1 && env.GetPayloadSchemaRef() == "accounting.v1.PortfolioCashBalance:1"
	for _, flag := range env.GetQualityFlags() {
		switch flag {
		case envelopepb.QualityFlag_QUALITY_FLAG_BACKFILLED, envelopepb.QualityFlag_QUALITY_FLAG_LATE,
			envelopepb.QualityFlag_QUALITY_FLAG_REVISED, envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED:
		default:
			valid = false
		}
	}
	var msg accountingpb.PortfolioCashBalance
	if len(payload) <= 512<<10 && proto.Unmarshal(payload, &msg) == nil {
		valid = valid && env.GetPartitionKey() == msg.GetPortfolioId() && validID(msg.GetPortfolioId()) &&
			env.GetEventTime() != nil && env.GetEventTime().CheckValid() == nil &&
			msg.GetAsOf() != nil && msg.GetAsOf().CheckValid() == nil &&
			env.GetEventTime().AsTime().Equal(msg.GetAsOf().AsTime())
	} else {
		valid = false
	}
	if valid {
		return ApplyPayload(ctx, pool, payload)
	}
	// Bind both framing and payload, without retaining financial source data in
	// a diagnostic log. The length prefix makes the two byte strings unambiguous.
	framing, err := proto.MarshalOptions{Deterministic: true}.Marshal(env)
	if err != nil {
		return err
	}
	h := sha256.New()
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(framing)))
	h.Write(size[:])
	h.Write(framing)
	h.Write(payload)
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	if err := invalidateSource(ctx, pool, digest); err != nil {
		return err // failure to persist the safety latch must remain retryable
	}
	return ErrInvalid
}
