// Command collateral_workflow supplies disposable input and checks the normal
// gateway and broker interfaces of the released Accounting process.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/devtoken"
	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/platform/subject"
	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	common "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelope "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const tenant = "rehearsal"
const portfolio = "PF1"
const workflow = "release-workflow"

func number(n int64) *common.Decimal { return &common.Decimal{Coefficient: n} }

func snapshot(at time.Time) *pb.WorkflowSnapshot {
	asOf := timestamppb.New(at)
	return &pb.WorkflowSnapshot{
		SnapshotId: "snapshot-1", PortfolioId: portfolio, CurrencyCode: "USD",
		PositionsVersion: "positions-1", ValuationsVersion: "valuation-1",
		CashForecastVersion: "cash-1", ExposureModelVersion: "model-1",
		AsOf: asOf, ValidUntil: timestamppb.New(at.Add(time.Hour)),
		Inventory: []*pb.WorkflowInventory{{
			LotId: "lot-1", AssetId: "cash", IssuerId: "issuer", CustodianId: "custodian", AccountId: "account",
			Quantity: number(100), UnitValue: number(1), QuantityIncrement: number(1), OpportunityCost: number(1),
			LiquidityBudget: number(100), AvailableAt: asOf,
		}},
		Agreements: []*pb.WorkflowAgreement{{
			AgreementId: "CSA1", Version: "v1", CounterpartyId: "counterparty", Exposure: number(60), Held: number(0),
			InitialMargin: number(0), Threshold: number(0), MinimumTransfer: number(0), IndependentAmount: number(0),
			Rounding: number(0), IssuerLimit: number(100), SettlementDeadline: timestamppb.New(at.Add(2 * time.Hour)),
			Schedule: []*pb.WorkflowEligibility{{AssetId: "cash", Eligible: true, WrongWayRiskCleared: true, Haircut: number(0)}},
		}},
	}
}

func publish(ctx context.Context, kind string) error {
	at, err := time.Parse(time.RFC3339Nano, os.Getenv("EVENT_TIME"))
	if err != nil {
		return errors.New("invalid fixture time")
	}
	c, err := bus.DialNATS(ctx, bus.NATSConfig{URL: os.Getenv("NATS_URL"), Name: "collateral-rehearsal"})
	if err != nil {
		return errors.New("fixture broker connection failed")
	}
	defer func() { _ = c.Close() }()
	source, eventType := "feed", "accounting.collateral.snapshot"
	var payload proto.Message = snapshot(at)
	route, key, schema := eventType, portfolio, "collateral.v1.WorkflowSnapshot:1"
	if kind == "confirm" {
		source, eventType = "custody", "accounting.collateral.confirmation"
		route, key, schema = eventType+"."+subject.Token(source), workflow, "collateral.v1.PostingConfirmation:1"
		payload = &pb.PostingConfirmation{SourceId: "confirmation-1", WorkflowId: workflow, AgreementId: "CSA1",
			LotId: "lot-1", CustodianId: "custodian", AccountId: "account", CumulativeQuantity: number(60),
			InstructionRevision: 2, SettledAt: timestamppb.New(at)}
	}
	producer, err := bus.NewProducer(c, bus.ProducerConfig{Source: source, ProducerVersion: "rehearsal", Tenant: tenant})
	if err != nil {
		return errors.New("fixture producer refused")
	}
	err = producer.Publish(ctx, bus.Event{Subject: route, EventType: eventType, EventClass: envelope.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: "accounting", EventTime: at, PartitionKey: key, PayloadSchemaRef: schema, Payload: payload})
	if err != nil {
		return errors.New("fixture publication failed")
	}
	return nil
}

// The observer waits for the real Accounting durable to acknowledge the last
// source publication. A database read alone can precede the duplicate delivery.
func receipt(ctx context.Context, route string, deliveries uint64) error {
	nc, err := nats.Connect(os.Getenv("NATS_URL"), nats.Timeout(3*time.Second))
	if err != nil {
		return errors.New("observer connection failed")
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return errors.New("observer JetStream unavailable")
	}
	st, err := js.Stream(ctx, "ACCOUNTING")
	if err != nil {
		return errors.New("accounting stream unavailable")
	}
	last, err := st.GetLastMsgForSubject(ctx, route)
	if err != nil {
		return errors.New("source publication unavailable")
	}
	if route == "accounting.collateral.recorded" {
		info, err := st.Info(ctx, jetstream.WithSubjectFilter(route))
		if err != nil || info.State.Subjects[route] != deliveries {
			return errors.New("recorded FACT count differs")
		}
		env, payload, err := bus.Unframe(last.Data)
		if err != nil || env.GetTenantId() != tenant || env.GetEventType() != route || env.GetSource() != "accounting" ||
			env.GetEventClass() != envelope.EventClass_EVENT_CLASS_FACT || env.GetPartitionKey() != workflow ||
			env.GetPayloadSchemaRef() != "collateral.v1.WorkflowRecorded:1" {
			return errors.New("recorded FACT envelope differs")
		}
		var state pb.WorkflowRecorded
		if err := proto.Unmarshal(payload, &state); err != nil || state.WorkflowId != workflow ||
			state.Revision < 1 || uint64(state.Revision) != deliveries || state.RequestId != env.CorrelationId {
			return errors.New("recorded FACT revision differs")
		}
		fmt.Printf("%x", sha256.Sum256(payload))
		return nil
	}
	matches := 0
	consumers := st.ListConsumers(ctx)
	for c := range consumers.Info() {
		if c.Config.FilterSubject != route || c.Config.Durable == "" {
			continue
		}
		matches++
		if c.AckFloor.Stream != last.Sequence || c.AckFloor.Consumer != deliveries ||
			c.Delivered.Consumer != deliveries || c.NumAckPending != 0 || c.NumPending != 0 {
			return errors.New("source delivery has not been acknowledged exactly")
		}
	}
	if consumers.Err() != nil || matches != 1 {
		return errors.New("expected a unique Accounting input durable")
	}
	fmt.Print(last.Sequence)
	return nil
}

// The disposable identity authority mints separate maker and checker tokens.
// Only short-lived bearer credentials reach the HTTP test client's process.
func mint(actor string) error {
	if actor != "maker" && actor != "checker" {
		return errors.New("unexpected fixture principal")
	}
	value, err := devtoken.Mint(os.Getenv("FIXTURE_ISSUER_KEY"), devtoken.Claims{
		Subject: actor, Tenant: tenant, Roles: []string{"kanz-user", "kanz-fund"}, Portfolios: []string{portfolio}, TTL: 15 * time.Minute,
	})
	if err != nil {
		return errors.New("disposable identity issuance failed")
	}
	fmt.Print(value)
	return nil
}

func gateway(ctx context.Context, mode string) error {
	method, path := "GET", "/v1/portfolios/"+portfolio+"/collateral/"+workflow
	var body []byte
	expected := http.StatusOK
	wantStatus := pb.WorkflowStatus_WORKFLOW_STATUS_INSTRUCTED
	wantRevision := int64(2)
	switch mode {
	case "propose", "self-approve", "approve":
		method, path = "POST", "/v1/portfolios/"+portfolio+"/collateral/actions"
		action, revision, request := "approve", "1", "approve-1"
		if mode == "self-approve" {
			expected, request = http.StatusConflict, "self-approval"
		}
		if mode == "propose" {
			action, revision, request = "propose", "0", "propose-1"
			wantStatus, wantRevision = pb.WorkflowStatus_WORKFLOW_STATUS_PROPOSED, 1
		}
		input := map[string]string{"request_id": request, "workflow_id": workflow, "expected_revision": revision,
			"action": action, "explanation": "disposable collateral workflow rehearsal"}
		if mode == "propose" {
			input["snapshot_id"] = "snapshot-1"
		}
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	case "instructed":
	case "settled":
		wantStatus, wantRevision = pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED, 3
	default:
		return errors.New("unexpected gateway operation")
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://gateway:8080"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("ACTOR_TOKEN"))
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	gatewaysig.SignRequest(req, []byte(os.Getenv("REQUEST_SIGNING_KEY")), body)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 6 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("authenticated gateway request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != expected {
		return fmt.Errorf("gateway %s returned %d, expected %d", mode, resp.StatusCode, expected)
	}
	if expected != http.StatusOK {
		fmt.Println("self-approval refused")
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return errors.New("invalid workflow response size")
	}
	var state pb.WorkflowRecorded
	if err = protojson.Unmarshal(raw, &state); err != nil {
		return errors.New("invalid workflow response")
	}
	if state.WorkflowId != workflow || state.SnapshotId != "snapshot-1" || state.Maker != "maker" ||
		state.Status != wantStatus || state.Revision != wantRevision || state.CurrencyCode != "USD" || len(state.Legs) != 1 {
		return errors.New("workflow state, principal or revision differs")
	}
	leg := state.Legs[0]
	if leg.AgreementId != "CSA1" || leg.LotId != "lot-1" || leg.AccountId != "account" || leg.CustodianId != "custodian" ||
		leg.Quantity == nil || dec.FromProto(leg.Quantity).Cmp(big.NewRat(60, 1)) != 0 {
		return errors.New("workflow settlement leg differs")
	}
	if wantStatus == pb.WorkflowStatus_WORKFLOW_STATUS_INSTRUCTED && (state.Actor != "checker" || state.PostingRevision != 2) {
		return errors.New("checker or instruction revision differs")
	}
	if wantStatus == pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED && (state.Actor != "custodian:custodian" || state.RequestId != "confirmation-1") {
		return errors.New("custody settlement identity differs")
	}
	stable, err := proto.MarshalOptions{Deterministic: true}.Marshal(&state)
	if err != nil {
		return errors.New("workflow state cannot be hashed")
	}
	fmt.Printf("%x", sha256.Sum256(stable))
	return nil
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("missing rehearsal operation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	switch os.Args[1] {
	case "receipt":
		if len(os.Args) != 4 {
			return errors.New("missing receipt query")
		}
		deliveries, err := strconv.ParseUint(os.Args[3], 10, 64)
		if err != nil || deliveries < 1 {
			return errors.New("invalid expected deliveries")
		}
		return receipt(ctx, os.Args[2], deliveries)
	case "mint":
		if len(os.Args) != 3 {
			return errors.New("missing fixture principal")
		}
		return mint(os.Args[2])
	case "snapshot", "confirm":
		return publish(ctx, os.Args[1])
	default:
		return gateway(ctx, os.Args[1])
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
