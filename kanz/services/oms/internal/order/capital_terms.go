package order

import (
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/refdata"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

var ErrCapitalTermsUnknown = errors.New("oms: settlement or maximum fee terms unknown")

// CapitalTerms supplies the entire worst executable debit before admission.
// Implementations must bind the instrument, venue account, settlement regime,
// fee currency and maximum fee; a historical fee is insufficient.
type CapitalTerms interface {
	Debits(context.Context, *orderpb.OrderState, time.Time) ([]*commonpb.Money, error)
}

// SimPhysicalTerms is authoritative only for the in-process simulator. Its
// Execute path charges no fee and physically exchanges the canonical pair.
// Real adapters require their own verified terms before capital admission.
type SimPhysicalTerms struct {
	References *refdata.Cache
	Router     *execution.Router
}

func (s SimPhysicalTerms) Debits(_ context.Context, st *orderpb.OrderState, now time.Time) ([]*commonpb.Money, error) {
	if s.References == nil || s.Router == nil || st == nil {
		return nil, ErrCapitalTermsUnknown
	}
	venue, err := s.Router.Route(st)
	if err != nil {
		return nil, ErrCapitalTermsUnknown
	}
	if _, ok := venue.(*execution.SimVenue); !ok {
		return nil, ErrCapitalTermsUnknown
	}
	record, known := s.References.Lookup(st.GetInstrumentId(), now)
	if !known || record.AssetClass != "CRYPTO" && record.AssetClass != "FX" {
		return nil, ErrCapitalTermsUnknown
	}
	debits, err := capital.PhysicalDebits(st, record.BaseAsset, record.QuoteAsset, []*commonpb.Money{})
	if err != nil {
		return nil, ErrCapitalTermsUnknown
	}
	return debits, nil
}
