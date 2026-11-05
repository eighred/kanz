package order

import (
	"context"
	"errors"
	"github.com/eighred/kanz/internal/shadowprocess"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"time"
)

// TerminalFoldCapture is the pure Cancel/Expire result, before announcement
// markers or parent attribution. It grants neither withdrawal nor commit authority.
type TerminalFoldCapture struct {
	Tenant, Path, Operation string
	ExpectedVersion         int64
	EventTime, CapturedAt   time.Time
	State, Candidate        *orderpb.OrderState
	UnfilledQuantity        *commonpb.Decimal
	RejectionCode           string
	Unsupported             bool
}

func WithTerminalFoldObserver(observer func(TerminalFoldCapture)) ServiceOption {
	return func(s *Service) { s.terminalFoldObserver = observer }
}

// Composition-time carrier for the one shared optional diagnostic worker.
func (p *Postgres) SetTerminalFoldObserver(observer func(TerminalFoldCapture)) {
	p.terminalFoldObserver = observer
}
func (p *Postgres) TerminalFoldObserver() func(TerminalFoldCapture) { return p.terminalFoldObserver }

func observeTerminalFold(ctx context.Context, observer func(TerminalFoldCapture), operation, path string, version int64, state, candidate *orderpb.OrderState, quantity *commonpb.Decimal, at time.Time, err error) {
	if observer == nil {
		return
	}
	c := TerminalFoldCapture{Tenant: bus.TenantIDFromContext(ctx), Path: path, Operation: operation, ExpectedVersion: version, EventTime: at, CapturedAt: time.Now().UTC()}
	if state == nil || proto.Size(state)+proto.Size(candidate)+proto.Size(quantity) > shadowprocess.MaxBytes-1024 {
		c.Unsupported = true
		observer(c)
		return
	}
	c.State = proto.Clone(state).(*orderpb.OrderState)
	if candidate != nil {
		c.Candidate = proto.Clone(candidate).(*orderpb.OrderState)
	}
	if quantity != nil {
		c.UnfilledQuantity = proto.Clone(quantity).(*commonpb.Decimal)
	}
	if err != nil {
		var rejection *RejectError
		if errors.As(err, &rejection) {
			c.RejectionCode = rejection.Code
		} else {
			c.Unsupported = true
		}
	}
	observer(c)
}
