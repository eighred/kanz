package returnbasis

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	performancepb "github.com/eighred/kanz/kanz-schemas-go/performance/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const Subject = "accounting.performance.daily_return"
const schema = "performance.v1.DailyReturnAssessment:1"

func assessment(r Receipt) *performancepb.DailyReturnAssessment {
	e := r.Evidence
	end, _ := time.Parse(time.DateOnly, e.CloseDate)
	out := &performancepb.DailyReturnAssessment{
		PortfolioId: e.Portfolio, Currency: e.Currency, SourceDigest: r.Digest, PreviousSourceDigest: e.PreviousDigest,
		Start: timestamppb.New(end.AddDate(0, 0, -1)), End: timestamppb.New(end), AsOf: timestamppb.New(e.AsOf),
	}
	if e.Opening != nil {
		out.OpeningNavReference = e.Opening.Reference
	}
	if e.Closing != nil {
		out.ClosingNavReference = e.Closing.Reference
	}
	if e.Flows != nil {
		out.FlowScope = e.Flows.Scope
		out.FlowCompletionReference = e.Flows.Reference
		out.ExternalFlowCount = e.Flows.EventCount
		if e.Flows.Split != nil {
			out.IntradayFlow = publishedFlow(*e.Flows.Split)
		}
		for _, split := range e.Flows.Splits {
			out.IntradayFlows = append(out.IntradayFlows, publishedFlow(split))
		}
	}
	fraction, reason := measure(e)
	if reason != "" {
		out.Result = &performancepb.DailyReturnAssessment_UnassessedReason{UnassessedReason: reason}
	} else {
		out.Result = &performancepb.DailyReturnAssessment_Measured{Measured: &performancepb.ReturnPeriod{
			PortfolioId: e.Portfolio, Start: out.Start, End: out.End, AsOf: out.AsOf,
			Method: performancepb.ReturnMethod_RETURN_METHOD_TIME_WEIGHTED, ReturnValue: fraction,
		}}
	}
	out.BenchmarkComparison = compareBenchmark(r, out)
	return out
}

func publishedFlow(split FlowSplit) *performancepb.IntradayReturnFlow {
	amount, _ := split.SignedAmount.Rat()
	exact, _ := dec.ToProtoExact(amount)
	out := &performancepb.IntradayReturnFlow{
		MovementId: split.MovementID, CommandDigest: split.CommandDigest,
		EffectiveTime: timestamppb.New(split.Effective), SignedAmount: exact,
	}
	if split.Before != nil {
		out.PreFlowNavReference = split.Before.Reference
	}
	if split.After != nil {
		out.PostFlowNavReference = split.After.Reference
	}
	return out
}

func measure(e Evidence) (float64, string) {
	exact, reason := exactReturn(e)
	if reason != "" {
		return 0, reason
	}
	fraction, _ := exact.Float64()
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) {
		return 0, "return_out_of_range"
	}
	return fraction, ""
}

func exactReturn(e Evidence) (*big.Rat, string) {
	switch {
	case e.Opening == nil || e.Closing == nil:
		return nil, "nav_missing"
	case e.Flows == nil || e.Flows.EventCount == nil:
		return nil, "flow_coverage_unknown"
	}
	bridges := e.Flows.bridges()
	if *e.Flows.EventCount != uint64(len(bridges)) {
		return nil, "external_flows_present"
	}
	opening, err := e.Opening.Total.Rat()
	if err != nil || opening.Sign() <= 0 {
		return nil, "opening_nav_nonpositive"
	}
	closing, err := e.Closing.Total.Rat()
	if err != nil {
		return nil, "nav_missing"
	}
	// Multiply invested subperiod growth in exact arithmetic. Each cash
	// movement changes only the base of the following subperiod.
	growth := big.NewRat(1, 1)
	base := opening
	for _, split := range bridges {
		if split.Before == nil || split.After == nil {
			return nil, "nav_at_flow_missing"
		}
		before, beforeErr := split.Before.Total.Rat()
		after, afterErr := split.After.Total.Rat()
		if beforeErr != nil || afterErr != nil {
			return nil, "nav_at_flow_missing"
		}
		if after.Sign() <= 0 {
			return nil, "post_flow_nav_nonpositive"
		}
		growth.Mul(growth, new(big.Rat).Quo(before, base))
		base = after
	}
	growth.Mul(growth, new(big.Rat).Quo(closing, base))
	return growth.Sub(growth, big.NewRat(1, 1)), ""
}

func lockJournal(ctx context.Context, tx pgx.Tx, portfolio string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext(app_current_tenant()),hashtext($1))`, portfolio)
	return err
}

func verifyBookedFlows(ctx context.Context, tx pgx.Tx, e Evidence) error {
	if e.Flows == nil {
		return nil
	}
	bridges := e.Flows.bridges()
	if len(bridges) == 0 {
		return nil
	}
	if err := lockJournal(ctx, tx, e.Portfolio); err != nil {
		return err
	}
	proofs := make([]cashmove.ExternalFlowProof, 0, len(bridges))
	for _, bridge := range bridges {
		amount, _ := bridge.SignedAmount.Rat()
		proofs = append(proofs, cashmove.ExternalFlowProof{
			MovementID: bridge.MovementID, CommandDigest: bridge.CommandDigest,
			Effective: bridge.Effective, Amount: amount,
		})
	}
	end, _ := time.Parse(time.DateOnly, e.CloseDate)
	matches, err := cashmove.RecordedExternalFlows(ctx, tx, e.Tenant, e.Portfolio, e.Currency,
		end.AddDate(0, 0, -1), end, e.AsOf, proofs)
	if err != nil {
		return err
	}
	if !matches {
		return ErrEvidence
	}
	return nil
}

func (s *Store) enqueue(ctx context.Context, tx pgx.Tx, receipt Receipt) error {
	e := receipt.Evidence
	tag, err := tx.Exec(ctx, `INSERT INTO return_basis_publications (portfolio_id,currency,close_date,as_of)
		VALUES ($1,$2,$3::text::date,$4) ON CONFLICT DO NOTHING`, e.Portfolio, e.Currency, e.CloseDate, e.AsOf)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	report, err := s.verifiedAssessment(ctx, tx, receipt)
	if err != nil {
		return err
	}
	payload, err := proto.Marshal(report)
	if err != nil {
		return err
	}
	return outbox.Enqueue(ctx, tx, outbox.ServiceAccounting, outbox.Record{
		Subject: Subject, EventType: Subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: "accounting", EventTime: e.AsOf, PartitionKey: e.Portfolio,
		PayloadSchemaRef: schema, CorrelationID: receipt.Digest, TenantID: e.Tenant, Payload: payload,
	})
}

// Handle marks a retained receipt visible after the broker delivers the
// matching Accounting FACT. A redelivery cannot create or change an edition.
func (s *Store) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	if env.GetSource() != outbox.ServiceAccounting || env.GetTenantId() != bus.TenantIDFromContext(ctx) ||
		env.GetEventType() != Subject || env.GetPayloadSchemaRef() != schema || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT ||
		env.GetSchemaVersion() != 1 || env.GetDomain() != "accounting" || len(payload) > Limit {
		return ErrEvidence
	}
	var incoming performancepb.DailyReturnAssessment
	if err := proto.Unmarshal(payload, &incoming); err != nil ||
		incoming.GetAsOf().CheckValid() != nil || incoming.GetEnd().CheckValid() != nil ||
		env.GetPartitionKey() != incoming.GetPortfolioId() || env.GetCorrelationId() != incoming.GetSourceDigest() {
		return ErrEvidence
	}
	day, horizon := incoming.End.AsTime().Format(time.DateOnly), incoming.AsOf.AsTime()
	if env.GetEventTime().CheckValid() != nil || !env.GetEventTime().AsTime().Equal(horizon) {
		return ErrEvidence
	}
	r, err := s.Read(ctx, incoming.PortfolioId, incoming.Currency, day, horizon)
	if err != nil {
		return err
	}
	// Serialize against replay and compare an already observed FACT byte for
	// byte. A later arithmetic deployment must preserve the published edition.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	key := []any{r.Evidence.Portfolio, r.Evidence.Currency, day, horizon}
	var saved []byte
	err = tx.QueryRow(ctx, `SELECT observed_payload FROM return_basis_publications
		WHERE portfolio_id=$1 AND currency=$2 AND close_date=$3::text::date AND as_of=$4 FOR UPDATE`, key...).Scan(&saved)
	if err != nil {
		return err
	}
	if saved != nil {
		if !bytes.Equal(saved, payload) {
			return ErrEvidence
		}
		return tx.Commit(ctx)
	}
	if err = verifyBookedFlows(ctx, tx, r.Evidence); err != nil {
		return err
	}
	report, err := s.verifiedAssessment(ctx, tx, r)
	if err != nil {
		return err
	}
	if !proto.Equal(&incoming, report) {
		return ErrEvidence
	}
	_, err = tx.Exec(ctx, `UPDATE return_basis_publications SET observed_payload=$5
		WHERE portfolio_id=$1 AND currency=$2 AND close_date=$3::text::date AND as_of=$4`, append(key, payload)...)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReadObserved serves only evidence independently observed by the consumer.
func (s *Store) ReadObserved(ctx context.Context, portfolio, ccy, date string, horizon time.Time) (*performancepb.DailyReturnAssessment, error) {
	r, err := s.Read(ctx, portfolio, ccy, date, horizon)
	if err != nil {
		return nil, err
	}
	var observed []byte
	err = s.pool.QueryRow(ctx, `SELECT observed_payload FROM return_basis_publications
		WHERE portfolio_id=$1 AND currency=$2 AND close_date=$3::text::date AND as_of=$4`, portfolio, ccy, date, horizon).Scan(&observed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(observed) == 0) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var report performancepb.DailyReturnAssessment
	if proto.Unmarshal(observed, &report) != nil || report.SourceDigest != r.Digest {
		return nil, ErrEvidence
	}
	return &report, nil
}
