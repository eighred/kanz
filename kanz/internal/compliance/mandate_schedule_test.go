package compliance_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/pkg/bus"
)

// #916: SCHEDULING A MANDATE CHANGE USED TO DISARM THE PORTFOLIO.
//
// The MANDATE stream keeps ONE message per (tenant, portfolio) subject and never
// ages it out. `effective_at` is allowed to be in the future — the two-signature
// flow carries it inside the digest precisely because it is a decision two people
// signed — so an operator may publish v2 today, dated next month. That publish
// EVICTED v1: the version actually governing the portfolio, off a compacted
// stream, permanently. Every replica that booted afterwards read only the
// future-dated v2, found nothing in effect, and answered UNGOVERNED — which under
// OMS_REQUIRE_MANDATE=false is admitted with NO CONSTRAINTS rather than refused.
//
// Nothing reported a loss. The message applied cleanly, the registry was Armed
// and Complete, and the only signal was a counter whose meaning is "this
// portfolio has no mandate" — indistinguishable from a portfolio nobody has
// written one for yet.
//
// The tests below are therefore about the DURABLE side. #884 settled the
// in-memory answer (retainSelectable: the one version in force plus every version
// dated later); these are about the stream being able to carry that set at all.

// mandateBrokerRig is the real-broker fixture the #916 tests share: a producer, a
// consumer, and a publisher that reads the subject it writes.
type mandateBrokerRig struct {
	pub      *comp.Publisher
	producer *bus.Producer
	consumer *bus.Consumer
	client   *bus.NATSClient
	suffix   string
}

// newMandateBrokerRig binds the REAL MANDATE stream and REFUSES to run against
// anything else.
//
// THE COMPACTION IS THE SUBJECT OF THE TEST, so a stream without it must not
// produce a pass. bustest.EnsureSubjects falls back to a scratch stream on a bare
// broker, and that fallback has no MaxMsgsPerSubject at all — every version would
// simply be retained, every assertion below would hold, and the run would be
// green while proving nothing about the store this defect lives in. Asserted, not
// assumed.
func newMandateBrokerRig(t *testing.T, ctx context.Context) *mandateBrokerRig {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the mandate publish path over a real compacted stream")
	}

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "MANDATE")
	if err != nil {
		t.Skipf("no MANDATE stream on this broker (%v) — bootstrap the CI topology first; a "+
			"scratch stream would retain every version and pass this test for the wrong reason", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxMsgsPerSubject != 1 {
		t.Fatalf("MANDATE.MaxMsgsPerSubject = %d, want 1. These tests exist because the stream "+
			"keeps exactly one message per portfolio; on a stream that keeps more they assert "+
			"nothing and pass regardless", info.Config.MaxMsgsPerSubject)
	}
	if info.Config.MaxAge != 0 {
		t.Fatalf("MANDATE.MaxAge = %s, want 0 — a mandate that ages off the stream is a portfolio "+
			"that has quietly become ungoverned", info.Config.MaxAge)
	}

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "mandate-schedule-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "kanz-mandate", ProducerVersion: "it", Tenant: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}
	return &mandateBrokerRig{
		pub:      comp.NewPublisher(producer, client),
		producer: producer,
		consumer: consumer,
		client:   client,
		// UNIQUE per run: the MANDATE stream is compacted and PERSISTENT, so a
		// previous run's mandates would arm this one and hide the failure.
		suffix: fmt.Sprintf("%d", time.Now().UnixNano()),
	}
}

// publish puts one mandate version under a real two-person approval.
func (r *mandateBrokerRig) publish(t *testing.T, ctx context.Context, m *compliancepb.Mandate) error {
	t.Helper()
	const reason = "#916 schedule test"
	return r.pub.Publish(ctx, m, mustApprove(t, m, reason), reason)
}

// bootAGate does what a restarting replica does: a fresh, empty registry armed
// from DeliverLastPerSubject over the mandate subject, i.e. from whatever the
// compacted stream retained and nothing else. It returns the registry once the
// portfolio's subject has been folded, or fails.
//
// IT IS A RESTART, NOT A CACHE. Nothing survives from the publisher's process,
// which is the whole point: a long-lived replica that saw both versions keeps
// resolving correctly and hides the loss entirely.
func (r *mandateBrokerRig) bootAGate(t *testing.T, ctx context.Context, tenant, portfolio string) *comp.MandateRegistry {
	t.Helper()
	registry := comp.NewMandateRegistry()
	consumerImpl := comp.NewMandateConsumer(registry, nil)

	subCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)

	var mu sync.Mutex
	seen := false
	go func() {
		_ = r.consumer.SubscribeBroadcast(subCtx, comp.SubjectMandateAll,
			func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
				err := consumerImpl.Handle(ctx, env, payload)
				var cc lifecyclepb.ConfigChanged
				if pErr := proto.Unmarshal(payload, &cc); pErr == nil {
					if tID, pID, ok := comp.ParseMandateConfigKey(cc.GetConfigKey()); ok &&
						tID == tenant && pID == portfolio {
						mu.Lock()
						seen = true
						mu.Unlock()
					}
				}
				return err
			})
	}()

	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		mu.Lock()
		done := seen
		mu.Unlock()
		if done {
			return registry
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("a booting gate never saw the retained message for %s/%s within 20s", tenant, portfolio)
	return nil
}

// TestAScheduledMandateDoesNotEvictTheOneInForce is the #916 regression, and it
// is the half that loses money.
//
// Publish v1 dated in the past, then v2 dated in the future — the routine,
// correct operator action of scheduling a mandate change. A replica that boots
// between those two dates must resolve the portfolio to v1, the mandate ACTUALLY
// IN FORCE. Before this it resolved to nothing at all, and an ungoverned
// portfolio under the default posture trades unconstrained.
func TestAScheduledMandateDoesNotEvictTheOneInForce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rig := newMandateBrokerRig(t, ctx)

	const tenant = "acme"
	portfolio := "pf-schedule-" + rig.suffix
	inForce := mandateVersion(tenant, portfolio, 1, time.Now().UTC().Add(-24*time.Hour))
	scheduled := mandateVersion(tenant, portfolio, 2, time.Now().UTC().Add(720*time.Hour))

	if err := rig.publish(t, ctx, inForce); err != nil {
		t.Fatalf("publish the mandate in force: %v", err)
	}
	if err := rig.publish(t, ctx, scheduled); err != nil {
		t.Fatalf("publish the SCHEDULED mandate: %v", err)
	}

	reg := rig.bootAGate(t, ctx, tenant, portfolio)
	got, ok, err := reg.Mandate(ctx, tenant, portfolio, time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ok.NoMandate() {
		t.Fatal("A RESTARTED GATE READ THIS PORTFOLIO AS UNGOVERNED while a mandate is in force.\n" +
			"Scheduling a mandate change evicted the version governing the portfolio from the " +
			"compacted stream. Under OMS_REQUIRE_MANDATE=false an ungoverned portfolio is not " +
			"refused — it is ADMITTED, unconstrained, and the only signal is a counter that reads " +
			"exactly like a portfolio nobody has written a mandate for yet (#916).")
	}
	if got.GetVersion() != 1 {
		t.Fatalf("resolved v%d, want v1 — the version in force NOW, not the one scheduled for later",
			got.GetVersion())
	}
}

// TestAScheduledMandateSurvivesARestart is the other half, and a fix that traded
// one for the other would pass the test above and fail this one.
//
// The scheduled version must still be on the stream a restart reads, and must
// take force on its own date. Carrying only the mandate in force would make
// scheduling silently do nothing — a dual-signed decision that evaporates at the
// next rolling update.
func TestAScheduledMandateSurvivesARestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rig := newMandateBrokerRig(t, ctx)

	const tenant = "acme"
	portfolio := "pf-survives-" + rig.suffix
	takesForce := time.Now().UTC().Add(720 * time.Hour)
	inForce := mandateVersion(tenant, portfolio, 1, time.Now().UTC().Add(-24*time.Hour))
	scheduled := mandateVersion(tenant, portfolio, 2, takesForce)

	if err := rig.publish(t, ctx, inForce); err != nil {
		t.Fatalf("publish the mandate in force: %v", err)
	}
	if err := rig.publish(t, ctx, scheduled); err != nil {
		t.Fatalf("publish the SCHEDULED mandate: %v", err)
	}

	reg := rig.bootAGate(t, ctx, tenant, portfolio)
	got, ok, err := reg.Mandate(ctx, tenant, portfolio, takesForce.Add(time.Hour))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ok.NoMandate() {
		t.Fatal("after a restart NOTHING governs this portfolio at the scheduled date — the " +
			"scheduled mandate is not on the stream a booting replica reads (#916)")
	}
	if got.GetVersion() != 2 {
		t.Fatalf("at the scheduled date the gate resolved v%d, want v2. The change two people "+
			"signed was lost in the store: it will never take force, and nothing says so",
			got.GetVersion())
	}
}

// TestAMandateWriteIsRefusedWhenTheSubjectMovedUnderIt proves the read-modify-write
// is CONDITIONAL against the real broker.
//
// Publishing the set means merging into what the subject holds. Two approvers
// merging concurrently would each write a set built from the value they read, and
// the loser's mandate would vanish — the same disappearance #916 is about, from a
// different cause. The broker refuses the second write instead.
//
// It also pins the SEQUENCE-ZERO case, which is the first publish for a portfolio
// and the one a naive uint64 could not express: expecting 0 on a subject that
// already holds a message must be REFUSED, not silently treated as "no
// expectation".
func TestAMandateWriteIsRefusedWhenTheSubjectMovedUnderIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rig := newMandateBrokerRig(t, ctx)

	const tenant = "acme"
	portfolio := "pf-cas-" + rig.suffix
	subject := comp.SubjectMandateFor(tenant, portfolio)
	m := mandateVersion(tenant, portfolio, 1, time.Now().UTC().Add(-time.Hour))

	// The subject is empty, so expecting sequence 0 must SUCCEED.
	var expectEmpty uint64
	if err := rig.producer.Publish(ctx, mandateEvent(tenant, portfolio, subject, m, &expectEmpty)); err != nil {
		t.Fatalf("a first publish conditioned on an empty subject was refused: %v.\n"+
			"Nats-Expected-Last-Subject-Sequence: 0 is how the FIRST writer for a portfolio claims "+
			"the subject; without it two first publishes race and one is lost", err)
	}

	// Now the subject holds something, so the same expectation must be REFUSED.
	err := rig.producer.Publish(ctx, mandateEvent(tenant, portfolio, subject, m, &expectEmpty))
	if err == nil {
		t.Fatal("a mandate publish conditioned on a STALE sequence was ACCEPTED. Every mandate " +
			"publish is a merge into the value it read; an unconditional write drops whatever " +
			"another approver put there in between, off a stream that never ages out (#916)")
	}

	// And the read reports the sequence the write has to be conditioned on.
	_, _, seq, err := rig.client.LastOnSubject(ctx, subject)
	if err != nil {
		t.Fatalf("LastOnSubject after a successful publish: %v", err)
	}
	if seq == 0 {
		t.Fatal("LastOnSubject reported sequence 0 for a subject that holds a message — a publisher " +
			"conditioning on that would assert the subject is empty and be refused forever")
	}
}

// TestPublishCarriesTheInForceMandateAlongsideTheScheduledOne is the same
// property one layer down, without a broker: what the publisher puts in
// ConfigChanged.new_value.
//
// It is here as well as on the broker because the two can fail independently — a
// publisher that wrote the set correctly onto a stream that could not carry it,
// or a stream that could carry a set a publisher never built.
func TestPublishCarriesTheInForceMandateAlongsideTheScheduledOne(t *testing.T) {
	b := &countingBus{}
	pub := comp.NewPublisher(b, b)
	ctx := context.Background()
	const reason = "#916"

	inForce := mandateVersion("acme", "PF1", 1, time.Now().UTC().Add(-24*time.Hour))
	if err := pub.Publish(ctx, inForce, mustApprove(t, inForce, reason), reason); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	scheduled := mandateVersion("acme", "PF1", 2, time.Now().UTC().Add(720*time.Hour))
	if err := pub.Publish(ctx, scheduled, mustApprove(t, scheduled, reason), reason); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	if len(b.events) != 2 {
		t.Fatalf("published %d events, want 2", len(b.events))
	}
	cc, ok := b.events[1].Payload.(*lifecyclepb.ConfigChanged)
	if !ok {
		t.Fatalf("payload is %T", b.events[1].Payload)
	}
	got, err := comp.DecodeMandateValue(cc.GetNewValue())
	if err != nil {
		t.Fatalf("decode the published value: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the scheduling publish carried %d mandate(s), want 2 — the version in force AND "+
			"the scheduled one. The stream keeps only this message, so anything missing here is "+
			"gone from the estate (#916). Got: %v", len(got), versionsOf(got))
	}
	if got[0].GetVersion() != 1 || got[1].GetVersion() != 2 {
		t.Fatalf("published set is %v, want [1 2] ordered by effective_at", versionsOf(got))
	}
	if cc.GetPreviousValue() == "" {
		t.Error("previous_value is empty on a publish that replaced a value the publisher READ — " +
			"the audit trail can no longer say what this superseded")
	}
}

// TestPublishRefusesWhenItCannotReadTheSubject pins the fail-closed direction.
//
// A read that FAILS and a subject that is EMPTY must not be collapsed: treating a
// broker error as "nothing is there" publishes a set containing only the new
// mandate, which is the original defect with an extra step.
func TestPublishRefusesWhenItCannotReadTheSubject(t *testing.T) {
	b := &countingBus{readErr: errors.New("broker said no")}
	pub := comp.NewPublisher(b, b)
	m := mandateVersion("acme", "PF1", 2, time.Now().UTC().Add(720*time.Hour))
	const reason = "#916"

	err := pub.Publish(context.Background(), m, mustApprove(t, m, reason), reason)
	if err == nil {
		t.Fatal("a mandate was published although the publisher could not read what the " +
			"portfolio's subject already holds — the write would have deleted it")
	}
	if len(b.events) != 0 {
		t.Errorf("%d event(s) published on the refusal path", len(b.events))
	}
}

// TestPublishRefusesWhenNoSubjectReaderIsWired is the composition-root case, and
// it is the one a unit test usually misses.
//
// A deployment that constructs the publisher without a reader has a mandate
// surface that looks armed and would overwrite the portfolio's subject blind.
// "Nothing configured" and "checked, and fine" must not look the same, so it
// refuses on the FIRST mandate change rather than surfacing at somebody's next
// restart as a portfolio that lost its mandate.
func TestPublishRefusesWhenNoSubjectReaderIsWired(t *testing.T) {
	b := &countingBus{}
	pub := comp.NewPublisher(b, nil)
	m := mandateVersion("acme", "PF1", 2, time.Now().UTC().Add(720*time.Hour))
	const reason = "#916"

	if err := pub.Publish(context.Background(), m, mustApprove(t, m, reason), reason); err == nil {
		t.Fatal("a publisher wired with NO reader of the mandate subject published anyway — it " +
			"cannot know what the portfolio already holds, so the write deletes it")
	}
	if len(b.events) != 0 {
		t.Errorf("%d event(s) published on the refusal path", len(b.events))
	}
}

// TestPublishRefusesAMandateNoLookupCouldSelect keeps the fix from being silently
// lossy in the other direction.
//
// A version dated BEHIND the one already in force is unreachable: retainSelectable
// drops it, the published set would not contain it, and the operator would be told
// their dual-signed change was PUBLISHED while nothing about the portfolio
// changed. A publish that cannot take effect must say so.
func TestPublishRefusesAMandateNoLookupCouldSelect(t *testing.T) {
	b := &countingBus{}
	pub := comp.NewPublisher(b, b)
	ctx := context.Background()
	const reason = "#916"

	inForce := mandateVersion("acme", "PF1", 1, time.Now().UTC().Add(-24*time.Hour))
	if err := pub.Publish(ctx, inForce, mustApprove(t, inForce, reason), reason); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	backdated := mandateVersion("acme", "PF1", 2, time.Now().UTC().Add(-96*time.Hour))
	if err := pub.Publish(ctx, backdated, mustApprove(t, backdated, reason), reason); err == nil {
		t.Fatal("a mandate dated behind the one in force was published. No lookup can select it, " +
			"so the operator is told a two-person decision took effect when it did not")
	}
	if len(b.events) != 1 {
		t.Errorf("%d event(s) published, want 1 — the refusal must not reach the broker", len(b.events))
	}
}

// TestARepublishCollapsesTheSetRatherThanGrowingIt pins the bound on what the
// stream carries.
//
// The set is what makes compaction correct, and an unbounded set would move #884's
// leak from a process's memory into a stream with infinite retention — where it is
// worse, because every replica that ever boots pays for it. Correcting the same
// version repeatedly must leave one member, not one per attempt.
func TestARepublishCollapsesTheSetRatherThanGrowingIt(t *testing.T) {
	b := &countingBus{}
	pub := comp.NewPublisher(b, b)
	ctx := context.Background()
	const reason = "#916"

	for i := range 5 {
		m := mandateVersion("acme", "PF1", 7, time.Now().UTC().Add(-time.Duration(5-i)*time.Hour))
		if err := pub.Publish(ctx, m, mustApprove(t, m, reason), reason); err != nil {
			t.Fatalf("republish %d: %v", i, err)
		}
	}
	cc := b.events[len(b.events)-1].Payload.(*lifecyclepb.ConfigChanged)
	got, err := comp.DecodeMandateValue(cc.GetNewValue())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("five corrections of v7 left %d members on the subject, want 1 — the durable set "+
			"is accumulating versions no lookup can choose (#884, now on a stream that never ages "+
			"out). Got: %v", len(got), versionsOf(got))
	}
}

// TestALegacySingleMandateValueStillArmsAGate is the upgrade path, and it is not
// optional.
//
// The MANDATE stream has NO max-age. Every value published before #916 is a bare
// Mandate and is still the retained message on its subject, and will be until
// someone republishes that portfolio. A decoder that understood only the new set
// shape would reject them — and a rejected mandate makes the gate REFUSE that
// portfolio's orders (#619), so an upgrade would have taken every portfolio
// nobody re-published out of trading.
func TestALegacySingleMandateValueStillArmsAGate(t *testing.T) {
	reg := comp.NewMandateRegistry()
	legacy := mandateVersion("acme", "PF1", 1, time.Now().UTC().Add(-time.Hour))
	value, err := comp.MarshalMandateValue(legacy)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := comp.NewMandateLoader(reg).Apply(&lifecyclepb.ConfigChanged{
		ConfigKey: comp.MandateConfigKey("acme", "PF1"),
		NewValue:  value,
	})
	if err != nil {
		t.Fatalf("a pre-#916 mandate value was REFUSED: %v. Every portfolio not yet republished "+
			"would be un-governable, and its orders refused, from the moment this shipped", err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied %d mandates from a legacy value, want 1", len(applied))
	}
	if _, ok, _ := reg.Mandate(context.Background(), "acme", "PF1", time.Now().UTC()); ok.NoMandate() {
		t.Fatal("a gate armed from a legacy value reads the portfolio as UNGOVERNED")
	}
}

// TestMandateLoaderAppliesEveryMemberOfTheSet — applying only the first member
// restores exactly the loss the set exists to prevent, on the consumer side.
func TestMandateLoaderAppliesEveryMemberOfTheSet(t *testing.T) {
	reg := comp.NewMandateRegistry()
	takesForce := time.Now().UTC().Add(720 * time.Hour)
	value, err := comp.MarshalMandateSetValue([]*compliancepb.Mandate{
		mandateVersion("acme", "PF1", 1, time.Now().UTC().Add(-24*time.Hour)),
		mandateVersion("acme", "PF1", 2, takesForce),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := comp.NewMandateLoader(reg).Apply(&lifecyclepb.ConfigChanged{
		ConfigKey: comp.MandateConfigKey("acme", "PF1"),
		NewValue:  value,
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now, ok, _ := reg.Mandate(ctx, "acme", "PF1", time.Now().UTC())
	if ok.NoMandate() || now.GetVersion() != 1 {
		t.Fatalf("resolved %v at now, want v1 in force", now.GetVersion())
	}
	later, ok, _ := reg.Mandate(ctx, "acme", "PF1", takesForce.Add(time.Hour))
	if ok.NoMandate() || later.GetVersion() != 2 {
		t.Fatalf("resolved %v at the scheduled date, want v2 — the scheduled member of the set "+
			"was dropped on the way in", later.GetVersion())
	}
}

// TestAnEmptyMandateSetIsNeverPublished — the stream keeps one message per
// portfolio forever, so a value carrying no mandate is not a no-op: it is the
// last word on that portfolio and every replica booting afterwards reads it as
// UNGOVERNED.
func TestAnEmptyMandateSetIsNeverPublished(t *testing.T) {
	if _, err := comp.MarshalMandateSetValue(nil); err == nil {
		t.Fatal("an EMPTY mandate set was serialized for publication")
	}
}

func mandateVersion(tenant, portfolio string, version uint64, effective time.Time) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId:   fmt.Sprintf("m-%s-v%d", portfolio, version),
		TenantId:    tenant,
		PortfolioId: portfolio,
		Version:     version,
		EffectiveAt: timestamppb.New(effective),
	}
}

func versionsOf(ms []*compliancepb.Mandate) []uint64 {
	out := make([]uint64, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.GetVersion())
	}
	return out
}

// mandateEvent builds the FACT the publisher builds, with an explicit sequence
// expectation — used to exercise the conditional write directly.
func mandateEvent(tenant, portfolio, subject string, m *compliancepb.Mandate, expect *uint64) bus.Event {
	value, err := comp.MarshalMandateSetValue([]*compliancepb.Mandate{m})
	if err != nil {
		panic(err)
	}
	key := comp.MandateConfigKey(tenant, portfolio)
	return bus.Event{
		Subject:                subject,
		EventType:              comp.EventTypeMandateChanged,
		EventClass:             envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:          1,
		Domain:                 comp.Domain,
		EventTime:              m.GetEffectiveAt().AsTime(),
		PartitionKey:           key,
		TenantID:               tenant,
		PayloadSchemaRef:       "lifecycle.v1.ConfigChanged:1",
		Payload:                &lifecyclepb.ConfigChanged{ConfigKey: key, NewValue: value},
		ExpectedLastSubjectSeq: expect,
	}
}
