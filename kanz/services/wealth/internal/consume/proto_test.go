package consume

import (
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDecodeProto_TwoAccounts(t *testing.T) {
	body, err := proto.Marshal(&wealthpb.HouseholdValued{
		HouseholdId: "HH-1",
		Accounts: []*wealthpb.ValuedAccount{
			{
				AccountId: "A-1",
				Holdings: []*wealthpb.ValuedHolding{
					{
						InstrumentId: "VTI",
						AssetClass:   "EQUITY",
						MarketValue:  &commonpb.Decimal{Coefficient: 9500000, Exponent: -2},
					},
				},
				Cash: &commonpb.Decimal{Coefficient: 500000, Exponent: -2},
			},
			{
				AccountId: "A-2",
				Holdings: []*wealthpb.ValuedHolding{
					{
						InstrumentId: "BND",
						AssetClass:   "FIXED_INCOME",
						MarketValue:  &commonpb.Decimal{Coefficient: 2000000, Exponent: -2},
					},
				},
				Cash: &commonpb.Decimal{Coefficient: 100000, Exponent: -2},
			},
		},
		AsOf:         timestamppb.Now(),
		CurrencyCode: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}

	h, err := DecodeProto(body)
	if err != nil {
		t.Fatalf("DecodeProto: %v", err)
	}
	if h.HouseholdID != "HH-1" {
		t.Fatalf("household id = %q, want HH-1", h.HouseholdID)
	}
	if len(h.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(h.Accounts))
	}

	a1 := h.Accounts[0]
	if a1.AccountID != "A-1" {
		t.Fatalf("account 0 id = %q, want A-1", a1.AccountID)
	}
	if a1.Cash != 5000 {
		t.Fatalf("account 0 cash = %v, want 5000", a1.Cash)
	}
	if len(a1.Holdings) != 1 {
		t.Fatalf("account 0 holdings = %d, want 1", len(a1.Holdings))
	}
	if a1.Holdings[0].InstrumentID != "VTI" || a1.Holdings[0].AssetClass != "EQUITY" || a1.Holdings[0].MarketValue != 95000 {
		t.Fatalf("account 0 holding = %+v, want VTI/EQUITY/95000", a1.Holdings[0])
	}

	a2 := h.Accounts[1]
	if a2.AccountID != "A-2" {
		t.Fatalf("account 1 id = %q, want A-2", a2.AccountID)
	}
	if a2.Cash != 1000 {
		t.Fatalf("account 1 cash = %v, want 1000", a2.Cash)
	}
	if len(a2.Holdings) != 1 {
		t.Fatalf("account 1 holdings = %d, want 1", len(a2.Holdings))
	}
	if a2.Holdings[0].InstrumentID != "BND" || a2.Holdings[0].AssetClass != "FIXED_INCOME" || a2.Holdings[0].MarketValue != 20000 {
		t.Fatalf("account 1 holding = %+v, want BND/FIXED_INCOME/20000", a2.Holdings[0])
	}
}

// AN UNREPRESENTABLE MARKET VALUE MUST BE REFUSED, NOT COERCED.
//
// dec.FromProtoChecked exists precisely because this payload is untrusted wire
// input: an exponent outside +/-64 is not a small number, it is one this
// platform cannot represent, and substituting zero would fold a household
// holding worth nothing into the book while reporting success.
func TestDecodeProto_RefusesUnrepresentableMarketValue(t *testing.T) {
	body, err := proto.Marshal(&wealthpb.HouseholdValued{
		HouseholdId: "HH-1",
		Accounts: []*wealthpb.ValuedAccount{
			{
				AccountId: "A-1",
				Holdings: []*wealthpb.ValuedHolding{
					{
						InstrumentId: "VTI",
						AssetClass:   "EQUITY",
						MarketValue:  &commonpb.Decimal{Coefficient: 1, Exponent: 9999},
					},
				},
				Cash: &commonpb.Decimal{Coefficient: 0, Exponent: 0},
			},
		},
		AsOf:         timestamppb.Now(),
		CurrencyCode: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := DecodeProto(body); err == nil {
		t.Fatal("an unrepresentable market value decoded without error — a holding " +
			"whose value we cannot represent must be refused, never silently zeroed")
	}
}

// wealth.v1.HouseholdValued now carries recorded_by/reason (provenance — see
// wealth.proto). The fold serves the exposure view, not audit trails:
// provenance reaches durable storage via the bus, exactly as
// lifecycle.v1.ConfigChanged.changed_by is never retained by the mandate
// registry. This is a regression guard proving the decoder still succeeds,
// and internal/wealth.Household stays unwidened, when a message carries
// those fields.
func TestDecodeProto_IgnoresProvenanceFields(t *testing.T) {
	body, err := proto.Marshal(&wealthpb.HouseholdValued{
		HouseholdId: "HH-1",
		Accounts: []*wealthpb.ValuedAccount{
			{
				AccountId: "A-1",
				Holdings: []*wealthpb.ValuedHolding{
					{
						InstrumentId: "VTI",
						AssetClass:   "EQUITY",
						MarketValue:  &commonpb.Decimal{Coefficient: 9500000, Exponent: -2},
					},
				},
				Cash: &commonpb.Decimal{Coefficient: 500000, Exponent: -2},
			},
		},
		AsOf:         timestamppb.Now(),
		CurrencyCode: "USD",
		RecordedBy:   "operator:akif",
		Reason:       "Q2 custodial statement",
	})
	if err != nil {
		t.Fatal(err)
	}

	h, err := DecodeProto(body)
	if err != nil {
		t.Fatalf("DecodeProto: %v", err)
	}
	if h.HouseholdID != "HH-1" {
		t.Fatalf("household id = %q, want HH-1", h.HouseholdID)
	}
	if len(h.Accounts) != 1 || h.Accounts[0].Cash != 5000 {
		t.Fatalf("accounts = %+v, want one account with cash 5000", h.Accounts)
	}
}

func TestDecodeProto_RefusesUnrepresentableCash(t *testing.T) {
	body, err := proto.Marshal(&wealthpb.HouseholdValued{
		HouseholdId: "HH-1",
		Accounts: []*wealthpb.ValuedAccount{
			{
				AccountId: "A-1",
				Cash:      &commonpb.Decimal{Coefficient: 1, Exponent: -9999},
			},
		},
		AsOf:         timestamppb.Now(),
		CurrencyCode: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := DecodeProto(body); err == nil {
		t.Fatal("an unrepresentable cash balance decoded without error — refuse, never zero it")
	}
}
