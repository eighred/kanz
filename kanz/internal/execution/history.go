package execution

import (
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

var ErrHistoryIncomplete = errors.New("execution: history is not complete and attributable")

// CompleteHistory validates the entire observation before returning any execution.
// Aggregate quantity alone never creates a fill. Exact duplicate rows contribute
// once; conflicting identities refuse the observation rather than pick a winner.
// Returned fills are detached and ordered by economic time for deterministic replay.
func CompleteHistory(st *orderpb.OrderState, view OrderView) ([]*orderpb.Fill, error) {
	fail := func(reason string) ([]*orderpb.Fill, error) {
		return nil, fmt.Errorf("%w: %s", ErrHistoryIncomplete, reason)
	}
	if st.GetOrderId() == "" || st.GetPortfolioId() == "" || st.GetInstrumentId() == "" || st.GetVenue() == "" || st.GetVenueAccountId() == "" {
		return fail("order account scope is missing")
	}
	if st.GetSide() != orderpb.Side_SIDE_BUY && st.GetSide() != orderpb.Side_SIDE_SELL {
		return fail("order side is unknown")
	}
	if _, ok := dec.InDomainDeep(st); !ok {
		return fail("order decimal is outside the supported domain")
	}
	switch view.State {
	case OrderViewWorking, OrderViewPartiallyFilled, OrderViewFilled, OrderViewRejected, OrderViewCancelled, OrderViewExpired:
	default:
		return fail("venue did not establish an order state")
	}
	if view.ExecutedQuantity == nil {
		return fail("independent executed quantity is missing")
	}
	if _, ok := dec.InDomainDeep(view.ExecutedQuantity); !ok {
		return fail("executed quantity is outside the supported domain")
	}
	total := dec.FromProto(view.ExecutedQuantity)
	if total.Sign() < 0 || total.Cmp(dec.FromProto(st.GetOrderedQuantity())) > 0 {
		return fail("executed quantity exceeds the admitted order")
	}
	if total.Cmp(dec.FromProto(st.GetFilledQuantity())) < 0 {
		return fail("venue total contradicts already booked quantity")
	}
	if (view.State == OrderViewFilled && total.Cmp(dec.FromProto(st.GetOrderedQuantity())) != 0) ||
		(view.State == OrderViewPartiallyFilled && (total.Sign() <= 0 || total.Cmp(dec.FromProto(st.GetOrderedQuantity())) >= 0)) ||
		((view.State == OrderViewWorking || view.State == OrderViewRejected) && total.Sign() != 0) {
		return fail("venue state contradicts executed quantity")
	}
	if len(view.Fills) > 8000 {
		return fail("execution count exceeds recovery bound")
	}
	byExecution := make(map[string]*orderpb.Fill, len(view.Fills))
	byFill := make(map[string]string, len(view.Fills))
	result := make([]*orderpb.Fill, 0, len(view.Fills))
	sum := new(big.Rat)
	for _, f := range view.Fills {
		if f == nil {
			return fail("nil execution")
		}
		if _, ok := dec.InDomainDeep(f); !ok {
			return fail("execution decimal is outside the supported domain")
		}
		if err := fillfact.Validate(f); err != nil {
			return fail(err.Error())
		}
		if f.GetOrderId() != st.GetOrderId() || f.GetInstrumentId() != st.GetInstrumentId() || f.GetVenue() != st.GetVenue() || f.GetVenueAccountId() != st.GetVenueAccountId() || f.GetSide() != st.GetSide() {
			return fail("execution does not belong to the proven order scope")
		}
		if f.GetVenueExecutionId() == "" || len(f.GetVenueExecutionId()) > 256 || len(f.GetFillId()) > 512 || f.GetRecovery() != nil {
			return fail("execution identity is missing or supplied recovery provenance is untrusted")
		}
		if f.GetPrice() == nil || dec.FromProto(f.GetPrice()).Sign() <= 0 || f.GetFee().GetAmount() == nil || f.GetFee().GetCurrencyCode() == "" {
			return fail("exact execution price or fee currency is missing")
		}
		if f.GetExecutedAt() == nil || f.GetExecutedAt().CheckValid() != nil || f.GetExecutedAt().AsTime().Unix() <= 0 {
			return fail("venue execution time is missing or invalid")
		}
		if previous, exists := byExecution[f.GetVenueExecutionId()]; exists {
			if !fillfact.SameExecution(previous, f) {
				return fail("one execution identity carries conflicting economics")
			}
			continue
		}
		if previous, exists := byFill[f.GetFillId()]; exists && previous != f.GetVenueExecutionId() {
			return fail("one fill identity names different executions")
		}
		copy := proto.Clone(f).(*orderpb.Fill)
		byExecution[f.GetVenueExecutionId()] = copy
		byFill[f.GetFillId()] = f.GetVenueExecutionId()
		result = append(result, copy)
		sum.Add(sum, dec.FromProto(f.GetQuantity()))
	}
	if sum.Cmp(total) != 0 {
		return fail("unique executions do not reconcile to the independent total")
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		at, bt := a.GetExecutedAt().AsTime(), b.GetExecutedAt().AsTime()
		if at.Equal(bt) {
			return a.GetVenueExecutionId() < b.GetVenueExecutionId()
		}
		return at.Before(bt)
	})
	return result, nil
}
