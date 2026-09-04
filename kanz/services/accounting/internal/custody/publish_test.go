package custody_test

// THE ENVELOPE, NOT THE ENCODER (#245, #962).
//
// Every other test in this package publishes through a double that satisfies
// custody.Publisher — an Event-level fake, which is exactly the shape that
// pkg/bus/producer.go records failing on the first real broker: "TWELVE publish
// sites … validated fine in unit tests (which inject fake Publishers that never
// validate) and failed on the first real broker." A double at the Event level
// proves nothing about the envelope, because it is precisely bus.Validate that it
// skips.
//
// These are Tier-B: a REAL bus.Producer over a fake bus.Client. The fake sits at
// the TRANSPORT level and receives wire bytes, so stamping, Validate and framing
// all really run and only the socket is mocked.
//
// WHAT IT WOULD HAVE CAUGHT HERE SPECIFICALLY. A ReconciliationRun is published
// from a TICKER, not from an inbound delivery, so there is no ctx tenant for the
// producer to inherit — the ProducerConfig.Tenant fallback is the only source of
// one, and without it bus.Validate refuses every publish with "tenant_id
// required". That is the failure that crash-looped the OMS and silently stopped
// accounting's cash movements reaching NAV; a scheduled reconciliation would hit
// it on its first tick, in production, having passed every unit test.

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

var runAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// captureClient is a bus.Client that records the framed wire bytes. It sits
// BELOW the Producer so Validate is on the path.
type captureClient struct {
	mu   sync.Mutex
	sent []bus.Message
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func (c *captureClient) messages() []bus.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.Message(nil), c.sent...)
}

// newRealProducerReconciler builds the Tier-B rig: a real Producer configured the
// way the composition root configures it (cashProducerConfig in
// services/accounting/cmd/accounting/main.go), over a capturing transport.
func newRealProducerReconciler(t *testing.T, cfg bus.ProducerConfig) (*custody.Reconciler, custody.Store, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, cfg)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	store := custody.NewMemoryStore()
	book := ledger.NewBook("PF1")
	book.Apply(&ledger.Event{
		EntryID: "e1", PortfolioID: "PF1", Type: ledger.EntryTrade, InstrumentID: "AAPL",
		Quantity: big.NewRat(100, 1), Price: big.NewRat(1, 1), Cash: new(big.Rat),
		CashCurrency: "USD", Effective: runAt, Knowledge: runAt,
	})
	r, err := custody.NewReconciler(store,
		func(context.Context, custody.Subject) (*ledger.Book, error) { return book, nil },
		prod, new(big.Rat), nil, nil, func() time.Time { return runAt })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	return r, store, cc
}

func productionProducerConfig() bus.ProducerConfig {
	return bus.ProducerConfig{Source: "accounting", ProducerVersion: "test", Tenant: "acme"}
}

func reconcileSubject() custody.Subject {
	return custody.Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: runAt}
}

func seedStatement(t *testing.T, store custody.Store, positions map[string]*big.Rat) {
	t.Helper()
	err := store.SaveStatement(context.Background(), custody.Statement{
		StatementID: "S1", CustodianID: "CUST-A", PortfolioID: "PF1",
		BusinessDate: custody.BusinessDay(runAt), ReceivedAt: runAt,
		Positions: positions,
	})
	if err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
}

// decodeOne unframes the single captured message and returns its envelope and
// payload bytes. bus.Unframe is the real receive-side path — the payload is
// FRAMED alongside the envelope rather than nested inside it, so decoding it any
// other way would test a shape the broker never carries.
func decodeOne(t *testing.T, cc *captureClient) (*envelopepb.Envelope, []byte) {
	t.Helper()
	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	env, payload, err := bus.Unframe(msgs[0].Body)
	if err != nil {
		t.Fatalf("unframe: %v", err)
	}
	return env, payload
}

// THE ENVELOPE A REAL PRODUCER STAMPS MUST PASS bus.Validate.
func TestReconciliationRunEnvelopeValidates(t *testing.T) {
	r, store, cc := newRealProducerReconciler(t, productionProducerConfig())
	seedStatement(t, store, map[string]*big.Rat{"AAPL": big.NewRat(100, 1)})

	if _, err := r.Reconcile(context.Background(), reconcileSubject()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	env, payload := decodeOne(t, cc)
	if err := bus.Validate(env); err != nil {
		t.Fatalf("bus.Validate refused the envelope a real producer stamped: %v", err)
	}
	if env.GetTenantId() != "acme" {
		t.Fatalf("tenant_id = %q, want acme", env.GetTenantId())
	}
	if env.GetEventType() != custody.SubjectRun {
		t.Fatalf("event_type = %q, want %q", env.GetEventType(), custody.SubjectRun)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Fatalf("event_class = %v, want FACT", env.GetEventClass())
	}
	if got := cc.messages()[0].Subject; got != custody.SubjectRun {
		t.Fatalf("subject = %q, want %q", got, custody.SubjectRun)
	}
	// The payload must decode as the message the schema ref names.
	var run accountingpb.ReconciliationRun
	if err := proto.Unmarshal(payload, &run); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if run.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_CLEAN {
		t.Fatalf("outcome = %s, want CLEAN", run.GetOutcome())
	}
	// DERIVED FROM THE PAYLOAD, not retyped: the schema ref must actually
	// describe the message on the wire, or a consumer routing on it decodes the
	// wrong type.
	if want := string(run.ProtoReflect().Descriptor().FullName()) + ":1"; env.GetPayloadSchemaRef() != want {
		t.Fatalf("PayloadSchemaRef = %q does not describe the payload (%q)", env.GetPayloadSchemaRef(), want)
	}
}

// A RUN IS PUBLISHED FROM A TICKER, so ProducerConfig.Tenant is the ONLY source
// of a tenant. Configured without it, bus.Validate refuses every publish — the
// scheduled reconciliation would fail on its first tick, in production, having
// passed every Event-level unit test in this package.
func TestAProducerWithNoTenantFallbackIsRefusedByValidate(t *testing.T) {
	cfg := productionProducerConfig()
	cfg.Tenant = ""
	r, store, cc := newRealProducerReconciler(t, cfg)
	seedStatement(t, store, map[string]*big.Rat{"AAPL": big.NewRat(100, 1)})

	_, err := r.Reconcile(context.Background(), reconcileSubject())
	if err == nil {
		t.Fatal("a tenantless producer published a run — bus.Validate was not on the path")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "tenant") {
		t.Fatalf("error = %v, want one naming the missing tenant", err)
	}
	if len(cc.messages()) != 0 {
		t.Fatalf("%d messages reached the transport despite a refused validation", len(cc.messages()))
	}
}

// THE no_statement RUN IS THE ONE THE ESTATE MOST NEEDS, and it is emitted on a
// path that never touches the ledger — so its envelope is proven separately
// rather than assumed to match the clean one's.
func TestNoStatementRunEnvelopeValidates(t *testing.T) {
	r, _, cc := newRealProducerReconciler(t, productionProducerConfig())

	run, err := r.Reconcile(context.Background(), reconcileSubject())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if run.Outcome != custody.OutcomeNoStatement {
		t.Fatalf("outcome = %s, want no_statement", run.Outcome)
	}
	env, payload := decodeOne(t, cc)
	if err := bus.Validate(env); err != nil {
		t.Fatalf("bus.Validate refused the no_statement envelope: %v", err)
	}
	var msg accountingpb.ReconciliationRun
	if err := proto.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if msg.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_NO_STATEMENT {
		t.Fatalf("outcome = %s, want NO_STATEMENT", msg.GetOutcome())
	}
	if msg.GetStatementId() != "" {
		t.Fatalf("statement_id = %q, want empty on a no_statement run", msg.GetStatementId())
	}
}

// THE PARTITION KEY IS THE PAIR, which is the granularity a consumer folds at
// and the ordering unit the staleness gauge is keyed on. Keying on the run id
// would order nothing that matters.
func TestRunPartitionKeyIsThePortfolioCustodianPair(t *testing.T) {
	r, store, cc := newRealProducerReconciler(t, productionProducerConfig())
	seedStatement(t, store, map[string]*big.Rat{"AAPL": big.NewRat(100, 1)})
	if _, err := r.Reconcile(context.Background(), reconcileSubject()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	env, _ := decodeOne(t, cc)
	if got, want := env.GetPartitionKey(), "PF1|CUST-A"; got != want {
		t.Fatalf("partition_key = %q, want %q", got, want)
	}
}
