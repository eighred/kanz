package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/pkg/bus"
)

const (
	subjectAccepted      = "order.order.accepted"
	subjectRejected      = "order.order.rejected"
	subjectRouted        = "order.order.routed"
	subjectPortfolioCash = "accounting.balance.portfolio"
	maxObservedFills     = 256
)

// factCollector is an evidence fold, not a second order aggregate. The canonical
// state remains in OMS and accounting; this fold only proves that one command's
// typed FACT lineage crossed each committed boundary and retains the venue's
// atomic executions for the subsequent book-of-record comparison.
type factCollector struct {
	cfg     config
	orderID string
	line    lineage
	changed chan struct{}

	mu              sync.Mutex
	fills           [maxObservedFills]*orderpb.Fill
	fillEvents      [maxObservedFills]string
	fillCount       int
	accountedCauses [maxObservedFills]string
	accountedCount  int
	liveArmed       bool
	terminalState   *orderpb.OrderState
	failure         error
}

func newFactCollector(cfg config, orderID string) *factCollector {
	return &factCollector{
		cfg: cfg, orderID: orderID, changed: make(chan struct{}, 1),
	}
}

// armLive is called only after every retained replay has drained. Accounting
// balance FACTs are portfolio-keyed rather than order-keyed, so ignoring them
// before this boundary prevents the estate's historical portfolio activity from
// entering this one-order proof.
func (c *factCollector) armLive() {
	c.mu.Lock()
	c.liveArmed = true
	c.mu.Unlock()
}

func (c *factCollector) handle(subject string) bus.EventHandler {
	return func(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
		// Replay necessarily sees the estate's retained order history. Filtering by
		// the envelope key first prevents unrelated tenants from becoming visible
		// through error text or retained payloads.
		expectedKey := c.orderID
		if subject == subjectPortfolioCash {
			expectedKey = c.cfg.portfolio
		}
		if env.GetPartitionKey() != expectedKey {
			return nil
		}
		if env.GetTenantId() != c.cfg.tenant {
			c.fail(fmt.Errorf("capitalpath: order %s FACT crossed tenant boundary", c.orderID))
			return nil
		}
		if env.GetEventType() != subject {
			c.fail(fmt.Errorf("capitalpath: subject %s carried event_type %s", subject, env.GetEventType()))
			return nil
		}

		var err error
		switch subject {
		case subjectAccepted:
			err = c.accepted(payload)
		case subjectRouted:
			err = c.routed(payload)
		case fillfact.SubjectPartiallyFilled:
			err = c.filled(payload, env.GetEventId(), false)
		case fillfact.SubjectFilled:
			err = c.filled(payload, env.GetEventId(), true)
		case subjectPortfolioCash:
			err = c.accounted(payload, env.GetCausationId())
		case subjectRejected:
			var event orderpb.OrderRejected
			if unmarshalErr := proto.Unmarshal(payload, &event); unmarshalErr != nil {
				err = fmt.Errorf("decode ORDER_REJECTED: %w", unmarshalErr)
			} else {
				err = fmt.Errorf("capitalpath: order %s was rejected: %s (%s)", c.orderID, event.GetReason(), event.GetErrorCode())
			}
		default:
			err = fmt.Errorf("capitalpath: unsupported lifecycle subject %s", subject)
		}
		if err != nil {
			c.fail(err)
		} else {
			c.signal()
		}
		return nil
	}
}

func (c *factCollector) accepted(payload []byte) error {
	var event orderpb.OrderAccepted
	if err := proto.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("capitalpath: decode ORDER_ACCEPTED: %w", err)
	}
	if event.GetOrderId() != c.orderID || event.GetState().GetOrderId() != c.orderID {
		return fmt.Errorf("capitalpath: ORDER_ACCEPTED identity disagrees with envelope for %s", c.orderID)
	}
	if event.GetState().GetPortfolioId() != c.cfg.portfolio {
		return fmt.Errorf("capitalpath: ORDER_ACCEPTED portfolio %q, want %q", event.GetState().GetPortfolioId(), c.cfg.portfolio)
	}
	if c.line.observe(factAccepted) {
		return fmt.Errorf("capitalpath: duplicate ORDER_ACCEPTED for %s", c.orderID)
	}
	return nil
}

func (c *factCollector) routed(payload []byte) error {
	var event orderpb.OrderRouted
	if err := proto.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("capitalpath: decode ORDER_ROUTED: %w", err)
	}
	if event.GetOrderId() != c.orderID {
		return fmt.Errorf("capitalpath: ORDER_ROUTED identity disagrees with envelope for %s", c.orderID)
	}
	if event.GetVenue() != c.cfg.venue || event.GetVenueOrderId() == "" {
		return fmt.Errorf("capitalpath: ORDER_ROUTED venue/ack = %q/%q, want %q/non-empty", event.GetVenue(), event.GetVenueOrderId(), c.cfg.venue)
	}
	if c.line.observe(factRouted) {
		return fmt.Errorf("capitalpath: duplicate ORDER_ROUTED for %s", c.orderID)
	}
	return nil
}

func (c *factCollector) filled(payload []byte, eventID string, final bool) error {
	var (
		orderID, portfolio string
		fill               *orderpb.Fill
		state              *orderpb.OrderState
	)
	if final {
		var event orderpb.OrderFilled
		if err := proto.Unmarshal(payload, &event); err != nil {
			return fmt.Errorf("capitalpath: decode ORDER_FILLED: %w", err)
		}
		if field, in := dec.InDomainDeep(&event); !in {
			return fmt.Errorf("capitalpath: ORDER_FILLED carries an out-of-domain decimal at %s", field)
		}
		state = event.GetState()
		orderID, portfolio, fill = event.GetOrderId(), state.GetPortfolioId(), event.GetFill()
	} else {
		var event orderpb.OrderPartiallyFilled
		if err := proto.Unmarshal(payload, &event); err != nil {
			return fmt.Errorf("capitalpath: decode ORDER_PARTIALLY_FILLED: %w", err)
		}
		if field, in := dec.InDomainDeep(&event); !in {
			return fmt.Errorf("capitalpath: ORDER_PARTIALLY_FILLED carries an out-of-domain decimal at %s", field)
		}
		state = event.GetState()
		orderID, portfolio, fill = event.GetOrderId(), state.GetPortfolioId(), event.GetFill()
	}
	if err := fillfact.Validate(fill); err != nil {
		return fmt.Errorf("capitalpath: invalid fill FACT: %w", err)
	}
	if orderID != c.orderID || fill.GetOrderId() != c.orderID {
		return fmt.Errorf("capitalpath: fill identity disagrees with envelope for %s", c.orderID)
	}
	if portfolio != c.cfg.portfolio || fill.GetInstrumentId() != c.cfg.instrument || fill.GetVenue() != c.cfg.venue {
		return fmt.Errorf("capitalpath: fill portfolio/instrument/venue does not match submitted intent")
	}
	if state.GetOrderId() != c.orderID {
		return fmt.Errorf("capitalpath: fill state identity disagrees with envelope for %s", c.orderID)
	}
	if fill.GetSide() != orderpb.Side_SIDE_BUY {
		return fmt.Errorf("capitalpath: fill side %s does not match BUY intent", fill.GetSide())
	}
	if fill.GetVenueAccountId() != c.cfg.account {
		return fmt.Errorf("capitalpath: fill venue_account_id %q, want %q", fill.GetVenueAccountId(), c.cfg.account)
	}
	if !dec.IsPositive(fill.GetPrice()) || !fill.GetExecutedAt().IsValid() {
		return errors.New("capitalpath: fill price and executed_at must be valid venue observations")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if eventID == "" {
		return errors.New("capitalpath: fill FACT has no event_id, so accounting causation cannot be proven")
	}
	for i := 0; i < c.fillCount; i++ {
		if c.fills[i].GetFillId() == fill.GetFillId() {
			return fmt.Errorf("capitalpath: duplicate fill_id %q", fill.GetFillId())
		}
	}
	if c.fillCount == maxObservedFills {
		return fmt.Errorf("capitalpath: more than %d fills observed; proof is deliberately bounded", maxObservedFills)
	}
	c.fills[c.fillCount] = fill
	c.fillEvents[c.fillCount] = eventID
	c.fillCount++
	if final && c.line.observe(factFilled) {
		return fmt.Errorf("capitalpath: duplicate ORDER_FILLED for %s", c.orderID)
	}
	if final {
		c.terminalState = state
	}
	c.markAccountedLocked()
	return nil
}

func (c *factCollector) accounted(payload []byte, causationID string) error {
	var event accountingpb.PortfolioCashBalance
	if err := proto.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("capitalpath: decode accounting balance: %w", err)
	}
	if event.GetPortfolioId() != c.cfg.portfolio {
		return fmt.Errorf("capitalpath: accounting balance portfolio %q, want %q", event.GetPortfolioId(), c.cfg.portfolio)
	}
	if causationID == "" {
		return errors.New("capitalpath: accounting post-commit FACT has no causation_id")
	}
	c.mu.Lock()
	if !c.liveArmed {
		c.mu.Unlock()
		return nil
	}
	for i := 0; i < c.accountedCount; i++ {
		if c.accountedCauses[i] == causationID {
			c.mu.Unlock()
			return nil
		}
	}
	if c.accountedCount == maxObservedFills {
		c.mu.Unlock()
		return fmt.Errorf("capitalpath: more than %d post-arm accounting causes observed; proof is deliberately bounded", maxObservedFills)
	}
	c.accountedCauses[c.accountedCount] = causationID
	c.accountedCount++
	c.markAccountedLocked()
	c.mu.Unlock()
	return nil
}

func (c *factCollector) markAccountedLocked() {
	if c.fillCount == 0 {
		return
	}
	for i := 0; i < c.fillCount; i++ {
		matched := false
		for j := 0; j < c.accountedCount; j++ {
			if c.fillEvents[i] == c.accountedCauses[j] {
				matched = true
				break
			}
		}
		if !matched {
			return
		}
	}
	c.line.observe(factAccounted)
}

func (c *factCollector) fail(err error) {
	c.mu.Lock()
	if c.failure == nil {
		c.failure = err
	}
	c.mu.Unlock()
	c.signal()
}

func (c *factCollector) signal() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *factCollector) await(ctx context.Context) ([]*orderpb.Fill, error) {
	for {
		c.mu.Lock()
		failure := c.failure
		c.mu.Unlock()
		if failure != nil {
			return nil, failure
		}
		if err := c.line.complete(); err == nil {
			c.mu.Lock()
			fills := slices.Clone(c.fills[:c.fillCount])
			terminal := c.terminalState
			c.mu.Unlock()
			want, parseErr := positiveDecimal("quantity", c.cfg.quantity)
			if parseErr != nil {
				return nil, parseErr
			}
			total := new(big.Rat)
			for _, fill := range fills {
				total.Add(total, dec.FromProto(fill.GetQuantity()))
			}
			if total.Cmp(want) != 0 {
				return nil, fmt.Errorf("capitalpath: executed quantity %s does not equal submitted quantity %s", total.RatString(), want.RatString())
			}
			if terminal == nil || terminal.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED ||
				dec.FromProto(terminal.GetOrderedQuantity()).Cmp(want) != 0 ||
				dec.FromProto(terminal.GetFilledQuantity()).Cmp(want) != 0 || !dec.IsZero(terminal.GetLeavesQuantity()) {
				return nil, errors.New("capitalpath: terminal OrderState does not prove FILLED with exact ordered/filled quantity and zero leaves")
			}
			slices.SortFunc(fills, func(a, b *orderpb.Fill) int {
				if byTime := a.GetExecutedAt().AsTime().Compare(b.GetExecutedAt().AsTime()); byTime != 0 {
					return byTime
				}
				if a.GetFillId() < b.GetFillId() {
					return -1
				}
				if a.GetFillId() > b.GetFillId() {
					return 1
				}
				return 0
			})
			return fills, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.changed:
		}
	}
}
