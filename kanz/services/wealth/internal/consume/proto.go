package consume

import (
	"fmt"

	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/wealth"
)

// DecodeProto is the wealth.v1.HouseholdValued Decoder. Unlike alternatives,
// which needs a factory keyed on subject because four subjects each carry a
// different message, wealth has exactly one subject (SubjectHouseholdAll) and
// one message type — so this is a single Decoder, not a factory.
//
// It replaces DecodeJSON on the live path. DecodeJSON decodes the Go-native
// wealth.Household as JSON, which is not what a HouseholdValued FACT carries —
// it was a placeholder for exactly this, and consume.go's own doc says so.
//
// # Decimal crosses to float64 here, by design
//
// internal/wealth is float64 BY DESIGN (household.go's own doc: money is exact
// Decimal at the wire, float at the analytics edge — EVT-14). So every
// common.v1.Decimal below is decoded with dec.FromProtoChecked — NOT FromProto
// — because this is untrusted wire input: an exponent outside the
// representable range is not a small number, it is one this platform cannot
// hold, and coercing it to zero would fold a household holding worth nothing
// into the book while reporting success. FromProtoChecked's *big.Rat is then
// converted to float64 via Float64() to land at the analytics-edge shape
// internal/wealth expects. Do NOT change internal/wealth to big.Rat — that
// package's float64 stance is deliberate and documented.
func DecodeProto(payload []byte) (wealth.Household, error) {
	var m wealthpb.HouseholdValued
	if err := proto.Unmarshal(payload, &m); err != nil {
		return wealth.Household{}, fmt.Errorf("consume: HouseholdValued decode: %w", err)
	}

	accounts := make([]wealth.Account, 0, len(m.GetAccounts()))
	for _, va := range m.GetAccounts() {
		cash, ok := dec.FromProtoChecked(va.GetCash())
		if !ok {
			return wealth.Household{}, fmt.Errorf(
				"consume: household %s account %s carries a cash balance this platform "+
					"cannot represent exactly; refusing rather than rounding it",
				m.GetHouseholdId(), va.GetAccountId())
		}
		cashF, _ := cash.Float64()

		holdings := make([]wealth.Holding, 0, len(va.GetHoldings()))
		for _, vh := range va.GetHoldings() {
			mv, ok := dec.FromProtoChecked(vh.GetMarketValue())
			if !ok {
				return wealth.Household{}, fmt.Errorf(
					"consume: household %s account %s holding %s carries a market value "+
						"this platform cannot represent exactly; refusing rather than "+
						"rounding it",
					m.GetHouseholdId(), va.GetAccountId(), vh.GetInstrumentId())
			}
			mvF, _ := mv.Float64()
			holdings = append(holdings, wealth.Holding{
				InstrumentID: vh.GetInstrumentId(),
				AssetClass:   vh.GetAssetClass(),
				MarketValue:  mvF,
			})
		}

		accounts = append(accounts, wealth.Account{
			AccountID: va.GetAccountId(),
			Holdings:  holdings,
			Cash:      cashF,
		})
	}

	return wealth.Household{
		HouseholdID: m.GetHouseholdId(),
		Accounts:    accounts,
	}, nil
}
