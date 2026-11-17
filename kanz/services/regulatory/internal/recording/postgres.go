// Package recording commits a filing, its context witness and its FACTs in one
// transaction. A broker outage retains that committed identity for relay/retry.
package recording

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eighred/kanz/internal/audit/linkstore"
	"github.com/eighred/kanz/internal/audit/signer"
	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/outbox"
	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	sustainabilitypublish "github.com/eighred/kanz/internal/sustainability/publish"
	regulatorypb "github.com/eighred/kanz/kanz-schemas-go/regulatory/v1"
	sustainabilitypb "github.com/eighred/kanz/kanz-schemas-go/sustainability/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var ErrUnpublished = errors.New("filing recorded; its FACTs are pending publication, retry the identical request")
var ErrInvalidInput = errors.New("filing input refused")

const maxPublicationRecords = 2*sustainabilitypublish.MaxHoldings + 2
const relayBatch = maxPublicationRecords + 1

// Plan.Build calls the delivered File* assembler with this transaction's signer.
// It runs only for a new semantic request, never on duplicate/restart recovery.
type Plan struct {
	Tenant, Portfolio, Framework, InputDigest string
	AsOf                                      time.Time
	Build                                     func(filing.PublicationScope, filing.Signer) (*regulatorypb.FilingRecordBody, map[string]any, error)
}

type Recorder interface {
	Record(context.Context, Plan) ([]byte, error)
}

// Publisher's tenant is fixed at construction, matching storage and the
// deployment's workload/broker account identity. Requests cannot select it.
type Publisher interface {
	outbox.Publisher
	Tenant() string
}

type Postgres struct {
	pool                  *pgxpool.Pool
	tenant, signatureKind string
	links                 *linkstore.Postgres
	relay                 *outbox.Relay
}

func New(pool *pgxpool.Pool, tenant, signatureKind string, publisher Publisher, logger *slog.Logger) (*Postgres, error) {
	if pool == nil || !filing.BoundedIdentifier(tenant) || (signatureKind != "chain" && signatureKind != "hash") || publisher == nil || publisher.Tenant() != tenant {
		return nil, errors.New("filing recording: scoped pool, tenant, signature kind and publisher required")
	}
	// A complete request has at most two observations per instrument, one
	// aggregate and one report. Flush must not report success with a tail pending.
	queue := serialDrainQueue{Queue: outbox.NewPostgres(pool, outbox.ServiceRegulatory), slot: make(chan struct{}, 1)}
	relay, err := newRelay(queue, retainedPublisher{pool: pool, tenant: tenant, publisher: publisher}, logger)
	if err != nil {
		return nil, err
	}
	return &Postgres{pool: pool, tenant: tenant, signatureKind: signatureKind, links: linkstore.NewPostgres(pool), relay: relay}, nil
}

func newRelay(queue outbox.Queue, publisher outbox.Publisher, logger *slog.Logger) (*outbox.Relay, error) {
	// A full canonical batch means UNKNOWN tail, even if it happened to be the
	// last record. Leave one slot beyond the maximum request; do not weaken the
	// canonical relay's refusal for a genuinely full/backlogged batch.
	return outbox.NewRelay(queue, publisher, logger, outbox.WithBatch(relayBatch))
}

func (p *Postgres) Run(ctx context.Context) error { return p.relay.Run(ctx) }

// A drain lock pins a database connection. Serialize this deployment's drains
// before acquiring one: concurrent HTTP flushes must not fill the whole pool
// with lock waiters while its winner needs a connection to read/mark records.
// Cross-process ordering remains the canonical queue's database advisory lock.
type serialDrainQueue struct {
	outbox.Queue
	slot chan struct{}
}

func (q serialDrainQueue) LockKey(ctx context.Context, key string, wait bool) (func(), bool, error) {
	if wait {
		select {
		case q.slot <- struct{}{}:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	} else {
		select {
		case q.slot <- struct{}{}:
		default:
			return nil, false, nil
		}
	}
	release, won, err := q.Queue.LockKey(ctx, key, wait)
	if err != nil || !won {
		<-q.slot
		return nil, won, err
	}
	return func() { release(); <-q.slot }, true, nil
}

// Pending outbox rows are operationally mutable (attempts/publication marks).
// Verify the payload against the separately immutable bound artifact before
// granting it the stored domain identity, including after a process restart.
type retainedPublisher struct {
	pool      *pgxpool.Pool
	tenant    string
	publisher outbox.Publisher
}

func (p retainedPublisher) Publish(ctx context.Context, event bus.Event) error {
	var binding string
	switch payload := event.Payload.(type) {
	case *regulatorypb.FilingRecorded:
		binding = payload.GetBindingHash()
	case *sustainabilitypb.InstrumentObserved:
		binding = payload.GetContextBindingHash()
	case *sustainabilitypb.PortfolioAggregated:
		binding = payload.GetContextBindingHash()
	default:
		return errors.New("filing recording: unsupported outbox payload")
	}
	if event.TenantID != p.tenant || !filing.Digest(binding) {
		return errors.New("filing recording: outbox scope mismatch")
	}
	var artifact, witness []byte
	var hash, previous string
	if err := p.pool.QueryRow(ctx, `SELECT a.artifact,a.binding_hash,l.prev_hash,l.canonical FROM regulatory_filing_artifacts a JOIN audit_chain_links l ON l.hash=a.binding_hash WHERE a.tenant_id=$1 AND a.binding_hash=$2`, p.tenant, binding).Scan(&artifact, &hash, &previous, &witness); err != nil {
		return errors.New("filing recording: bound artifact unavailable")
	}
	retained, err := validateRetained(artifact, hash, previous, witness)
	if err != nil {
		return err
	}
	matched := false
	switch payload := event.Payload.(type) {
	case *regulatorypb.FilingRecorded:
		matched = proto.Equal(payload, retained)
	case *sustainabilitypb.InstrumentObserved:
		for _, observation := range retained.Body.InstrumentObservations {
			if proto.Equal(payload.Observation, observation) {
				matched = true
				break
			}
		}
	case *sustainabilitypb.PortfolioAggregated:
		matched = payload.Observation != nil && proto.Equal(payload.Observation, retained.Body.PortfolioObservation)
	}
	if !matched {
		return errors.New("filing recording: outbox payload differs from its bound artifact")
	}
	return p.publisher.Publish(ctx, event)
}

func (p *Postgres) Record(ctx context.Context, plan Plan) ([]byte, error) {
	if p == nil || plan.Tenant != p.tenant || !filing.BoundedIdentifier(plan.Portfolio) || !filing.Digest(plan.InputDigest) || plan.AsOf.IsZero() || plan.Build == nil {
		return nil, errors.New("filing recording: invalid request scope")
	}
	switch plan.Framework {
	case "FRTB", "FORM_PF", "AIFMD", "TCFD", "SFDR":
	default:
		return nil, errors.New("filing recording: unsupported framework")
	}
	// JSON's fixed struct is length framed and unambiguous; it contains known
	// identity fields only, never headers or opaque authentication material.
	keyInput, err := json.Marshal(struct{ Tenant, Portfolio, Framework, AsOf, Input string }{plan.Tenant, plan.Portfolio, plan.Framework, plan.AsOf.UTC().Format(time.RFC3339Nano), plan.InputDigest})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(keyInput)
	key := hex.EncodeToString(digest[:])
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var actualTenant string
	if err := tx.QueryRow(ctx, `SELECT app_current_tenant()`).Scan(&actualTenant); err != nil || actualTenant != p.tenant {
		return nil, errors.New("filing recording: pool tenant differs from publisher tenant")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "regulatory/filing/"+p.tenant+"/"+key); err != nil {
		return nil, err
	}
	var artifact, response, witness []byte
	var hash, previous string
	err = tx.QueryRow(ctx, `SELECT a.artifact,a.response,a.binding_hash,l.prev_hash,l.canonical FROM regulatory_filing_artifacts a JOIN audit_chain_links l ON l.hash=a.binding_hash WHERE a.tenant_id=$1 AND a.semantic_key=$2`, p.tenant, key).Scan(&artifact, &response, &hash, &previous, &witness)
	if err == nil {
		retained, err := validateRetained(artifact, hash, previous, witness)
		if err != nil {
			return nil, err
		}
		b := retained.Body
		decoded, decodeErr := regulatorypublish.FromWire(b.Report)
		if decodeErr != nil || decoded.Framework != plan.Framework || b.TenantId != plan.Tenant || b.PortfolioId != plan.Portfolio || b.InputManifestDigest != plan.InputDigest || !b.Report.AsOf.AsTime().Equal(plan.AsOf) || len(response) > regulatorypublish.MaxBodyBytes || !json.Valid(response) {
			return nil, errors.New("filing recording: retained artifact scope mismatch")
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return p.flush(ctx, plan, response)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	scope := filing.PublicationScope{Tenant: plan.Tenant, Portfolio: plan.Portfolio, ReportID: id.String(), InputDigest: plan.InputDigest, AsOf: plan.AsOf, RecordedAt: time.Now().UTC()}
	signing := &transactionSigner{ctx: ctx, tx: tx, links: p.links, kind: p.signatureKind}
	body, output, err := plan.Build(scope, signing)
	if err != nil {
		if signing.failure != nil {
			return nil, errors.New("filing recording: original signature transaction failed")
		}
		return nil, fmt.Errorf("%w: %s", ErrInvalidInput, err)
	}
	if signing.calls != 1 || body == nil || body.Report == nil || body.Report.ReportId != scope.ReportID || body.Report.Signature != signing.signature || !bytes.Equal(body.FilingCanonical, signing.canonical) {
		return nil, errors.New("filing recording: assembler did not use the transaction signer exactly once")
	}
	decoded, decodeErr := regulatorypublish.FromWire(body.Report)
	if decodeErr != nil || decoded.Framework != plan.Framework || body.TenantId != scope.Tenant || body.PortfolioId != scope.Portfolio || body.InputManifestDigest != scope.InputDigest || !decoded.AsOf.Equal(scope.AsOf) || body.RecordedAt == nil || !body.RecordedAt.AsTime().Equal(scope.RecordedAt) {
		return nil, errors.New("filing recording: assembler changed the requested scope")
	}
	body.SignatureKind, body.FilingPrevHash = signing.wireKind(), signing.link.Prev
	canonical, err := regulatorypublish.BindingCanonical(body)
	if err != nil {
		return nil, err
	}
	link, err := p.links.AppendChainedTx(ctx, tx, canonical)
	if err != nil {
		return nil, err
	}
	recorded, err := regulatorypublish.Recorded(body, link)
	if err != nil {
		return nil, err
	}
	artifact, err = (proto.MarshalOptions{Deterministic: true}).Marshal(recorded)
	if err != nil {
		return nil, err
	}
	if output == nil {
		return nil, errors.New("filing recording: missing filing response")
	}
	output["report_id"], output["context_binding_hash"] = scope.ReportID, link.Cur
	output["recorded_at"] = scope.RecordedAt
	response, err = json.Marshal(output)
	if err != nil || len(response) > regulatorypublish.MaxBodyBytes {
		return nil, errors.New("filing recording: invalid response")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO regulatory_filing_artifacts (tenant_id,semantic_key,report_id,portfolio_id,framework,as_of,input_digest,binding_hash,artifact,response) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, p.tenant, key, scope.ReportID, scope.Portfolio, plan.Framework, scope.AsOf, scope.InputDigest, link.Cur, artifact, response); err != nil {
		return nil, err
	}
	events := make([]bus.Event, 0, len(body.InstrumentObservations)+2)
	for _, observation := range body.InstrumentObservations {
		event, err := sustainabilitypublish.Event(&sustainabilitypb.InstrumentObserved{Observation: observation, ContextBindingHash: link.Cur})
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if body.PortfolioObservation != nil {
		event, err := sustainabilitypublish.Event(&sustainabilitypb.PortfolioAggregated{Observation: body.PortfolioObservation, ContextBindingHash: link.Cur})
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	event, err := regulatorypublish.Event(recorded)
	if err != nil {
		return nil, err
	}
	events = append(events, event)
	records := make([]outbox.Record, 0, len(events))
	for _, event := range events {
		if _, err := tx.Exec(ctx, `INSERT INTO regulatory_filing_publications (publication_id,tenant_id,semantic_key) VALUES($1,$2,$3)`, event.EventID, p.tenant, key); err != nil {
			return nil, err
		}
		record, err := outbox.From(ctx, event)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := outbox.Enqueue(ctx, tx, outbox.ServiceRegulatory, records...); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return p.flush(ctx, plan, response)
}

func validateRetained(artifact []byte, hash, previous string, witness []byte) (*regulatorypb.FilingRecorded, error) {
	retained := new(regulatorypb.FilingRecorded)
	if len(artifact) > regulatorypublish.MaxBodyBytes+1024 || proto.Unmarshal(artifact, retained) != nil {
		return nil, errors.New("filing recording: invalid retained artifact")
	}
	if _, err := regulatorypublish.Event(retained); err != nil {
		return nil, err
	}
	expected, err := regulatorypublish.BindingCanonical(retained.Body)
	if err != nil || retained.BindingHash != hash || retained.BindingPrevHash != previous || !bytes.Equal(expected, witness) {
		return nil, errors.New("filing recording: retained preimage differs from the committed chain witness")
	}
	return retained, nil
}

func (p *Postgres) flush(ctx context.Context, plan Plan, response []byte) ([]byte, error) {
	if _, err := p.relay.Flush(ctx, plan.Tenant+"/"+plan.Portfolio); err != nil {
		return nil, fmt.Errorf("%w: publication did not complete", ErrUnpublished)
	}
	return append([]byte(nil), response...), nil
}

type transactionSigner struct {
	ctx       context.Context
	tx        pgx.Tx
	links     *linkstore.Postgres
	kind      string
	calls     int
	canonical []byte
	signature string
	link      signer.Link
	failure   error
}

func (s *transactionSigner) Sign(body []byte) (string, error) {
	s.calls++
	if s.calls != 1 {
		return "", errors.New("filing recording: one original filing signature required")
	}
	s.canonical = append([]byte(nil), body...)
	if s.kind == "hash" {
		var err error
		s.signature, err = (filing.HashSigner{}).Sign(body)
		return s.signature, err
	}
	var err error
	s.link, err = s.links.AppendChainedTx(s.ctx, s.tx, body)
	if err != nil {
		s.failure = err
		return "", err
	}
	s.signature = s.link.Cur
	return s.signature, nil
}

func (s *transactionSigner) wireKind() regulatorypb.FilingSignatureKind {
	if s.kind == "hash" {
		return regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_CONTENT_HASH
	}
	return regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_AUDIT_CHAIN
}
