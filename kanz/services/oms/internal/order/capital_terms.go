package order

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/refdata"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

var ErrCapitalTermsUnknown = errors.New("oms: settlement or maximum fee terms unknown")

// CapitalTerms supplies the entire worst executable debit before admission.
// Implementations must bind the instrument, venue account, settlement regime,
// fee currency and maximum fee; a historical fee is insufficient.
type CapitalTerms interface {
	Debits(context.Context, *orderpb.OrderState, time.Time) ([]*commonpb.Money, error)
}

// SimPhysicalTerms uses the simulator's zero-fee contract and resolves real
// adapters through their exchange evidence plus independent approved fee policy.
// An absent policy leaves real admission closed.
type SimPhysicalTerms struct {
	References   *refdata.Cache
	Router       *execution.Router
	ApprovedFees ApprovedMaximumFeeAuthority
}

// ApprovedMaximumFee is a policy authority, never a venue observation. Its
// regime binds the approved worst-case amounts to one account and order shape.
// Source and version identify the approval artifact used for this decision.
type ApprovedMaximumFee struct {
	Venue, Account, InstrumentID, VenueSymbol string
	Side                                      orderpb.Side
	OrderType                                 orderpb.OrderType
	TimeInForce                               orderpb.TimeInForce
	Source, Version, ApprovedBy               string
	Fees                                      []*commonpb.Money
}

type ApprovedMaximumFeeAuthority interface {
	MaximumFee(context.Context, *orderpb.OrderState, string) (ApprovedMaximumFee, error)
}

type capitalEvidenceReader interface {
	ReadCapitalEvidence(context.Context, string) (*venuepb.ReadCapitalEvidenceResponse, error)
}

func (s SimPhysicalTerms) Debits(ctx context.Context, st *orderpb.OrderState, now time.Time) ([]*commonpb.Money, error) {
	if s.References == nil || s.Router == nil || st == nil {
		return nil, ErrCapitalTermsUnknown
	}
	venue, err := s.Router.Route(st)
	if err != nil {
		return nil, ErrCapitalTermsUnknown
	}
	record, known := s.References.Lookup(st.GetInstrumentId(), now)
	if !known || record.AssetClass != "CRYPTO" && record.AssetClass != "FX" {
		return nil, ErrCapitalTermsUnknown
	}
	var fees []*commonpb.Money
	if _, ok := venue.(*execution.SimVenue); ok {
		fees = []*commonpb.Money{}
	} else {
		reader, ok := venue.(capitalEvidenceReader)
		if !ok || s.ApprovedFees == nil || st.GetVenueAccountId() != venue.Account() {
			return nil, ErrCapitalTermsUnknown
		}
		evidence, err := reader.ReadCapitalEvidence(ctx, st.GetInstrumentId())
		if err != nil || evidence == nil || evidence.GetBaseAsset() != record.BaseAsset || evidence.GetQuoteAsset() != record.QuoteAsset ||
			evidence.GetSettlementSourceVersion().GetSource() == "" || evidence.GetSettlementSourceVersion().GetContentSha256() == "" ||
			evidence.GetCurrentFee().GetSourceVersion().GetSource() == "" || evidence.GetCurrentFee().GetSourceVersion().GetContentSha256() == "" {
			return nil, ErrCapitalTermsUnknown
		}
		policy, err := s.ApprovedFees.MaximumFee(ctx, st, evidence.GetVenueSymbol())
		if err != nil || policy.Venue != venue.MIC() || policy.Account != venue.Account() || policy.InstrumentID != st.GetInstrumentId() ||
			policy.VenueSymbol != evidence.GetVenueSymbol() || policy.Side != st.GetSide() || policy.OrderType != st.GetOrderType() ||
			policy.TimeInForce != st.GetTimeInForce() || strings.TrimSpace(policy.Source) == "" || strings.TrimSpace(policy.Version) == "" ||
			strings.TrimSpace(policy.ApprovedBy) == "" || policy.Fees == nil {
			return nil, ErrCapitalTermsUnknown
		}
		fees = policy.Fees
	}
	debits, err := capital.PhysicalDebits(st, record.BaseAsset, record.QuoteAsset, fees)
	if err != nil {
		return nil, ErrCapitalTermsUnknown
	}
	return debits, nil
}
