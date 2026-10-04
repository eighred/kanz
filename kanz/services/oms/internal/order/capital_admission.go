package order

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/jackc/pgx/v5"
)

var ErrUnreconciledOrders = errors.New("oms: existing orders lack verified capital commitments")

func armCapitalAdmission(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `INSERT INTO capital_activation DEFAULT VALUES ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT enabled FROM capital_activation WHERE tenant_id=app_current_tenant()`).Scan(&enabled); err != nil {
		return err
	}
	if enabled {
		return nil
	}
	// Only the first activation needs an exclusive tenant row lock. Once armed,
	// the monotonic trigger makes the cheap read above stable; ordinary orders
	// do not queue behind a tenant-wide FOR UPDATE on their hot path.
	if err := tx.QueryRow(ctx, `SELECT enabled FROM capital_activation WHERE tenant_id=app_current_tenant() FOR UPDATE`).Scan(&enabled); err != nil {
		return err
	}
	if enabled {
		return nil
	}
	var legacy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM orders o WHERE NOT EXISTS (
			SELECT 1 FROM capital_members m WHERE m.tenant_id=o.tenant_id AND m.order_id=o.order_id
		))`).Scan(&legacy); err != nil {
		return err
	}
	if legacy {
		return ErrUnreconciledOrders
	}
	_, err := tx.Exec(ctx, `UPDATE capital_activation SET enabled=true,activated_at=clock_timestamp() WHERE tenant_id=app_current_tenant()`)
	return err
}

// FundedAdmissionStore keeps reservation and acceptance in one transaction.
// A store lacking this method cannot be used with an armed capital resolver.
type FundedAdmissionStore interface {
	CreateFunded(context.Context, *orderpb.OrderState, []outbox.Record, []*commonpb.Money, time.Time, time.Duration) error
}

type FundedAmendStore interface {
	SaveFundedAmend(context.Context, *orderpb.OrderState, int64, []outbox.Record, []*commonpb.Money, time.Time) error
}

type FundedTerminalStore interface {
	SaveFundedTerminal(context.Context, *orderpb.OrderState, int64, []outbox.Record, time.Time) error
}

// SaveFundedTerminal is called only after the OMS confirmed venue withdrawal,
// or proved the order never reached a venue. It retains actual debit until the
// accounting receipt proves it is included in cash.
func (p *Postgres) SaveFundedTerminal(ctx context.Context, st *orderpb.OrderState, expectedVersion int64, announce []outbox.Record, now time.Time) error {
	if st == nil || st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED && st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_EXPIRED && st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_REJECTED {
		return capital.ErrInvalid
	}
	return p.save(ctx, st, expectedVersion, announce, "", func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT currency FROM capital_members WHERE order_id=$1 ORDER BY currency`, st.GetOrderId())
		if err != nil {
			return err
		}
		var currencies []string
		for rows.Next() {
			var currency string
			if err := rows.Scan(&currency); err != nil {
				rows.Close()
				return err
			}
			currencies = append(currencies, currency)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(currencies) == 0 {
			return nil
		} // legacy cancel must remain possible while activation is halted
		if _, err := fundedSettlement(st, currencies); err != nil {
			return err
		}
		if st.GetParentOrderId() != "" {
			return nil
		} // the parent's commitment covers remaining siblings
		var persisted []byte
		if err := tx.QueryRow(ctx, `SELECT state FROM orders WHERE order_id=$1`, st.GetOrderId()).Scan(&persisted); err != nil {
			return err
		}
		current, err := unmarshalState(persisted, st.GetOrderId())
		if err != nil {
			return err
		}
		if current.GetExecutionSchedule() != nil {
			// Cash is locked before this scan. A new child must acquire that same
			// cash lock before it can pass the parent cancellation barrier, so it
			// cannot appear between the scan and the terminal parent CAS.
			children, err := tx.Query(ctx, `SELECT order_id,state FROM orders WHERE parent_order_id=$1`, st.GetOrderId())
			if err != nil {
				return err
			}
			for children.Next() {
				var id string
				var data []byte
				if err := children.Scan(&id, &data); err != nil {
					children.Close()
					return err
				}
				child, err := unmarshalState(data, id)
				if err != nil {
					children.Close()
					return err
				}
				if !IsTerminal(child) {
					children.Close()
					return ErrParentStopped
				}
			}
			if err := children.Err(); err != nil {
				children.Close()
				return err
			}
			children.Close()
		}
		for _, currency := range currencies {
			if err := capital.FinishExecution(ctx, tx, st.GetPortfolioId(), currency, st.GetOrderId(), now); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveFundedAmend changes the cash bound before taking the order row lock, then
// commits the new order version and command outcome under the same transaction.
func (p *Postgres) SaveFundedAmend(ctx context.Context, st *orderpb.OrderState, expectedVersion int64, announce []outbox.Record, debits []*commonpb.Money, now time.Time) error {
	if st == nil || st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW || st.GetParentOrderId() != "" || len(debits) == 0 {
		return capital.ErrInvalid
	}
	for i, debit := range debits {
		if debit == nil || debit.Amount == nil || i > 0 && debits[i-1].CurrencyCode >= debit.CurrencyCode {
			return capital.ErrInvalid
		}
		value, ok := dec.FromProtoChecked(debit.Amount)
		if !ok || value.Sign() < 0 {
			return capital.ErrInvalid
		}
	}
	return p.save(ctx, st, expectedVersion, announce, "", func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT currency FROM capital_commitments WHERE order_id=$1 ORDER BY currency`, st.GetOrderId())
		if err != nil {
			return err
		}
		var currencies []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return err
			}
			currencies = append(currencies, c)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(currencies) != len(debits) {
			return capital.ErrInvalid
		}
		for i, currency := range currencies {
			if debits[i].CurrencyCode != currency {
				return capital.ErrInvalid
			}
		}
		if _, err := fundedSettlement(st, currencies); err != nil {
			return err
		}
		for _, debit := range debits {
			value, _ := dec.FromProtoChecked(debit.Amount)
			var version int64
			if err := tx.QueryRow(ctx, `SELECT version FROM capital_commitments WHERE order_id=$1 AND currency=$2 AND portfolio_id=$3`, st.GetOrderId(), debit.CurrencyCode, st.GetPortfolioId()).Scan(&version); err != nil {
				return err
			}
			if err := capital.Change(ctx, tx, st.GetPortfolioId(), debit.CurrencyCode, st.GetOrderId(), version, dec.Exact(value.RatString()), now, time.Minute); err != nil {
				return err
			}
		}
		return nil
	})
}

// Recovery's venue history is complete, so its exact cumulative debit can
// replace the member observation without replaying each fill as a new cash
// operation. This runs after execution claims and before the order CAS; a
// conflict rolls all cash changes back with the recovery journal and FACTs.
func (p *Postgres) observeFundedRecovery(ctx context.Context, tx pgx.Tx, st *orderpb.OrderState, fills []*orderpb.Fill, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT currency FROM capital_members WHERE order_id=$1 ORDER BY currency`, st.GetOrderId())
	if err != nil {
		return err
	}
	var currencies []string
	for rows.Next() {
		var currency string
		if err := rows.Scan(&currency); err != nil {
			rows.Close()
			return err
		}
		currencies = append(currencies, currency)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(currencies) == 0 {
		return nil
	} // pre-capital order; activation reconciliation owns it
	settlement, err := fundedSettlement(st, currencies)
	if err != nil {
		return err
	}
	actual := make(map[string]*big.Rat)
	for _, currency := range currencies {
		actual[currency] = new(big.Rat)
	}
	for _, fill := range fills {
		if fill == nil {
			return capital.ErrInvalid
		}
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
		} else if st.GetSide() == orderpb.Side_SIDE_SELL {
			actual[settlement].Add(actual[settlement], quantity)
		} else {
			return capital.ErrInvalid
		}
		if err := addFundedFee(actual, fill.GetFee(), currencies); err != nil {
			return err
		}
	}
	for _, currency := range currencies {
		if err := capital.ObserveExecution(ctx, tx, st.GetPortfolioId(), currency, st.GetOrderId(), dec.Exact(actual[currency].RatString())); err != nil {
			return err
		}
	}
	if IsTerminal(st) && st.GetParentOrderId() == "" {
		for _, currency := range currencies {
			if err := capital.FinishExecution(ctx, tx, st.GetPortfolioId(), currency, st.GetOrderId(), now); err != nil {
				return err
			}
		}
	}
	return nil
}

// CreateFunded commits an order, its cash ownership and acceptance together.
// Schedules reserve once at the parent; each child inherits that owner and
// consumes a bounded exact quantity under the parent cancellation row lock.
func (p *Postgres) CreateFunded(ctx context.Context, st *orderpb.OrderState, announce []outbox.Record, debits []*commonpb.Money, now time.Time, maxAge time.Duration) error {
	if st == nil || st.GetParentOrderId() != "" && st.GetExecutionSchedule() != nil ||
		st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW && st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED ||
		st.GetParentOrderId() != "" && st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW ||
		st.GetExecutionSchedule() != nil && st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		return capital.ErrInvalid
	}
	var afterParent func(pgx.Tx) error
	if st.GetParentOrderId() != "" {
		afterParent = func(tx pgx.Tx) error { return allocateScheduledCapital(ctx, tx, st) }
	}
	return p.create(ctx, st, announce, func(tx pgx.Tx) error {
		// A redelivery remains a duplicate even if its original cash coverage
		// has subsequently expired. Never acquire a second reservation for it.
		exists := func() (bool, error) {
			var found bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE order_id=$1)`, st.GetOrderId()).Scan(&found)
			return found, err
		}
		if found, err := exists(); err != nil {
			return err
		} else if found {
			return ErrExists
		}
		if err := armCapitalAdmission(ctx, tx); err != nil {
			return err
		}
		if st.GetParentOrderId() != "" {
			// A terminal parent must be reported as stopped even if its original
			// allocation is already exhausted. This read does not lock: the
			// authoritative parent-row lock in create still arbitrates a racing
			// cancellation after the currency locks have been acquired.
			var blob []byte
			err := tx.QueryRow(ctx, `SELECT state FROM orders WHERE order_id=$1`, st.GetParentOrderId()).Scan(&blob)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrParentStopped
			}
			if err != nil {
				return err
			}
			parent, err := unmarshalState(blob, st.GetParentOrderId())
			if err != nil {
				return err
			}
			if err := checkWorkingParent(parent, st); err != nil {
				return err
			}
		}
		var err error
		if st.GetParentOrderId() == "" {
			err = capital.ReserveMany(ctx, tx, st.GetPortfolioId(), st.GetOrderId(), debits, now, maxAge)
		} else {
			// The parent's reservation owns EVERY debit currency. Registering
			// only its settlement leg would leave a later fee in another asset
			// without a member to attribute the execution or accounting receipt.
			// RegisterChild does not reserve again; each member shares the owner.
			parentRows, queryErr := tx.Query(ctx, `SELECT currency FROM capital_commitments WHERE order_id=$1 ORDER BY currency`, st.GetParentOrderId())
			if queryErr != nil {
				err = queryErr
			} else {
				var parentCurrencies []string
				for parentRows.Next() {
					var currency string
					if scanErr := parentRows.Scan(&currency); scanErr != nil {
						err = scanErr
						break
					}
					parentCurrencies = append(parentCurrencies, currency)
				}
				if err == nil {
					err = parentRows.Err()
				}
				parentRows.Close()
				if err == nil && len(parentCurrencies) != len(debits) {
					err = capital.ErrInvalid
				}
				if err == nil {
					for i, currency := range parentCurrencies {
						if debits[i] == nil || debits[i].Amount == nil || debits[i].CurrencyCode != currency {
							err = capital.ErrInvalid
							break
						}
						value, ok := dec.FromProtoChecked(debits[i].Amount)
						if !ok || value.Sign() < 0 {
							err = capital.ErrInvalid
							break
						}
						if err = capital.AllocateChild(ctx, tx, st.GetPortfolioId(), currency, st.GetParentOrderId(), st.GetOrderId(), dec.Exact(value.RatString()), now, maxAge); err != nil {
							break
						}
					}
				}
			}
		}
		if err != nil {
			// ReserveMany rolls back its savepoint on refusal. A racing winner
			// may have consumed the cash and committed this same order while we
			// waited for its currency lock; preserve Create's idempotent result.
			if found, queryErr := exists(); queryErr != nil {
				return queryErr
			} else if found {
				return ErrExists
			}
			return err
		}
		return nil
	}, afterParent)
}

func allocateScheduledCapital(ctx context.Context, tx pgx.Tx, child *orderpb.OrderState) error {
	// lockWorkingParent has already locked this row and checked cancellation.
	var data []byte
	var allocatedText string
	if err := tx.QueryRow(ctx, `SELECT state,allocated_quantity::text FROM orders WHERE order_id=$1`, child.GetParentOrderId()).Scan(&data, &allocatedText); err != nil {
		return err
	}
	parent, err := unmarshalState(data, child.GetParentOrderId())
	if err != nil {
		return err
	}
	if parent.GetExecutionSchedule() == nil || parent.GetInstrumentId() != child.GetInstrumentId() || parent.GetSide() != child.GetSide() ||
		parent.GetOrderType() != child.GetOrderType() || parent.GetTimeInForce() != child.GetTimeInForce() ||
		parent.GetVenue() != child.GetVenue() || parent.GetVenueAccountId() != child.GetVenueAccountId() ||
		parent.GetMarginMode() != child.GetMarginMode() || !sameCapitalDecimal(parent.GetLeverage(), child.GetLeverage()) ||
		!sameCapitalDecimal(parent.GetLimitPrice(), child.GetLimitPrice()) || !sameCapitalDecimal(parent.GetStopPrice(), child.GetStopPrice()) {
		return capital.ErrInvalid
	}
	quantity, valid := dec.FromProtoChecked(child.GetOrderedQuantity())
	parentQuantity, parentValid := dec.FromProtoChecked(parent.GetOrderedQuantity())
	allocated, parseErr := dec.Exact(allocatedText).Rat()
	if !valid || !parentValid || parseErr != nil || quantity.Sign() <= 0 || parentQuantity.Sign() <= 0 {
		return capital.ErrInvalid
	}
	next := new(big.Rat).Add(allocated, quantity)
	if next.Cmp(parentQuantity) > 0 {
		return capital.ErrInvalid
	}
	exact, err := dec.ExactFromRat(next)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET allocated_quantity=$2 WHERE order_id=$1`, child.GetParentOrderId(), string(exact)); err != nil {
		return err
	}
	return nil
}

func sameCapitalDecimal(a, b *commonpb.Decimal) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ar, aOK := dec.FromProtoChecked(a)
	br, bOK := dec.FromProtoChecked(b)
	return aOK && bOK && ar.Cmp(br) == 0
}

// An unexpected same-currency fee is observed as actual liability even when it
// exceeds the admission bound. A different fee currency needs a separate cash
// commitment and cannot be silently netted against this one.
func fundedSettlement(st *orderpb.OrderState, currencies []string) (string, error) {
	parts := strings.Split(st.GetInstrumentId(), "-")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", capital.ErrInvalid
	}
	settlement := parts[1]
	if st.GetSide() == orderpb.Side_SIDE_SELL {
		settlement = parts[0]
	} else if st.GetSide() != orderpb.Side_SIDE_BUY {
		return "", capital.ErrInvalid
	}
	for _, currency := range currencies {
		if currency == settlement {
			return settlement, nil
		}
	}
	return "", capital.ErrInvalid
}

func addFundedFee(actual map[string]*big.Rat, fee *commonpb.Money, currencies []string) error {
	if fee == nil {
		return nil
	}
	value, ok := dec.FromProtoChecked(fee.GetAmount())
	if !ok {
		return capital.ErrInvalid
	}
	for _, currency := range currencies {
		if currency == fee.GetCurrencyCode() {
			if actual[currency] == nil {
				actual[currency] = new(big.Rat)
			}
			actual[currency].Add(actual[currency], value)
			return nil
		}
	}
	return capital.ErrInvalid
}
