package collateralops

import (
	"context"
	"encoding/json"
	platformsubject "github.com/eighred/kanz/internal/platform/subject"

	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

const SubjectSnapshot = "accounting.collateral.snapshot"
const SubjectConfirmation = "accounting.collateral.confirmation"

// SourceScope is a deployment declaration, never supplied by the message being
// authorized. Broker ACLs must restrict each subject to its feed workload.
type SourceScope struct{ Source, Custodian, Account string }
type Consumer struct {
	store               *Store
	tenant, inputSource string
	custody             []SourceScope
}

func NewConsumer(store *Store, tenant, inputSource, scopeJSON string) (*Consumer, error) {
	var scopes []SourceScope
	if store == nil || !identifier(tenant) || !identifier(inputSource) || json.Unmarshal([]byte(scopeJSON), &scopes) != nil || len(scopes) == 0 || len(scopes) > 64 {
		return nil, ErrInput
	}
	for _, s := range scopes {
		if !identifier(s.Source) || !identifier(s.Custodian) || !identifier(s.Account) {
			return nil, ErrInput
		}
	}
	return &Consumer{store: store, tenant: tenant, inputSource: inputSource, custody: scopes}, nil
}
func (c *Consumer) HandleSnapshot(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), c.tenant); err != nil {
		return err
	}
	if env == nil || env.TenantId != c.tenant || env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || env.Source != c.inputSource || env.EventType != SubjectSnapshot || env.PayloadSchemaRef != "collateral.v1.WorkflowSnapshot:1" || len(payload) > 1<<20 {
		return ErrInput
	}
	snapshot := new(pb.WorkflowSnapshot)
	if err := proto.Unmarshal(payload, snapshot); err != nil {
		return err
	}
	return c.store.ImportSnapshot(ctx, c.tenant, snapshot)
}

// ConfirmationHandlers binds authority to the actual broker subscription. An
// envelope's self-declared Source cannot select another custodian's authority.
// Feed credentials must be granted only their own returned subject.
func (c *Consumer) ConfirmationHandlers() map[string]bus.EventHandler {
	handlers := map[string]bus.EventHandler{}
	for _, scope := range c.custody {
		source := scope.Source
		handlers[SubjectConfirmation+"."+platformsubject.Token(source)] = func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
			return c.handleConfirmation(ctx, source, env, payload)
		}
	}
	return handlers
}

func (c *Consumer) handleConfirmation(ctx context.Context, source string, env *envelopepb.Envelope, payload []byte) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), c.tenant); err != nil {
		return err
	}
	if env == nil || env.Source != source || env.TenantId != c.tenant || env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || env.EventType != SubjectConfirmation || env.PayloadSchemaRef != "collateral.v1.PostingConfirmation:1" || len(payload) > 16<<10 {
		return ErrInput
	}
	confirmation := new(pb.PostingConfirmation)
	if err := proto.Unmarshal(payload, confirmation); err != nil {
		return err
	}
	for _, scope := range c.custody {
		if env.Source == scope.Source && confirmation.CustodianId == scope.Custodian && confirmation.AccountId == scope.Account {
			return c.store.Confirm(ctx, c.tenant, confirmation)
		}
	}
	return ErrInput
}

func (s *Store) Portfolio(ctx context.Context, tenant, snapshotID, workflowID string) (string, error) {
	tx, err := s.begin(ctx, tenant)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if workflowID != "" {
		state, err := loadWorkflow(ctx, tx, workflowID)
		if err != nil {
			return "", err
		}
		snapshotID = state.SnapshotId
	}
	snapshot, err := loadSnapshot(ctx, tx, snapshotID)
	if err != nil {
		return "", err
	}
	return snapshot.PortfolioId, tx.Commit(ctx)
}
