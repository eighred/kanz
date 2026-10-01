package artifacts

import (
	"context"
	"errors"

	"github.com/eighred/kanz/internal/risk/publish"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

// Handler materializes FACTs under the authenticated deployment tenant. The
// transport envelope and payload must agree before a row can become evidence.
func (s *Store) Handler(tenant string) bus.EventHandler {
	return func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		if err := bus.RequireTenantScope(env.GetTenantId(), tenant); err != nil {
			return err
		}
		bad := errors.New("risk artifact envelope does not match payload")
		if env == nil || tenant == "" || env.TenantId != tenant || env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || env.SchemaVersion != 1 || env.EventTime == nil || len(payload) > 8<<20 {
			return bad
		}
		switch env.EventType {
		case publish.EventTypeCurveCalibrated:
			a := new(domainpb.CalibratedCurve)
			if err := proto.Unmarshal(payload, a); err != nil {
				return err
			}
			if a.Curve == nil || env.PartitionKey != a.Curve.CurrencyCode || !proto.Equal(env.EventTime, a.Curve.AsOf) || env.PayloadSchemaRef != "domain.v1.CalibratedCurve:1" {
				return bad
			}
			return s.SaveCurve(ctx, a)
		case publish.EventTypeFactorModelFitted:
			a := new(factorpb.FactorModelSnapshot)
			if err := proto.Unmarshal(payload, a); err != nil {
				return err
			}
			if a.Model == nil || env.PartitionKey != a.Model.ModelId || !proto.Equal(env.EventTime, a.Model.AsOf) || env.PayloadSchemaRef != "factor.v1.FactorModelSnapshot:1" {
				return bad
			}
			return s.SaveModel(ctx, a)
		default:
			return bad
		}
	}
}
