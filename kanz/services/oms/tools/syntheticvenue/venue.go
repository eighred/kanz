// Package syntheticvenue implements a disposable, deterministic venue.v1 counterparty.
// Its receipts describe only what this mock accepted. OMS owns financial state.
package syntheticvenue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	MIC           = "XSIM"
	Account       = "synthetic-spot"
	Instrument    = "SYNTH-USD"
	OMSIdentity   = "spiffe://kanz.internal/ns/kanz-services/sa/oms"
	VenueIdentity = "spiffe://kanz.internal/ns/kanz-services/sa/venue-fixture"
)

type Config struct {
	Tenant, Portfolio string
	// The fixture supplies a venue clock independently from OMS observation time.
	FillTime       time.Time
	LoseExecuteAck bool
	// When enabled, Execute persists a partial fill while the order is still open.
	FillOnExecute bool
	// The venue reports its actual commission in its execution receipt.
	ExecutionFee *commonpb.Money
}

type Venue struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	cfg  Config
	pool *pgxpool.Pool
}

// The OMS observation key is bounded even when a submitted order or event ID
// already uses its entire allowance. JSON array framing preserves scope.
func sourceID(kind string, coordinates ...string) string {
	encoded, _ := json.Marshal(coordinates)
	digest := sha256.Sum256(encoded)
	return kind + "/" + hex.EncodeToString(digest[:])
}

func New(ctx context.Context, cfg Config, pool *pgxpool.Pool) (*Venue, error) {
	if cfg.Tenant == "" || cfg.Portfolio == "" || cfg.FillTime.IsZero() || timestamppb.New(cfg.FillTime).CheckValid() != nil || pool == nil {
		return nil, errors.New("synthetic venue requires a tenant, portfolio, valid fill time and isolated receipt database")
	}
	if fee := cfg.ExecutionFee; fee != nil {
		if fee.CurrencyCode != "USD" || !dec.InDomain(fee.Amount) || !dec.IsPositive(fee.Amount) {
			return nil, errors.New("synthetic XSIM execution fee must be positive USD")
		}
		cfg.ExecutionFee = proto.Clone(fee).(*commonpb.Money)
	}
	conn := pool.Config().ConnConfig
	if !net.ParseIP(conn.Host).IsLoopback() {
		return nil, errors.New("synthetic receipt store requires literal loopback")
	}
	for _, fallback := range conn.Fallbacks {
		if !net.ParseIP(fallback.Host).IsLoopback() {
			return nil, errors.New("synthetic receipt store fallback must be loopback")
		}
	}
	var safe bool
	err := pool.QueryRow(ctx, `SELECT current_user = 'kanz_synthetic_venue' AND NOT rolsuper AND NOT rolbypassrls
        AND NOT rolcreaterole AND NOT rolcreatedb AND NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member = r.oid)
        AND c.relrowsecurity AND c.relforcerowsecurity AND c.relowner <> r.oid
        FROM pg_roles r, pg_class c WHERE r.rolname=current_user AND c.oid='synthetic_venue.receipts'::regclass`).Scan(&safe)
	if err != nil || !safe {
		return nil, errors.New("synthetic venue requires its restricted database identity and tenant-isolated receipt table")
	}
	return &Venue{cfg: cfg, pool: pool}, nil
}
func (v *Venue) Describe(context.Context, *venuepb.DescribeRequest) (*venuepb.DescribeResponse, error) {
	return &venuepb.DescribeResponse{Mic: MIC, Account: Account, AccountVerified: true, ExchangeAccountId: Account,
		SupportedOrderTypes:  []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT},
		SupportedTimeInForce: []orderpb.TimeInForce{orderpb.TimeInForce_TIME_IN_FORCE_GTC},
		SupportedMarginModes: []orderpb.MarginMode{orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED}}, nil
}
func (v *Venue) ListInstruments(context.Context, *venuepb.ListInstrumentsRequest) (*venuepb.ListInstrumentsResponse, error) {
	return &venuepb.ListInstrumentsResponse{Mic: MIC, Instruments: []*venuepb.VenueInstrument{{InstrumentId: Instrument, VenueSymbol: "SYNTHUSD", BaseAsset: "SYNTH", QuoteAsset: "USD"}}}, nil
}
func (v *Venue) check(tenant string, st *orderpb.OrderState) error {
	if tenant != v.cfg.Tenant || st.GetPortfolioId() != v.cfg.Portfolio {
		return status.Error(codes.PermissionDenied, "synthetic venue scope denied")
	}
	if st.GetOrderId() == "" || len(st.GetOrderId()) > 256 || st.GetInstrumentId() != Instrument || st.GetVenue() != MIC || st.GetVenueAccountId() != Account {
		return status.Error(codes.InvalidArgument, "invalid synthetic venue order coordinates")
	}
	return nil
}

// XSIM reports one half-quantity execution on withdrawal. Refuse any order
// whose fill cannot travel losslessly through the canonical Decimal domain.
func partialQuantity(q *commonpb.Decimal) (*commonpb.Decimal, bool) {
	if !dec.InDomain(q) || !dec.IsPositive(q) {
		return nil, false
	}
	if q.Coefficient%2 == 0 {
		return &commonpb.Decimal{Coefficient: q.Coefficient / 2, Exponent: q.Exponent}, true
	}
	if q.Coefficient > math.MaxInt64/5 {
		return nil, false
	}
	half := &commonpb.Decimal{Coefficient: q.Coefficient * 5, Exponent: q.Exponent - 1}
	return half, dec.InDomain(half)
}

func terms(st *orderpb.OrderState) ([]byte, error) {
	_, splittable := partialQuantity(st.GetOrderedQuantity())
	if st.GetOrderType() != orderpb.OrderType_ORDER_TYPE_LIMIT || st.GetTimeInForce() != orderpb.TimeInForce_TIME_IN_FORCE_GTC ||
		(st.GetSide() != orderpb.Side_SIDE_BUY && st.GetSide() != orderpb.Side_SIDE_SELL) ||
		st.GetMarginMode() != orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED || st.GetLeverage() != nil ||
		st.GetStopPrice() != nil || st.GetExpireAt() != nil || st.GetExecutionSchedule() != nil || st.GetParentOrderId() != "" ||
		!splittable ||
		!dec.InDomain(st.GetLimitPrice()) || !dec.IsPositive(st.GetLimitPrice()) {
		return nil, status.Error(codes.InvalidArgument, "only positive unlevered synthetic limit orders are supported")
	}
	// Compare immutable submitted terms across retry; status and observation time
	// may change while the same remote order is being reconciled.
	return proto.MarshalOptions{Deterministic: true}.Marshal(&orderpb.OrderState{
		OrderId: st.OrderId, PortfolioId: st.PortfolioId, InstrumentId: st.InstrumentId,
		Venue: st.Venue, VenueAccountId: st.VenueAccountId, Side: st.Side, OrderType: st.OrderType,
		TimeInForce: st.TimeInForce, OrderedQuantity: st.OrderedQuantity, LimitPrice: st.LimitPrice,
	})
}
func (v *Venue) transaction(ctx context.Context, f func(pgx.Tx) error) error {
	err := pgx.BeginFunc(ctx, v.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", v.cfg.Tenant); err != nil {
			return err
		}
		return f(tx)
	})
	if err != nil && status.Code(err) == codes.Unknown {
		return status.Error(codes.Unavailable, "synthetic receipt store unavailable")
	}
	return err
}
func (v *Venue) Execute(ctx context.Context, r *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	st := r.GetState()
	if err := v.check(r.GetTenantId(), st); err != nil {
		return nil, err
	}
	wire, err := terms(st)
	if err != nil {
		return nil, err
	}
	var created bool
	var execution []byte
	if v.cfg.FillOnExecute {
		execution, err = v.halfFill(st)
		if err != nil {
			return nil, err
		}
	}
	err = v.transaction(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO synthetic_venue.receipts(tenant_id,portfolio_id,order_id,terms,execution,working_partial) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, v.cfg.Tenant, v.cfg.Portfolio, st.OrderId, wire, execution, v.cfg.FillOnExecute)
		if err != nil {
			return err
		}
		created = tag.RowsAffected() == 1
		if !created {
			var same bool
			if err := tx.QueryRow(ctx, `SELECT terms=$4 FROM synthetic_venue.receipts WHERE tenant_id=$1 AND portfolio_id=$2 AND order_id=$3`, v.cfg.Tenant, v.cfg.Portfolio, st.OrderId, wire).Scan(&same); err != nil {
				return err
			}
			if !same {
				return status.Error(codes.AlreadyExists, "remote order ID already has different terms")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if created && v.cfg.LoseExecuteAck {
		return nil, status.Error(codes.DeadlineExceeded, "synthetic acknowledgement unavailable")
	}
	return &venuepb.ExecuteResponse{}, nil
}
func (v *Venue) halfFill(original *orderpb.OrderState) ([]byte, error) {
	half, ok := partialQuantity(original.GetOrderedQuantity())
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "half the submitted quantity cannot be represented")
	}
	id := sourceID("xsim-partial", v.cfg.Tenant, v.cfg.Portfolio, original.OrderId)
	return proto.Marshal(&orderpb.Fill{FillId: id, VenueExecutionId: id, OrderId: original.OrderId, InstrumentId: Instrument, Side: original.Side, Venue: MIC, VenueAccountId: Account,
		Quantity: half, Price: original.LimitPrice, Fee: v.cfg.ExecutionFee, ExecutedAt: timestamppb.New(v.cfg.FillTime)})
}
func (v *Venue) CancelOrder(ctx context.Context, r *venuepb.CancelOrderRequest) (*venuepb.CancelOrderResponse, error) {
	st := r.GetState()
	if err := v.check(r.GetTenantId(), st); err != nil {
		return nil, err
	}
	err := v.transaction(ctx, func(tx pgx.Tx) error {
		var wire, prior []byte
		var workingPartial bool
		err := tx.QueryRow(ctx, `SELECT terms,execution,working_partial FROM synthetic_venue.receipts WHERE tenant_id=$1 AND portfolio_id=$2 AND order_id=$3 FOR UPDATE`, v.cfg.Tenant, v.cfg.Portfolio, st.OrderId).Scan(&wire, &prior, &workingPartial)
		if errors.Is(err, pgx.ErrNoRows) {
			return status.Error(codes.NotFound, "unknown synthetic order")
		}
		if err != nil || prior != nil && !workingPartial {
			return err
		}
		if workingPartial {
			_, err = tx.Exec(ctx, `UPDATE synthetic_venue.receipts SET working_partial=false WHERE tenant_id=$1 AND portfolio_id=$2 AND order_id=$3`, v.cfg.Tenant, v.cfg.Portfolio, st.OrderId)
			return err
		}
		var original orderpb.OrderState
		if err := proto.Unmarshal(wire, &original); err != nil {
			return err
		}
		wire, err = v.halfFill(&original)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE synthetic_venue.receipts SET execution=$4 WHERE tenant_id=$1 AND portfolio_id=$2 AND order_id=$3`, v.cfg.Tenant, v.cfg.Portfolio, st.OrderId, wire)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &venuepb.CancelOrderResponse{}, nil
}
func (v *Venue) QueryOrder(ctx context.Context, r *venuepb.QueryOrderRequest) (*venuepb.QueryOrderResponse, error) {
	st := r.GetState()
	if err := v.check(r.GetTenantId(), st); err != nil {
		return nil, err
	}
	reply := &venuepb.QueryOrderResponse{}
	err := v.transaction(ctx, func(tx pgx.Tx) error {
		var wire, storedTerms []byte
		var partial bool
		err := tx.QueryRow(ctx, `SELECT execution,terms,working_partial FROM synthetic_venue.receipts WHERE tenant_id=$1 AND portfolio_id=$2 AND order_id=$3`, v.cfg.Tenant, v.cfg.Portfolio, st.OrderId).Scan(&wire, &storedTerms, &partial)
		if errors.Is(err, pgx.ErrNoRows) {
			reply.State = venuepb.OrderViewState_ORDER_VIEW_STATE_UNKNOWN
			return nil
		}
		if err != nil {
			return err
		}
		// Coordinate-only recovery can still query by identity. The funded
		// control sends full trusted terms, which XSIM binds to its own receipt.
		if st.OrderedQuantity != nil && st.LimitPrice != nil {
			supplied, err := terms(st)
			if err != nil || !bytes.Equal(supplied, storedTerms) {
				return status.Error(codes.FailedPrecondition, "queried synthetic terms differ from remote order")
			}
		}
		if wire == nil {
			reply.State = venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING
			reply.ExecutedQuantity = &commonpb.Decimal{}
			return nil
		}
		var fill orderpb.Fill
		if err := proto.Unmarshal(wire, &fill); err != nil {
			return err
		}
		reply.State = venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED
		if partial {
			reply.State = venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED
		}
		reply.Fills = []*orderpb.Fill{&fill}
		reply.ExecutedQuantity = fill.Quantity
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reply, nil
}
