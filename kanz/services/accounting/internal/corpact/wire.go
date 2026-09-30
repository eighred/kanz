package corpact

import (
	"errors"
	"math/big"

	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// FromProto validates the canonical announcement without activating a feed.
// Portfolio scope comes from the authorized accounting boundary, not the vendor.
func FromProto(portfolio string, m *accountingpb.CorporateAction) (*ledger.Event, error) {
	if m == nil || m.GetExDate() == nil || m.GetAnnouncedAt() == nil || m.GetExDate().CheckValid() != nil || m.GetAnnouncedAt().CheckValid() != nil {
		return nil, errors.New("corpact: valid ex-date and knowledge time required")
	}
	if m.GetRatio() != 0 || m.GetDividendPerShare() != 0 || m.GetCouponRate() != 0 { //nolint:staticcheck // Inspect deprecated inputs only to reject ambiguous legacy economics.
		return nil, errors.New("corpact: legacy floating terms are ambiguous; exact periodic terms required")
	}
	c := CorporateAction{ActionID: m.GetActionId(), PortfolioID: portfolio, InstrumentID: m.GetInstrumentId(), Target: m.GetTargetInstrument(), Currency: m.GetCurrencyCode(), ExDate: m.GetExDate().AsTime(), AnnouncedAt: m.GetAnnouncedAt().AsTime(), ActionLifecycle: ledger.ActionLifecycle{Revision: m.GetAnnouncementRevision(), Cancelled: m.GetCancelled(), PaymentRef: m.GetPaymentRef()}}
	switch m.GetActionType() {
	case accountingpb.CorporateActionType_CORPORATE_ACTION_TYPE_SPLIT:
		c.Kind = Split
	case accountingpb.CorporateActionType_CORPORATE_ACTION_TYPE_DIVIDEND:
		c.Kind = Dividend
	case accountingpb.CorporateActionType_CORPORATE_ACTION_TYPE_MERGER:
		c.Kind = Merger
	case accountingpb.CorporateActionType_CORPORATE_ACTION_TYPE_COUPON:
		c.Kind = Coupon
	default:
		return nil, errors.New("corpact: unknown action type")
	}
	if m.GetPayDate() != nil {
		if err := m.GetPayDate().CheckValid(); err != nil {
			return nil, err
		}
		c.PayDate = m.GetPayDate().AsTime()
	}
	if m.GetPaidAt() != nil {
		if err := m.GetPaidAt().CheckValid(); err != nil {
			return nil, err
		}
		c.PaidAt = m.GetPaidAt().AsTime()
	}
	var err error
	if c.Ratio, err = exact(m.GetExactRatio()); err != nil {
		return nil, err
	}
	if c.Kind == Dividend || c.Kind == Coupon {
		if m.GetCashPerShare() != nil || m.GetExactRatio() != nil || m.GetTargetInstrument() != "" {
			return nil, errors.New("corpact: unexpected income conversion terms")
		}
		c.PerUnit, err = exact(m.GetIncomePerUnit())
	} else {
		if m.GetIncomePerUnit() != nil {
			return nil, errors.New("corpact: income terms on conversion")
		}
		c.PerUnit, err = exact(m.GetCashPerShare())
	}
	if err != nil {
		return nil, err
	}
	return c.ToEntry()
}

func exact(d *commonpb.Decimal) (*big.Rat, error) {
	if d == nil {
		return nil, nil
	}
	r, ok := dec.FromProtoChecked(d)
	if !ok {
		return nil, errors.New("corpact: invalid decimal exponent")
	}
	return r, nil
}
