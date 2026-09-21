package fillfact

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/dualcontrol"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

var ErrFeeRevision = errors.New("execution fee revision lacks matching approved evidence")

// EconomicDigest identifies exact execution economics independently of decimal
// encoding, transport aliases, and recovery metadata. A push and a historical
// query may encode the same price differently without changing what was traded.
func EconomicDigest(f *orderpb.Fill) (string, error) {
	if f == nil {
		return "", ErrExecutionIdentityConflict
	}
	if _, ok := dec.InDomainDeep(f); !ok {
		return "", ErrExecutionIdentityConflict
	}
	if f.GetOrderId() == "" || ExecutionKey(f) == f.GetFillId() || f.GetQuantity() == nil || f.GetPrice() == nil || f.GetFee().GetAmount() == nil || f.GetFee().GetCurrencyCode() == "" || f.GetExecutedAt() == nil || f.GetExecutedAt().CheckValid() != nil {
		return "", ErrExecutionIdentityConflict
	}
	if !dec.IsPositive(f.Quantity) || !dec.IsPositive(f.Price) || f.ExecutedAt.AsTime().Unix() <= 0 || (f.Side != orderpb.Side_SIDE_BUY && f.Side != orderpb.Side_SIDE_SELL) {
		return "", ErrExecutionIdentityConflict
	}
	return dualcontrol.Digest("execution-economics-v1", f.OrderId, ExecutionKey(f), f.Side.String(), dec.FromProto(f.Quantity).RatString(), dec.FromProto(f.Price).RatString(), dec.FromProto(f.Fee.Amount).RatString(), f.Fee.CurrencyCode, f.ExecutedAt.AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00")), nil
}

// SameExecutionExceptFee permits neither a trade amendment nor a currency
// substitution. Those require a different, explicitly modeled correction.
func SameExecutionExceptFee(previous, revised *orderpb.Fill) bool {
	if previous == nil || revised == nil || previous.GetFee().GetAmount() == nil || revised.GetFee().GetAmount() == nil || previous.GetFee().GetCurrencyCode() == "" || previous.GetFee().GetCurrencyCode() != revised.GetFee().GetCurrencyCode() {
		return false
	}
	a := proto.Clone(previous).(*orderpb.Fill)
	b := proto.Clone(revised).(*orderpb.Fill)
	a.Fee = nil
	b.Fee = nil
	return SameExecution(a, b)
}

// FeeRevisionTerms validates the proof carried by an OMS-authorized recovery
// FACT. Command authentication and the dual-control claim remain OMS's job;
// books additionally bind the revision to their own immutable execution head.
func FeeRevisionTerms(revised *orderpb.Fill) (*orderpb.Fill, error) {
	p := revised.GetRecovery()
	a := p.GetFeeApproval()
	if a == nil || a.GetProposalId() != p.GetCaseId() || a.GetProposalId() == "" || strings.TrimSpace(a.GetProposer()) == "" || strings.TrimSpace(a.GetApprover()) == "" || dualcontrol.SameSubject(a.GetProposer(), a.GetApprover()) || a.GetApprovedAt() == nil || a.GetApprovedAt().CheckValid() != nil || a.GetApprovedAt().AsTime().Unix() <= 0 {
		return nil, ErrFeeRevision
	}
	for _, digest := range []string{a.GetDigest(), a.GetPreviousExecutionDigest()} {
		b, err := hex.DecodeString(digest)
		if err != nil || len(b) != 32 {
			return nil, ErrFeeRevision
		}
	}
	if _, ok := dec.InDomainDeep(revised); !ok {
		return nil, ErrFeeRevision
	}
	if len(a.Proposer) > 256 || len(a.Approver) > 256 || len(a.ProposalId) > 256 {
		return nil, ErrFeeRevision
	}
	previous := proto.Clone(revised).(*orderpb.Fill)
	if a.GetPreviousFee() == nil {
		return nil, ErrFeeRevision
	}
	previous.Fee = proto.Clone(a.PreviousFee).(*commonpb.Money)
	previous.Recovery = nil
	if !SameExecutionExceptFee(previous, revised) {
		return nil, ErrFeeRevision
	}
	if dec.Cmp(previous.Fee.Amount, revised.Fee.Amount) == 0 {
		return nil, ErrFeeRevision
	}
	digest, err := EconomicDigest(previous)
	if err != nil || digest != a.GetPreviousExecutionDigest() {
		return nil, ErrFeeRevision
	}
	return previous, nil
}

// ApprovedFeeDelta is the append-only cash adjustment; it never reverses a
// quantity or rewrites the original trade. A replay against the revised head
// is detected by the caller before invoking this transition.
func ApprovedFeeDelta(previous, revised *orderpb.Fill) (*big.Rat, error) {
	approvedPrevious, err := FeeRevisionTerms(revised)
	if err != nil || !SameExecution(previous, approvedPrevious) {
		return nil, ErrFeeRevision
	}
	return new(big.Rat).Sub(dec.FromProto(previous.GetFee().GetAmount()), dec.FromProto(revised.GetFee().GetAmount())), nil
}

func FeeProposalDigest(tenant string, proposal *orderpb.ExecutionFeeCorrectionProposed) (string, error) {
	if tenant == "" || proposal == nil {
		return "", ErrFeeRevision
	}
	copy := proto.Clone(proposal).(*orderpb.ExecutionFeeCorrectionProposed)
	copy.Digest = ""
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(copy)
	if err != nil {
		return "", err
	}
	return dualcontrol.Digest(string(dualcontrol.ActExecutionFeeCorrection), tenant, string(data)), nil
}
