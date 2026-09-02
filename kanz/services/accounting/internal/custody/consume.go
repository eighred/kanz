package custody

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"

	"google.golang.org/protobuf/proto"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// StatementConsumer folds accounting.v1.CustodianStatement FACTs into the store.
//
// IT STORES AND DOES NOT RECONCILE. The reconciliation is the SCHEDULER's job,
// and separating them is deliberate: a control that only runs when a statement
// arrives cannot report that one did not, which is the failure this whole issue
// is about. Reconciling here as well would also mean a custodian resending a
// statement six times produced six runs of identical evidence, burying the daily
// record under redelivery noise.
type StatementConsumer struct {
	store  Store
	tenant string
	logger *slog.Logger
}

// NewStatementConsumer wires a consumer over store, pinned to tenant.
//
// THE TENANT IS REQUIRED AND IS NOT DEFAULTED. This consumer writes through an
// RLS pool scoped to one tenant, so a statement from another tenant folded here
// would put one fund's custodian holdings into another's reconciliation — a
// cross-tenant read of exactly the kind tenant isolation is deny-by-default
// about. An empty tenant would make the check below pass everything, so it is
// refused at construction rather than at the first delivery.
func NewStatementConsumer(store Store, tenant string, logger *slog.Logger) (*StatementConsumer, error) {
	if store == nil {
		return nil, errors.New("custody: nil store")
	}
	if tenant == "" {
		return nil, errors.New("custody: statement consumer needs a tenant scope")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &StatementConsumer{store: store, tenant: tenant, logger: logger}, nil
}

// Handle folds one statement delivery.
//
// A MALFORMED STATEMENT IS REFUSED, NOT REPAIRED, and the refusal is what routes
// it to the DLQ where an operator sees it. The tempting alternative — accept what
// parsed and drop the rest — would reconcile a book against a PARTIAL level, and
// a partial level read as complete manufactures a MISSING_AT_CUSTODIAN break for
// every position it omitted. A screen of fabricated breaks is worse than no
// reconciliation, because it buries the real ones and teaches the operator that
// the queue is noise.
func (c *StatementConsumer) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	// This consumer writes through an RLS pool pinned to c.tenant, so an envelope
	// from another tenant would fold one fund's custodian holdings into another's
	// book (the same guard consume.Folder.Handle takes, #223).
	if err := bus.RequireTenantScope(env.GetTenantId(), c.tenant); err != nil {
		return err
	}
	var msg accountingpb.CustodianStatement
	if err := proto.Unmarshal(payload, &msg); err != nil {
		return fmt.Errorf("custody: unmarshal statement: %w", err)
	}
	stmt, err := statementFromProto(&msg)
	if err != nil {
		return err
	}
	if err := c.store.SaveStatement(ctx, stmt); err != nil {
		return fmt.Errorf("custody: save statement %s: %w", stmt.StatementID, err)
	}
	c.logger.Info("accounting: custodian statement stored",
		"statement", stmt.StatementID, "custodian", stmt.CustodianID,
		"portfolio", stmt.PortfolioID, "business_date", BusinessDay(stmt.BusinessDate).Format("2006-01-02"),
		"positions", len(stmt.Positions), "currencies", len(stmt.Cash))
	return nil
}

// statementFromProto converts a wire statement to the domain shape.
//
// EVERY FIGURE IS CHECKED, and an unrepresentable one refuses the whole
// statement rather than being dropped from it. A statement missing one position
// because its quantity would not decode is a partial level with no marker saying
// so — see Handle for why that is the worst available outcome.
func statementFromProto(msg *accountingpb.CustodianStatement) (Statement, error) {
	if msg == nil {
		return Statement{}, errors.New("custody: nil statement")
	}
	stmt := Statement{
		StatementID: msg.GetStatementId(),
		CustodianID: msg.GetCustodianId(),
		PortfolioID: msg.GetPortfolioId(),
	}
	if bd := msg.GetBusinessDate(); bd != nil {
		stmt.BusinessDate = BusinessDay(bd.AsTime())
	}
	if ra := msg.GetReceivedAt(); ra != nil {
		stmt.ReceivedAt = ra.AsTime().UTC()
	}
	if n := len(msg.GetPositions()); n > 0 {
		stmt.Positions = make(map[string]*big.Rat, n)
	}
	for _, p := range msg.GetPositions() {
		instrument := p.GetInstrumentId()
		if instrument == "" {
			return Statement{}, fmt.Errorf("custody: statement %s has a position with no instrument_id", stmt.StatementID)
		}
		if _, dup := stmt.Positions[instrument]; dup {
			// TWO ROWS FOR ONE INSTRUMENT IS AMBIGUOUS, not additive. Silently
			// summing them would invent a holding the custodian never asserted,
			// and taking the last would discard one it did. Neither is a level
			// anybody can reconcile against.
			return Statement{}, fmt.Errorf("custody: statement %s names instrument %s twice", stmt.StatementID, instrument)
		}
		qty, ok := dec.FromProtoChecked(p.GetQuantity())
		if !ok {
			return Statement{}, fmt.Errorf("custody: statement %s: quantity for %s is not a valid Decimal", stmt.StatementID, instrument)
		}
		stmt.Positions[instrument] = qty
	}
	if n := len(msg.GetCash()); n > 0 {
		stmt.Cash = make(map[string]*big.Rat, n)
	}
	for _, ccy := range msg.GetCash() {
		code := ccy.GetCurrencyCode()
		if code == "" {
			return Statement{}, fmt.Errorf("custody: statement %s has a cash row with no currency_code", stmt.StatementID)
		}
		if _, dup := stmt.Cash[code]; dup {
			return Statement{}, fmt.Errorf("custody: statement %s names currency %s twice", stmt.StatementID, code)
		}
		bal, ok := dec.FromProtoChecked(ccy.GetBalance())
		if !ok {
			return Statement{}, fmt.Errorf("custody: statement %s: balance for %s is not a valid Decimal", stmt.StatementID, code)
		}
		stmt.Cash[code] = bal
	}
	if err := stmt.Validate(); err != nil {
		return Statement{}, err
	}
	return stmt, nil
}
