package order

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// LegacyAuthority is the independently approved historical maximum source.
// It must cover the original order regime and all remaining executable fees;
// observed venue rates and posted fees cannot implement this contract.
type LegacyAuthority interface {
	ResolveLegacy(context.Context, *orderpb.OrderState, time.Time) (LegacyTerms, error)
}

type LegacyTerms struct {
	Required                    []*commonpb.Money
	Source, Version, ApprovedBy string
}

// ReconcileLegacy materializes one old obligation before cash-history replay.
// Every fill must be present in the immutable OMS execution journal, including
// fee revision heads. Missing history, a changed order, an incomplete currency
// vector or an already-applied cash prefix refuses the import. The method does
// not enable admission; the activation barrier still scans every old order and
// ReserveMany still requires fresh, complete accounting coverage.
func (p *Postgres) ReconcileLegacy(ctx context.Context, orderID string, authority LegacyAuthority, now time.Time) error {
	if orderID == "" || authority == nil || now.IsZero() {
		return capital.ErrInvalid
	}
	st, version, err := p.Load(ctx, orderID)
	if err != nil {
		return err
	}
	posted, err := p.postedRecoveryExecutions(ctx, st)
	if err != nil {
		return err
	}
	terms, err := authority.ResolveLegacy(ctx, st, now)
	if err != nil {
		return err
	}
	if strings.TrimSpace(terms.Source) == "" || len(terms.Source) > 256 || strings.TrimSpace(terms.Version) == "" || len(terms.Version) > 256 || strings.TrimSpace(terms.ApprovedBy) == "" || len(terms.ApprovedBy) > 256 || len(terms.Required) == 0 || len(terms.Required) > 32 {
		return capital.ErrInvalid
	}
	currencies := make([]string, len(terms.Required))
	required := make(map[string]*big.Rat, len(terms.Required))
	for i, money := range terms.Required {
		if money == nil || money.Amount == nil || money.CurrencyCode == "" || i > 0 && currencies[i-1] >= money.CurrencyCode {
			return capital.ErrInvalid
		}
		value, ok := dec.FromProtoChecked(money.Amount)
		if !ok || value.Sign() < 0 {
			return capital.ErrInvalid
		}
		currencies[i], required[money.CurrencyCode] = money.CurrencyCode, value
	}
	settlement, err := fundedSettlement(st, currencies)
	if err != nil {
		return err
	}
	if !IsTerminal(st) {
		pair := strings.Split(st.GetInstrumentId(), "-")
		principal, err := capital.PhysicalDebits(st, pair[0], pair[1], []*commonpb.Money{})
		if err != nil {
			return capital.ErrInvalid
		}
		minimum, ok := dec.FromProtoChecked(principal[0].GetAmount())
		if !ok || required[settlement].Cmp(minimum) < 0 {
			return capital.ErrInvalid
		}
	}
	actual := make(map[string]*big.Rat, len(currencies))
	for _, currency := range currencies {
		actual[currency] = new(big.Rat)
	}
	keys := make([]string, 0, len(posted))
	for key := range posted {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fillBytes := make([][]byte, 0, len(keys))
	for _, key := range keys {
		fill := posted[key]
		quantity, ok := dec.FromProtoChecked(fill.GetQuantity())
		if !ok || quantity.Sign() <= 0 {
			return capital.ErrInvalid
		}
		if st.GetSide() == orderpb.Side_SIDE_BUY {
			price, ok := dec.FromProtoChecked(fill.GetPrice())
			if !ok || price.Sign() <= 0 {
				return capital.ErrInvalid
			}
			actual[settlement].Add(actual[settlement], new(big.Rat).Mul(quantity, price))
		} else {
			actual[settlement].Add(actual[settlement], quantity)
		}
		if err := addFundedFee(actual, fill.GetFee(), currencies); err != nil {
			return err
		}
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(fill)
		if err != nil {
			return err
		}
		fillBytes = append(fillBytes, data)
	}
	for _, currency := range currencies {
		if actual[currency].Sign() < 0 || !IsTerminal(st) && required[currency].Cmp(actual[currency]) < 0 {
			return capital.ErrInvalid
		}
	}
	stateBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(st)
	if err != nil {
		return err
	}
	proof, err := json.Marshal(struct {
		State                       []byte
		Fills                       [][]byte
		Required                    []*commonpb.Money
		Source, Version, ApprovedBy string
	}{stateBytes, fillBytes, terms.Required, terms.Source, terms.Version, terms.ApprovedBy})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(proof)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Admission locks currencies before order/parent rows. Keep the same order
	// while importing so a concurrent funded admission cannot form a cycle.
	for _, currency := range currencies {
		if _, err := tx.Exec(ctx, `INSERT INTO capital_balances(portfolio_id,currency) VALUES($1,$2) ON CONFLICT DO NOTHING`, st.GetPortfolioId(), currency); err != nil {
			return err
		}
		var locked string
		if err := tx.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id=$1 AND currency=$2 FOR UPDATE`, st.GetPortfolioId(), currency).Scan(&locked); err != nil {
			return err
		}
	}
	// Match dispatch's parent-before-child row order to avoid a cancellation or
	// amendment cycle while historical obligations are imported.
	if st.GetParentOrderId() != "" {
		var locked []byte
		if err := tx.QueryRow(ctx, `SELECT state FROM orders WHERE order_id=$1 FOR UPDATE`, st.GetParentOrderId()).Scan(&locked); err != nil {
			return err
		}
	}
	var current []byte
	var currentVersion int64
	if err := tx.QueryRow(ctx, `SELECT state,version FROM orders WHERE order_id=$1 FOR UPDATE`, orderID).Scan(&current, &currentVersion); err != nil {
		return err
	}
	currentState, err := unmarshalState(current, orderID)
	if err != nil {
		return err
	}
	if currentVersion != version || !proto.Equal(st, currentState) {
		return ErrConflict
	}
	var active, members bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT enabled FROM capital_activation WHERE tenant_id=app_current_tenant()),false)`).Scan(&active); err != nil {
		return err
	}
	if active {
		return capital.ErrInvalid
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM capital_members WHERE order_id=$1)`, orderID).Scan(&members); err != nil {
		return err
	}
	if members {
		return capital.ErrConflict
	}
	if st.GetParentOrderId() == "" {
		for _, currency := range currencies {
			amount := required[currency]
			if IsTerminal(st) {
				amount = actual[currency]
			}
			if err := capital.ImportLegacy(ctx, tx, st.GetPortfolioId(), currency, orderID, dec.Exact(amount.RatString()), dec.Exact(actual[currency].RatString())); err != nil {
				return err
			}
		}
	} else {
		if err := p.reconcileLegacyChild(ctx, tx, st, currencies, actual); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO capital_legacy_evidence(order_id,source,source_version,approved_by,evidence_digest,reconciled_at) VALUES($1,$2,$3,$4,$5,$6)`, orderID, terms.Source, terms.Version, terms.ApprovedBy, digest[:], now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) reconcileLegacyChild(ctx context.Context, tx pgx.Tx, st *orderpb.OrderState, currencies []string, actual map[string]*big.Rat) error {
	var parentBytes []byte
	if err := tx.QueryRow(ctx, `SELECT state FROM orders WHERE order_id=$1`, st.GetParentOrderId()).Scan(&parentBytes); err != nil {
		return err
	}
	parent, err := unmarshalState(parentBytes, st.GetParentOrderId())
	if err != nil {
		return err
	}
	if parent.GetExecutionSchedule() == nil || parent.GetPortfolioId() != st.GetPortfolioId() || parent.GetInstrumentId() != st.GetInstrumentId() || parent.GetVenue() != st.GetVenue() || parent.GetVenueAccountId() != st.GetVenueAccountId() || parent.GetSide() != st.GetSide() || parent.GetOrderType() != st.GetOrderType() || !sameCapitalDecimal(parent.GetLimitPrice(), st.GetLimitPrice()) {
		return capital.ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT currency FROM capital_commitments WHERE order_id=$1 ORDER BY currency`, st.GetParentOrderId())
	if err != nil {
		return err
	}
	var parentCurrencies []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return err
		}
		parentCurrencies = append(parentCurrencies, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(parentCurrencies) != len(currencies) {
		return capital.ErrInvalid
	}
	for i, currency := range currencies {
		if parentCurrencies[i] != currency {
			return capital.ErrInvalid
		}
	}
	for _, currency := range currencies {
		if err := capital.ImportLegacyChild(ctx, tx, st.GetPortfolioId(), currency, st.GetParentOrderId(), st.GetOrderId()); err != nil {
			return err
		}
		if err := capital.ObserveExecution(ctx, tx, st.GetPortfolioId(), currency, st.GetOrderId(), dec.Exact(actual[currency].RatString())); err != nil {
			return err
		}
	}
	return nil
}
