package order

import (
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/shadowprocess"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

// FillFoldCapture is diagnostic input/output of the pure fold, not a committed
// transaction. In recovery State is the normalized working input; terminal
// restoration and journal reconciliation remain authoritative Go operations.
type FillFoldCapture struct {
	Tenant, Path                  string
	ExpectedVersion               int64
	EventTime, CapturedAt         time.Time
	State, Candidate              *orderpb.OrderState
	Fill                          *orderpb.Fill
	RejectionCode                 string
	NumericalFailure, Unsupported bool
}

func WithFillFoldObserver(observer func(FillFoldCapture)) ServiceOption {
	return func(s *Service) { s.fillFoldObserver = observer }
}

// SetFillFoldObserver is composition-time only, before the store serves requests.
func (p *Postgres) SetFillFoldObserver(observer func(FillFoldCapture)) { p.fillFoldObserver = observer }

func observeFillFold(ctx context.Context, observer func(FillFoldCapture), path string, version int64, state *orderpb.OrderState, fill *orderpb.Fill, at time.Time, candidate *orderpb.OrderState, err error) {
	if observer == nil {
		return
	}
	c := FillFoldCapture{Tenant: bus.TenantIDFromContext(ctx), Path: path, ExpectedVersion: version, EventTime: at, CapturedAt: time.Now().UTC()}
	// Bound ALL detached bytes before cloning on the capital path. Oversized
	// metadata is explicitly unassessed; it never blocks recording a real fill.
	if state == nil || fill == nil || proto.Size(state)+proto.Size(fill)+proto.Size(candidate) > shadowprocess.MaxBytes-1024 {
		c.Unsupported = true
		observer(c)
		return
	}
	c.State = proto.Clone(state).(*orderpb.OrderState)
	c.Fill = proto.Clone(fill).(*orderpb.Fill)
	if candidate != nil {
		c.Candidate = proto.Clone(candidate).(*orderpb.OrderState)
	}
	if err != nil {
		var rejection *RejectError
		if errors.As(err, &rejection) {
			c.RejectionCode = rejection.Code
		} else {
			c.NumericalFailure = true
		}
	}
	observer(c)
}

func (p *Postgres) FillFoldObserver() func(FillFoldCapture) { return p.fillFoldObserver }
