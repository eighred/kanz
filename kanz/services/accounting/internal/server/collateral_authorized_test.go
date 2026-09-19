package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/collateralops"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresCollateralHTTPRequiresEntitledIndependentActors(t *testing.T) {
	pool := newServerPool(t)
	store := collateralops.New(pool)
	now := time.Now().UTC().Add(-time.Minute)
	stamp := timestamppb.New(now)
	end := timestamppb.New(now.Add(time.Hour))
	n := func(v int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: v} }
	input := &pb.WorkflowSnapshot{SnapshotId: "snapshot", PortfolioId: "PF", CurrencyCode: "USD", PositionsVersion: "p1", ValuationsVersion: "v1", CashForecastVersion: "c1", ExposureModelVersion: "m1", AsOf: stamp, ValidUntil: end,
		Inventory:  []*pb.WorkflowInventory{{LotId: "lot", AssetId: "cash", IssuerId: "issuer", CustodianId: "custody", AccountId: "account", Quantity: n(100), UnitValue: n(1), QuantityIncrement: n(1), OpportunityCost: n(1), LiquidityBudget: n(100), AvailableAt: stamp}},
		Agreements: []*pb.WorkflowAgreement{{AgreementId: "CSA", Version: "1", CounterpartyId: "CP", Exposure: n(60), Held: n(0), InitialMargin: n(0), Threshold: n(0), MinimumTransfer: n(0), IndependentAmount: n(0), Rounding: n(0), IssuerLimit: n(100), SettlementDeadline: end, Schedule: []*pb.WorkflowEligibility{{AssetId: "cash", Eligible: true, WrongWayRiskCleared: true, Haircut: n(0)}}}},
	}
	if err := store.ImportSnapshot(t.Context(), "__system__", input); err != nil {
		t.Fatal(err)
	}
	s := New(&Readiness{}, nil, ledger.NewPostgres(pool), "USD", WithTenant("__system__"), WithCollateral(store))
	call := func(actor, portfolio, body string) int {
		r := httptest.NewRequest("POST", "/v1/portfolios/PF/collateral/actions", strings.NewReader(body))
		auth.SetPrincipalHeaders(r.Header, actor, "__system__", nil)
		if err := auth.SetPrincipalPortfolios(r.Header, []string{portfolio}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	proposal := `{"request_id":"proposal","workflow_id":"workflow","snapshot_id":"snapshot","action":"propose","expected_revision":"0"}`
	if status := call("maker", "OTHER", proposal); status != 404 {
		t.Fatalf("unentitled proposal: %d", status)
	}
	if status := call("maker", "PF", proposal); status != 200 {
		t.Fatalf("entitled proposal: %d", status)
	}
	approval := `{"request_id":"approval","workflow_id":"workflow","action":"approve","expected_revision":"1"}`
	if status := call("maker", "PF", approval); status != 409 {
		t.Fatalf("self approval: %d", status)
	}
	if status := call("checker", "PF", approval); status != 200 {
		t.Fatalf("independent approval: %d", status)
	}
	state, err := store.Load(t.Context(), "__system__", "workflow")
	if err != nil || state.Status != pb.WorkflowStatus_WORKFLOW_STATUS_INSTRUCTED || state.Maker != "maker" || state.Actor != "checker" {
		t.Fatalf("identity/state lost: %v %+v", err, state)
	}
}
