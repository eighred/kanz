package consume

import (
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// THE FOLD MUST CARRY THE EXCHANGE ACCOUNT THE FACT DECLARED (#415).
//
// decodeCash unmarshals a full accounting.v1.LedgerEntry — venue_account_id
// included, it is field 12 — and built a ledger.Event that omitted it. So even
// once the producer stamps the account, the consumer would drop it on the floor
// and ledger_entries.venue_account_id would be '' for every cash row.
//
// '' IS NOT "UNKNOWN" HERE. Migration 0003 makes it a POSITIVE DECLARATION that
// the entry touched no exchange account, and the write-guard refuses a row whose
// transaction did not declare one. So dropping the field does not lose
// information quietly — it asserts something false, in the append-only book of
// record, on every funded cash movement.
//
// The FILL path has always carried it (ledger/fill.go). This is the same
// journal, the same column, and the sibling decoder in the same file.

// cashPayloadWithAccount is cashPayload plus the venue account, kept separate so
// the existing fixtures keep proving the unscoped case.
func cashPayloadWithAccount(t *testing.T, entryID, portfolioID, account string,
	entryType accountingpb.EntryType, cash *commonpb.Decimal, ccy string, eff time.Time) []byte {
	t.Helper()
	le := &accountingpb.LedgerEntry{
		EntryId:        entryID,
		PortfolioId:    portfolioID,
		VenueAccountId: account,
		EntryType:      entryType,
		Cash:           cash,
		CashCurrency:   ccy,
		EffectiveTime:  timestamppb.New(eff),
		KnowledgeTime:  timestamppb.New(eff),
	}
	b, err := proto.Marshal(le)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestDecodeCashCarriesTheVenueAccount(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	payload := cashPayloadWithAccount(t, "cash:S1", "PORT-1", "okx-sub-1",
		accountingpb.EntryType_ENTRY_TYPE_CASH, decv(100, 0), "USDT", t0)

	ev, err := decodeCash(cashEventSubscription, payload, t0)
	if err != nil {
		t.Fatalf("decodeCash: %v", err)
	}
	if ev == nil {
		t.Fatal("decodeCash returned no event for a subscription")
	}
	if ev.VenueAccountID != "okx-sub-1" {
		t.Fatalf("VenueAccountID = %q, want %q.\n"+
			"The FACT declared the account and the fold dropped it, so the row lands with '' — "+
			"which migration 0003 reads as the positive claim that this cash touched no exchange "+
			"account. Dropping the field writes a falsehood, it does not merely lose detail.",
			ev.VenueAccountID, "okx-sub-1")
	}
}

// AN UNSCOPED MOVEMENT STAYS UNSCOPED. Without this, "carry the account" is
// satisfied by defaulting it to something, which would post an investor
// subscription against collateral it never reached.
func TestDecodeCashLeavesAnUnscopedMovementEmpty(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	payload := cashPayload(t, "cash:S2", "PORT-1",
		accountingpb.EntryType_ENTRY_TYPE_CASH, decv(100, 0), "USD", t0)

	ev, err := decodeCash(cashEventSubscription, payload, t0)
	if err != nil || ev == nil {
		t.Fatalf("decodeCash: ev=%v err=%v", ev, err)
	}
	if ev.VenueAccountID != "" {
		t.Fatalf("VenueAccountID = %q, want empty for a movement that declared no account",
			ev.VenueAccountID)
	}
}
