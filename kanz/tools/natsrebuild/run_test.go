package natsrebuild

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eighred/kanz/tools/replay"
)

// fakeChecker answers the existence probe from a fixed set, so the three
// outcomes can be driven without a broker.
type fakeChecker struct {
	exists map[string]bool
	err    error
}

func (f fakeChecker) Exists(_ context.Context, topic string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.exists[topic], nil
}

// closableSource is sliceSource (rebuild_test.go) with the Close the Runner's
// Source interface requires.
type closableSource struct{ *sliceSource }

func (closableSource) Close() error { return nil }

// openFrom serves each topic's canned events; a topic absent from the map opens
// empty, which is the "exists but nothing in the window" case.
func openFrom(byTopic map[string][]replay.Event) OpenFunc {
	return func(_ context.Context, topic string, _ replay.Range) (Source, error) {
		return closableSource{&sliceSource{events: byTopic[topic]}}, nil
	}
}

func outcomesOf(rep Report) map[string]Outcome {
	out := map[string]Outcome{}
	for _, r := range rep.Results {
		out[r.Topic] = r.Outcome
	}
	return out
}

// TestRunnerSystemTenantPathUnchanged pins requirement C: with no tenant
// configured the run reads exactly the un-prefixed topics it always did.
func TestRunnerSystemTenantPathUnchanged(t *testing.T) {
	tenants, err := ParseTenants("", false)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := ResolveTargets(tenants, []string{"order.order", "risk.position"}, []string{"risk.position"})
	if err != nil {
		t.Fatal(err)
	}
	pub := &capturePublisher{}
	r := &Runner{
		Targets:   targets,
		Requested: tenants,
		Checker:   fakeChecker{exists: map[string]bool{"order.order": true, "risk.position": true}},
		Open: openFrom(map[string][]replay.Event{
			"order.order":   {ev("order.order.submitted")},
			"risk.position": {ev("risk.position.changed")},
		}),
		Publisher: pub,
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("system-tenant run failed: %v", err)
	}
	if rep.Published() != 2 {
		t.Fatalf("published = %d, want 2", rep.Published())
	}
	if got := outcomesOf(rep); got["order.order"] != OutcomeReplayed || got["risk.position"] != OutcomeReplayed {
		t.Fatalf("outcomes = %v, want both replayed", got)
	}
	// The subject is the envelope's own EventType — NOT tenant-prefixed. NATS
	// subjects carry no tenant segment; tenancy rides in the envelope.
	if pub.msgs[0].Subject != "order.order.submitted" {
		t.Fatalf("subject = %q, want the envelope's event_type", pub.msgs[0].Subject)
	}
}

// TestRunnerMultiTenantReadsPrefixedTopics is the capability issue #93 asks
// for: more than one tenant prefix in one run, each read from its own topic.
func TestRunnerMultiTenantReadsPrefixedTopics(t *testing.T) {
	tenants := []string{SystemTenant, "acme", "globex"}
	targets, err := ResolveTargets(tenants, []string{"order.order"}, []string{"order.order"})
	if err != nil {
		t.Fatal(err)
	}
	pub := &capturePublisher{}
	r := &Runner{
		Targets:   targets,
		Requested: tenants,
		Checker: fakeChecker{exists: map[string]bool{
			"order.order": true, "acme.order.order": true, "globex.order.order": true}},
		Open: openFrom(map[string][]replay.Event{
			"order.order":        {ev("order.order.submitted")},
			"acme.order.order":   {ev("order.order.submitted"), ev("order.order.filled")},
			"globex.order.order": {ev("order.order.filled")},
		}),
		Publisher: pub,
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("multi-tenant run failed: %v", err)
	}
	if rep.Published() != 4 {
		t.Fatalf("published = %d, want 4", rep.Published())
	}
	want := map[string]uint64{SystemTenant: 1, "acme": 2, "globex": 1}
	for _, s := range rep.Tenants() {
		if s.Published != want[s.Tenant] {
			t.Fatalf("tenant %s published %d, want %d (summary: %s)", s.Tenant, s.Published, want[s.Tenant], rep.Summary())
		}
		if s.Replayed != 1 || s.Attempted != 1 {
			t.Fatalf("tenant %s: attempted=%d replayed=%d, want 1/1", s.Tenant, s.Attempted, s.Replayed)
		}
	}
	if barren := rep.BarrenTenants(); len(barren) != 0 {
		t.Fatalf("BarrenTenants = %v, want none", barren)
	}
}

// TestRunnerMissingTopicIsFatal is the core of issue #93: a tenant whose topics
// are not in Kafka must NOT come back as a successful rebuild.
func TestRunnerMissingTopicIsFatal(t *testing.T) {
	tenants := []string{SystemTenant, "acme"}
	targets, err := ResolveTargets(tenants, []string{"order.order"}, []string{"order.order"})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := (&Runner{
		Targets:   targets,
		Requested: tenants,
		// acme.order.order was never provisioned — the tenant DR case that
		// used to drain nothing and exit 0.
		Checker:   fakeChecker{exists: map[string]bool{"order.order": true}},
		Open:      openFrom(map[string][]replay.Event{"order.order": {ev("order.order.submitted")}}),
		Publisher: &capturePublisher{},
	}).Run(context.Background())

	if err == nil {
		t.Fatal("a tenant whose topic does not exist in Kafka returned success — this is the exit-0 defect issue #93 exists to close")
	}
	for _, want := range []string{"acme.order.order", "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q, got: %v", want, err)
		}
	}
	if got := outcomesOf(rep)["acme.order.order"]; got != OutcomeMissing {
		t.Fatalf("outcome = %q, want %q", got, OutcomeMissing)
	}
	// The partial report still accounts for what was republished before the
	// abort — a re-run needs it.
	if rep.Published() != 1 {
		t.Fatalf("partial report published = %d, want the 1 event drained before the abort", rep.Published())
	}
}

// TestRunnerEmptyTopicIsReportedNotFatal keeps the middle state distinct: the
// topic exists and simply held nothing in the window. Legitimate — but it must
// be visible, not folded into a total.
func TestRunnerEmptyTopicIsReportedNotFatal(t *testing.T) {
	tenants := []string{"acme"}
	targets, err := ResolveTargets(tenants, []string{"order.order", "risk.position"}, []string{"risk.position"})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := (&Runner{
		Targets:   targets,
		Requested: tenants,
		Checker:   fakeChecker{exists: map[string]bool{"acme.order.order": true, "acme.risk.position": true}},
		Open:      openFrom(map[string][]replay.Event{"acme.order.order": {ev("order.order.submitted")}}),
		Publisher: &capturePublisher{},
	}).Run(context.Background())
	if err != nil {
		t.Fatalf("an existing-but-empty topic must not fail the run: %v", err)
	}
	got := outcomesOf(rep)
	if got["acme.order.order"] != OutcomeReplayed {
		t.Fatalf("acme.order.order outcome = %q, want %q", got["acme.order.order"], OutcomeReplayed)
	}
	if got["acme.risk.position"] != OutcomeEmpty {
		t.Fatalf("acme.risk.position outcome = %q, want %q — an empty topic must be distinguishable from a missing one",
			got["acme.risk.position"], OutcomeEmpty)
	}
	summary := rep.Tenants()[0]
	if summary.Replayed != 1 || summary.Empty != 1 {
		t.Fatalf("summary = %s, want replayed=1 empty=1", summary)
	}
}

// TestRunnerBarrenTenantIsNamed — every topic existed, none held anything. Not
// fatal, but the tenant is named rather than disappearing into a zero.
func TestRunnerBarrenTenantIsNamed(t *testing.T) {
	tenants := []string{SystemTenant, "acme"}
	targets, err := ResolveTargets(tenants, []string{"order.order"}, []string{"order.order"})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := (&Runner{
		Targets:   targets,
		Requested: tenants,
		Checker:   fakeChecker{exists: map[string]bool{"order.order": true, "acme.order.order": true}},
		Open:      openFrom(map[string][]replay.Event{"order.order": {ev("order.order.submitted")}}),
		Publisher: &capturePublisher{},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	barren := rep.BarrenTenants()
	if len(barren) != 1 || barren[0] != "acme" {
		t.Fatalf("BarrenTenants = %v, want [acme]", barren)
	}
}

// TestRunnerProbeErrorIsNotMissing — an unreachable broker must not be reported
// as a topic that does not exist. A loud failure naming the wrong cause sends
// the operator to re-provision topics that are already there.
func TestRunnerProbeErrorIsNotMissing(t *testing.T) {
	tenants := []string{"acme"}
	targets, err := ResolveTargets(tenants, []string{"order.order"}, []string{"order.order"})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := (&Runner{
		Targets:   targets,
		Requested: tenants,
		Checker:   fakeChecker{err: errors.New("dial tcp kafka:9092: connect: connection refused")},
		Open:      openFrom(nil),
		Publisher: &capturePublisher{},
	}).Run(context.Background())
	if err == nil {
		t.Fatal("a failed probe must fail the run")
	}
	if got := outcomesOf(rep)["acme.order.order"]; got != OutcomeFailed {
		t.Fatalf("outcome = %q, want %q (never %q — the broker was unreachable, the topic may well exist)",
			got, OutcomeFailed, OutcomeMissing)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error must carry the probe cause, got: %v", err)
	}
}

// TestRunnerRefusesAnEmptyWorkSet — a Runner with nothing to do is a
// misconfiguration, not a successful no-op.
func TestRunnerRefusesAnEmptyWorkSet(t *testing.T) {
	_, err := (&Runner{
		Checker:   fakeChecker{},
		Open:      openFrom(nil),
		Publisher: &capturePublisher{},
	}).Run(context.Background())
	if err == nil {
		t.Fatal("a Runner with no targets reported success")
	}
	if !strings.Contains(err.Error(), "restore nothing") {
		t.Fatalf("error = %v, want it to say the run would restore nothing", err)
	}
}

// TestRunnerRefusesARequestedTenantWithNoTargets — the same refusal one level
// up: the tenant list and the target list disagree.
func TestRunnerRefusesARequestedTenantWithNoTargets(t *testing.T) {
	rep, err := (&Runner{
		Targets:   []Target{{Tenant: "acme", Base: "order.order", Topic: "acme.order.order"}},
		Requested: []string{"acme", "globex"},
		Checker:   fakeChecker{exists: map[string]bool{"acme.order.order": true}},
		Open:      openFrom(nil),
		Publisher: &capturePublisher{},
	}).Run(context.Background())
	if err == nil {
		t.Fatal("a requested tenant with no targets reported success")
	}
	if !strings.Contains(err.Error(), "globex") {
		t.Fatalf("error must name the uncovered tenant, got: %v", err)
	}
	if len(rep.Results) != 0 {
		t.Fatalf("nothing may be published once the plan is known bad, got %d results", len(rep.Results))
	}
}
