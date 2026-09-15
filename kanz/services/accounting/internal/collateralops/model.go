// Package collateralops owns durable collateral control beside the accounting
// book. It never substitutes internally calculated margin for venue facts.
package collateralops

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/collateral"
	"github.com/eighred/kanz/internal/dec"
	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

var (
	ErrInput       = errors.New("collateral workflow: invalid or incomplete input")
	ErrConflict    = errors.New("collateral workflow: revision or idempotency conflict")
	ErrNotFound    = errors.New("collateral workflow: not found")
	ErrTransition  = errors.New("collateral workflow: transition not permitted")
	ErrStale       = errors.New("collateral workflow: input snapshot expired")
	ErrUnsupported = errors.New("collateral workflow: allocation requires unsupported quantity precision; no instruction issued")
)

// AllocationProof retains the actual bounded program after subtracting other
// reservations. Releasing those reservations must not erase the decision inputs.
type AllocationProof struct {
	collateral.AllocationResult
	Assets       []collateral.Asset
	Requirements []collateral.Requirement
	Limits       []collateral.AllocationLimit
}

func identifier(s string) bool { return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s }
func exact(p *commonpb.Decimal, signed bool) (dec.Exact, error) {
	if p == nil {
		return "", ErrInput
	}
	r, ok := dec.FromProtoChecked(p)
	if !ok || (!signed && r.Sign() < 0) {
		return "", ErrInput
	}
	v, err := dec.ExactFromRat(r)
	if err != nil {
		return "", ErrInput
	}
	return v, nil
}
func rat(p *commonpb.Decimal) *big.Rat { r, _ := dec.FromProtoChecked(p); return r }
func wire(r *big.Rat) (*commonpb.Decimal, error) {
	v, ok := dec.ToProtoExact(r)
	if !ok {
		return nil, ErrUnsupported
	}
	return v, nil
}

func validateSnapshot(s *pb.WorkflowSnapshot) error {
	if s == nil || !identifier(s.SnapshotId) || !identifier(s.PortfolioId) || !identifier(s.PositionsVersion) || !identifier(s.ValuationsVersion) || !identifier(s.CashForecastVersion) || !identifier(s.ExposureModelVersion) || len(s.CurrencyCode) != 3 || len(s.Agreements) == 0 || len(s.Agreements) > 32 || len(s.Inventory) > 32 {
		return ErrInput
	}
	for _, c := range s.CurrencyCode {
		if c < 'A' || c > 'Z' {
			return ErrInput
		}
	}
	if s.AsOf == nil || s.ValidUntil == nil || s.AsOf.CheckValid() != nil || s.ValidUntil.CheckValid() != nil || !s.ValidUntil.AsTime().After(s.AsOf.AsTime()) || s.ValidUntil.AsTime().Sub(s.AsOf.AsTime()) > 24*time.Hour {
		return ErrInput
	}
	assets := map[string]bool{}
	lots := map[string]bool{}
	for _, l := range s.Inventory {
		if l == nil || !identifier(l.LotId) || lots[l.LotId] || !identifier(l.AssetId) || !identifier(l.IssuerId) || !identifier(l.CustodianId) || !identifier(l.AccountId) || l.AvailableAt == nil || l.AvailableAt.CheckValid() != nil {
			return ErrInput
		}
		lots[l.LotId] = true
		assets[l.AssetId] = true
		for _, v := range []*commonpb.Decimal{l.Quantity, l.UnitValue, l.QuantityIncrement, l.OpportunityCost, l.LiquidityBudget} {
			if _, err := exact(v, false); err != nil {
				return err
			}
		}
		if rat(l.UnitValue).Sign() <= 0 || rat(l.QuantityIncrement).Sign() <= 0 || !new(big.Rat).Quo(rat(l.Quantity), rat(l.QuantityIncrement)).IsInt() {
			return ErrInput
		}
	}
	agreements := map[string]bool{}
	for _, a := range s.Agreements {
		if a == nil || !identifier(a.AgreementId) || agreements[a.AgreementId] || !identifier(a.Version) || !identifier(a.CounterpartyId) || a.SettlementDeadline == nil || a.SettlementDeadline.CheckValid() != nil || a.SettlementDeadline.AsTime().Before(s.AsOf.AsTime()) || len(a.Schedule) > 32 {
			return ErrInput
		}
		agreements[a.AgreementId] = true
		if _, err := exact(a.Exposure, true); err != nil {
			return err
		}
		for _, v := range []*commonpb.Decimal{a.Held, a.InitialMargin, a.Threshold, a.MinimumTransfer, a.IndependentAmount, a.Rounding, a.IssuerLimit} {
			if _, err := exact(v, false); err != nil {
				return err
			}
		}
		seen := map[string]bool{}
		issuers := map[string]bool{}
		if len(a.IssuerHeadroom) > 32 {
			return ErrInput
		}
		for _, h := range a.IssuerHeadroom {
			if h == nil || !identifier(h.IssuerId) || issuers[h.IssuerId] {
				return ErrInput
			}
			issuers[h.IssuerId] = true
			if _, err := exact(h.Remaining, false); err != nil {
				return err
			}
		}
		for _, e := range a.Schedule {
			if e == nil || !assets[e.AssetId] || seen[e.AssetId] {
				return ErrInput
			}
			seen[e.AssetId] = true
			if _, err := exact(e.Haircut, false); err != nil {
				return err
			}
			if rat(e.Haircut).Cmp(big.NewRat(1, 1)) >= 0 {
				return ErrInput
			}
		}
	}
	return nil
}

// plan subtracts tenant-wide live reservations before solving all agreements
// together. Issuer concentration is on credited value; liquidity on market
// value. Unknown wrong-way clearance and late settlement exclude an edge.
func plan(ctx context.Context, s *pb.WorkflowSnapshot, reserved map[string]*big.Rat) ([]*pb.PostingLeg, AllocationProof, error) {
	if err := validateSnapshot(s); err != nil {
		return nil, AllocationProof{}, err
	}
	assets := []collateral.Asset{}
	requirements := []collateral.Requirement{}
	limits := []collateral.AllocationLimit{}
	lots := map[string]*pb.WorkflowInventory{}
	for _, l := range s.Inventory {
		available := rat(l.Quantity)
		available.Mul(available, rat(l.UnitValue))
		if available.Cmp(rat(l.LiquidityBudget)) > 0 {
			available.Set(rat(l.LiquidityBudget))
		}
		if v := reserved[l.LotId]; v != nil {
			available.Sub(available, new(big.Rat).Mul(v, rat(l.UnitValue)))
		}
		if available.Sign() < 0 {
			return nil, AllocationProof{}, ErrConflict
		}
		value, err := dec.ExactFromRat(available)
		if err != nil {
			return nil, AllocationProof{}, err
		}
		cost, _ := exact(l.OpportunityCost, false)
		assets = append(assets, collateral.Asset{ID: l.LotId, Currency: s.CurrencyCode, Available: value, Cost: cost})
		lots[l.LotId] = l
	}
	for _, a := range s.Agreements {
		exposure, _ := exact(a.Exposure, true)
		held, _ := exact(a.Held, false)
		im, _ := exact(a.InitialMargin, false)
		threshold, _ := exact(a.Threshold, false)
		mta, _ := exact(a.MinimumTransfer, false)
		ia, _ := exact(a.IndependentAmount, false)
		rounding, _ := exact(a.Rounding, false)
		margin, err := collateral.CalculateMargin(exposure, held, im, collateral.ExactCSATerms{Currency: s.CurrencyCode, Threshold: threshold, MinimumTransfer: mta, IndependentAmount: ia, Rounding: rounding})
		if err != nil {
			return nil, AllocationProof{}, err
		}
		need, _ := margin.Movement.Rat()
		// Existing collateral returns refer to settled workflow legs, never to a
		// fresh inventory lot invented from a negative margin calculation.
		if need.Sign() < 0 {
			return nil, AllocationProof{}, ErrUnsupported
		}
		r := collateral.Requirement{AgreementID: a.AgreementId, Currency: s.CurrencyCode, Amount: margin.Movement, Schedule: map[string]collateral.Eligibility{}}
		byIssuer := map[string][]collateral.AllocationLimitTerm{}
		for _, l := range s.Inventory {
			for _, e := range a.Schedule {
				if e.AssetId != l.AssetId || !e.Eligible || !e.WrongWayRiskCleared || l.IssuerId == a.CounterpartyId || l.AvailableAt.AsTime().After(a.SettlementDeadline.AsTime()) {
					continue
				}
				haircut, _ := exact(e.Haircut, false)
				r.Schedule[l.LotId] = collateral.Eligibility{Eligible: true, Haircut: haircut}
				weight, _ := dec.ExactFromRat(new(big.Rat).Sub(big.NewRat(1, 1), rat(e.Haircut)))
				byIssuer[l.IssuerId] = append(byIssuer[l.IssuerId], collateral.AllocationLimitTerm{AssetID: l.LotId, AgreementID: a.AgreementId, Weight: weight})
			}
		}
		for issuer, terms := range byIssuer {
			maximum, _ := exact(a.IssuerLimit, false)
			matched := false
			for _, headroom := range a.IssuerHeadroom {
				if headroom.IssuerId == issuer {
					matched = true
					if rat(headroom.Remaining).Cmp(rat(a.IssuerLimit)) < 0 {
						maximum, _ = exact(headroom.Remaining, false)
					}
				}
			}
			if need.Sign() > 0 && rat(a.Held).Sign() > 0 && !matched {
				return nil, AllocationProof{}, ErrInput
			}
			identity, _ := json.Marshal([]string{a.AgreementId, issuer})
			id := "issuer/" + digest(identity)
			limits = append(limits, collateral.AllocationLimit{ID: id, Maximum: maximum, Terms: terms})
		}
		requirements = append(requirements, r)
	}
	solution, err := collateral.OptimizeConstrained(ctx, assets, requirements, limits)
	result := AllocationProof{AllocationResult: solution, Assets: assets, Requirements: requirements, Limits: limits}
	if err != nil || !result.Feasible {
		return nil, result, err
	}
	legs := []*pb.PostingLeg{}
	for _, a := range result.Allocations {
		l := lots[a.AssetID]
		value, _ := a.UsedValue.Rat()
		credited, _ := a.PostedValue.Rat()
		quantity := new(big.Rat).Quo(value, rat(l.UnitValue))
		if !new(big.Rat).Quo(quantity, rat(l.QuantityIncrement)).IsInt() {
			return nil, AllocationProof{}, ErrUnsupported
		}
		q, e1 := wire(quantity)
		v, e2 := wire(value)
		c, e3 := wire(credited)
		if e1 != nil || e2 != nil || e3 != nil {
			return nil, AllocationProof{}, ErrUnsupported
		}
		legs = append(legs, &pb.PostingLeg{AgreementId: a.AgreementID, LotId: a.AssetID, CustodianId: l.CustodianId, AccountId: l.AccountId, Quantity: q, MarketValue: v, CreditedValue: c})
	}
	return legs, result, nil
}
