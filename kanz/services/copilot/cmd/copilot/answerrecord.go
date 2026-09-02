package main

import (
	"context"
	"errors"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/bus"
)

// SubjectAgentAnswer is where the copilot records what a model was shown (#971).
//
// ON THE PLATFORM DOMAIN, BESIDE platform.authz.decision. The answer record and
// the authorization decisions are two halves of ONE request — "this principal was
// permitted to read this resource" and "and here is what was then done with it" —
// and a reader reconstructing an answer needs both. Same stream means the same
// retention, so they age out together; splitting them across domains would let
// one half outlive the other and leave a trail that is worse than either.
//
// {domain}.{entity}.{event_type}, so it archives to the platform.agent Kafka
// topic on the same money-path retention the authz decisions already take.
const SubjectAgentAnswer = "platform.agent.answered"

const schemaRefAgentAnswer = "observation.v1.AgentAnswerRecord:1"

// busAnswerRecorder publishes an answer record onto the observation stream.
//
// IT RIDES THE DECISION RECORDER'S PRODUCER, not a second connection. The
// authorization decisions and the answer record describe two halves of one
// request, and a process that could publish the first and not the second would
// produce a trail saying a principal was permitted to read a resource with
// nothing saying what was then done with it.
type busAnswerRecorder struct {
	producer *bus.Producer
}

// RecordAnswer implements agent.AnswerRecorder.
//
// THE TENANT COMES OFF THE RECORD, NOT THE DEPLOYMENT. The copilot serves many
// tenants from one process — its producer is built with NO ProducerConfig.Tenant
// for exactly that reason, so pkg/authbus can stamp each decision with the
// deciding principal's own — and an answer filed under this deployment's tenant
// would put one analyst's retrieval manifest in another's audit trail.
//
// AN UNTENANTED RECORD IS REFUSED rather than published under a fallback. The
// principal is authenticated before Ask runs (agent.ErrUnauthenticated), so an
// empty tenant here means the record was built wrong, and guessing a tenant for
// an audit record is how a trail stops being evidence.
func (r *busAnswerRecorder) RecordAnswer(ctx context.Context, rec *observationpb.AgentAnswerRecord) error {
	if rec == nil {
		return errors.New("copilot: nil answer record")
	}
	if rec.GetTenantId() == "" {
		return errors.New("copilot: answer record has no tenant — it would be filed under this " +
			"deployment's tenant rather than the asking principal's, which puts one analyst's " +
			"retrieval manifest in another's audit trail")
	}
	// THE PARTITION KEY IS THE PRINCIPAL, which is the granularity a reader
	// reconstructs at ("what did alice ask, and what was she shown") and the unit
	// whose order matters. Keying on the answer id would order nothing and make
	// every answer its own key.
	return r.producer.Publish(ctx, bus.Event{
		Subject:          SubjectAgentAnswer,
		EventType:        SubjectAgentAnswer,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_OBSERVATION,
		SchemaVersion:    1,
		Domain:           "platform",
		EventTime:        rec.GetAnsweredAt().AsTime(),
		PartitionKey:     rec.GetPrincipalSubject(),
		PayloadSchemaRef: schemaRefAgentAnswer,
		TenantID:         rec.GetTenantId(),
		Payload:          rec,
	})
}

// answerRecordsLost counts answers produced whose record could not be published.
//
// A PLAIN COUNTER, registered before anything can drop, so "none lost" is a
// readable zero rather than a missing series (#622). Non-zero means the estate
// answered questions it cannot now explain.
var answerRecordsLost = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "kanz_copilot_answer_records_lost_total",
	Help: "Copilot answers that were produced and whose record could not be published. Non-zero " +
		"means an answer exists that cannot be reconstructed: what the model was shown, which " +
		"model answered and whether the answer was grounded are unrecoverable for it (#971).",
})
