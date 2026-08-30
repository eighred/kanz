package controller_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/services/autopilot/internal/controller"
	"github.com/eighred/kanz/services/autopilot/internal/signal"
)

// escalationMsg is the message LogEscalator emits at error level. It is the
// escalation's real sink: OBS-01a ships stdout JSON, so this line is what
// platform alerting routes to an on-call human.
const escalationMsg = "autopilot human escalation"

// escRecorder captures the escalator's own log output.
//
// It is handed to the LogEscalator ALONE and never to the Controller, so a line
// counted here proves LogEscalator.Escalate emitted it — not the controller's
// separate "autopilot escalating to human" warning, which fires on the same
// path and would otherwise double every count.
//
// This is the assertion surface that replaced LogEscalator.Escalations() when
// #844 removed the in-process buffer: the log line is the sink production
// actually has, so it is the honest thing for a test to assert on.
type escRecorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *escRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *escRecorder) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// escalations returns the parsed escalation records the escalator emitted.
func (r *escRecorder) escalations(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(r.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("escalator emitted a non-JSON line %q: %v", line, err)
		}
		if rec["msg"] == escalationMsg {
			out = append(out, rec)
		}
	}
	return out
}

// LogEscalator MUST RETAIN NOTHING THAT GROWS WITH TRAFFIC (#844).
//
// # What this is protecting
//
// LogEscalator is the production-wired Escalator (services/autopilot/cmd/
// autopilot/main.go). It used to append every escalation to a `seen
// []Escalation` under a mutex — no cap, no TTL, no eviction — and the only
// reader was a test helper documented "(test/inspection)". Each entry pinned a
// whole signal.Signal plus the reason string for the life of the pod.
//
// The growth curve was the wrong way round: autopilot escalates when it hands
// off to a human, so the accumulation rate peaked exactly during a degraded
// period — a flapping venue, a stalled consumer, an incident — which is when an
// OOM kill of the remediation controller costs the most and reports the least.
//
// # Why the assertion is structural rather than a count
//
// The field is gone, so there is no count left to drive past. What a future
// change can still do is put the retention back — a ring "for inspection", a
// per-subject dedupe map, a last-N buffer — and none of those fail anything
// until a pod dies of one. So this is default-deny over LogEscalator's own
// state: no field of it, transitively through kanz-owned types, may be a slice,
// map or channel.
//
// If in-process retention is ever genuinely wanted, the shape to copy is
// internal/risk/state's dedupWindow (explicit cap, TTL, gc()), and this test is
// the place to state the bound — not to delete.
func TestLogEscalatorRetainsNoUnboundedBuffer(t *testing.T) {
	ty := reflect.TypeOf((*controller.LogEscalator)(nil)).Elem()

	var containers []string
	fields := walkKanzContainers(ty, ty.Name(), 0, map[reflect.Type]bool{}, &containers)

	// NON-VACUITY: the walk actually saw LogEscalator's fields. If the type is
	// renamed, emptied or replaced by an alias, every assertion below passes
	// over nothing.
	if fields == 0 {
		t.Fatalf("the field walk visited 0 fields of %s — the type has moved and this "+
			"guard's verdict is empty", ty)
	}
	if _, ok := ty.FieldByName("logger"); !ok {
		t.Fatalf("%s has no `logger` field — the escalator's sink has changed shape, so "+
			"this guard is no longer describing the type it was written about", ty)
	}

	if len(containers) > 0 {
		t.Errorf("%s retains growable state: %v.\n\n"+
			"A slice/map/channel on the production-wired escalator grows once per "+
			"escalation for the life of the pod, and escalations peak during exactly the "+
			"incident where an OOM kill of the remediation controller is most damaging "+
			"(#844). The error-level log line is the sink the platform routes to a human; "+
			"a buffer no production code reads is retention, not a sink.\n\n"+
			"If in-process retention is genuinely wanted, give it an explicit cap and TTL "+
			"the way internal/risk/state's dedupWindow does, and assert the bound here.",
			ty, containers)
	}
}

// walkKanzContainers appends to *out every slice/map/channel reachable from ty's
// fields, and returns how many fields it visited at the top level.
//
// It descends only into types declared in this module: a walk into stdlib
// internals (slog.Logger's handler chain, sync.Mutex's runtime state) reports
// containers that are not this service's state, and the false positive would be
// read as the guard being noisy rather than the code being wrong.
func walkKanzContainers(ty reflect.Type, path string, depth int, seen map[reflect.Type]bool, out *[]string) int {
	if depth > 4 || seen[ty] {
		return 0
	}
	seen[ty] = true

	if ty.Kind() != reflect.Struct {
		return 0
	}
	for i := range ty.NumField() {
		f := ty.Field(i)
		at := path + "." + f.Name
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Slice, reflect.Map, reflect.Chan:
			*out = append(*out, at+" "+f.Type.String())
		case reflect.Struct:
			if strings.HasPrefix(ft.PkgPath(), "github.com/eighred/kanz") {
				walkKanzContainers(ft, at, depth+1, seen, out)
			}
		}
	}
	return ty.NumField()
}

// The escalation's sink is the error-level log line, and it fires once per
// escalation however many there are (#844). This is the coverage that replaced
// the removed Escalations() buffer: it asserts the same "one escalation, one
// record" property against the artifact production actually keeps.
func TestLogEscalatorEmitsOneRecordPerEscalation(t *testing.T) {
	// Well past anything a "last N for inspection" ring would have held, so a
	// reintroduced bounded buffer could not satisfy this assertion by accident.
	const escalations = 1000

	rec := &escRecorder{}
	esc := controller.NewLogEscalator(rec.logger())

	for i := range escalations {
		s := signal.Signal{
			Kind:     signal.KindDataGap,
			Subject:  "AAPL",
			Severity: signal.SeverityWarning,
			EventID:  "evt-" + strconv.Itoa(i),
			Time:     time.Unix(0, 0).UTC(),
		}
		if err := esc.Escalate(context.Background(), s, "sub-threshold gap"); err != nil {
			t.Fatalf("escalate %d: %v", i, err)
		}
	}

	got := rec.escalations(t)
	if len(got) != escalations {
		t.Fatalf("escalation log lines = %d, want %d — the sink drops or coalesces "+
			"escalations, so a hand-off to a human can be lost", len(got), escalations)
	}

	// Every attribute an on-call human needs to act must be on the line, because
	// the line is now the only record. Asserting only the count would pass on a
	// log call that had lost the subject or the reason.
	last := got[len(got)-1]
	for _, want := range []struct{ key, value string }{
		{"level", "ERROR"},
		{"kind", string(signal.KindDataGap)},
		{"subject", "AAPL"},
		{"severity", signal.SeverityWarning.String()},
		{"reason", "sub-threshold gap"},
		{"event_id", "evt-" + strconv.Itoa(escalations-1)},
	} {
		if got, ok := last[want.key].(string); !ok || got != want.value {
			t.Errorf("escalation line %s = %v, want %q", want.key, last[want.key], want.value)
		}
	}
}
