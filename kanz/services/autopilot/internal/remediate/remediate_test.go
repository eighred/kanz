package remediate

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The two messages the default remediators emit. They are the only production
// record that names WHAT was acted on: autopilot serves /healthz, /readyz and
// /metrics and nothing else, and the one metric there —
// kanz_autopilot_outcomes_total (controller/metrics.go) — is labelled by
// condition and outcome rather than by subject or model. So these lines are
// what these tests assert on, the way #844's escalation tests assert on the
// escalator's error-level line.
const (
	quarantineMsg = "autopilot quarantined data subject"
	rollbackMsg   = "autopilot rolled back model"
)

// recorder captures a remediator's own log output.
type recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *recorder) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines returns the parsed records whose msg is want.
func (r *recorder) lines(t *testing.T, want string) []map[string]any {
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
			t.Fatalf("remediator emitted a non-JSON line %q: %v", line, err)
		}
		if rec["msg"] == want {
			out = append(out, rec)
		}
	}
	return out
}

// subjects returns the "subject" attribute of every quarantine line, in order.
func (r *recorder) subjects(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, rec := range r.lines(t, quarantineMsg) {
		s, _ := rec["subject"].(string)
		out = append(out, s)
	}
	return out
}

// THE QUARANTINE WARNING IS SAID ONCE PER SUBJECT (#892 kept this, it did not
// introduce it).
//
// This is the behaviour that stopped LogQuarantiner from being repaired the way
// LogEscalator (#844) and LogModelRoller were. Its map had a live reader —
// Quarantine itself — so deleting it outright would have turned a data-quality
// storm's memory growth into a log flood on the one line an operator has. The
// map became a bounded window instead, and this test pins the property the
// window exists to preserve.
func TestLogQuarantinerWarnsOncePerSubject(t *testing.T) {
	rec := &recorder{}
	q := NewLogQuarantiner(rec.logger())

	for range 5 {
		if err := q.Quarantine(context.Background(), "AAPL", "data_gap: sequence gap"); err != nil {
			t.Fatalf("quarantine: %v", err)
		}
	}
	if err := q.Quarantine(context.Background(), "risk.exposure", "reconcile_divergence: NATS↔Kafka"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	if got, want := rec.subjects(t), []string{"AAPL", "risk.exposure"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("quarantine warn lines = %v, want %v — the subjects are warned about once each, "+
			"in the order they first arrived", got, want)
	}

	// Asserting only the count would pass on a line that had lost the subject or
	// the reason, and this line is the only record that names the subject —
	// kanz_autopilot_outcomes_total is labelled by condition and outcome.
	first := rec.lines(t, quarantineMsg)[0]
	for _, want := range []struct{ key, value string }{
		{"level", "WARN"},
		{"subject", "AAPL"},
		{"reason", "data_gap: sequence gap"},
	} {
		if got, ok := first[want.key].(string); !ok || got != want.value {
			t.Errorf("quarantine line %s = %v, want %q", want.key, first[want.key], want.value)
		}
	}
}

// THE SAY-IT-ONCE SET IS BOUNDED, AND THE BOUND IS THE ONE PRODUCTION RUNS ON
// (#892).
//
// The subject is `dqe.GetSubject()` off a wire DataQualityEvent
// (signal/classify.go), so its universe is whatever a producer sends. Before
// this repair the set grew one permanent entry per distinct subject, fastest
// during exactly the degraded period the autopilot exists to handle.
//
// This drives an order of magnitude past the ceiling and asserts BOTH halves:
// the retained set stops growing, and the eviction actually RAN — an evicted
// subject warns again. The second half is the one that matters, because the
// estate-wide guard in test/arch proves an evictor EXISTS and says outright that
// whether it runs is a behavioural property belonging in the owning package's
// tests. This is that test.
func TestLogQuarantinerSayOnceSetIsBounded(t *testing.T) {
	rec := &recorder{}
	q := NewLogQuarantiner(rec.logger())

	const subjects = maxQuarantineWarnings * 10
	for i := range subjects {
		if err := q.Quarantine(context.Background(), "sub-"+strconv.Itoa(i), "data_gap: storm"); err != nil {
			t.Fatalf("quarantine %d: %v", i, err)
		}
	}

	q.mu.Lock()
	held, queued := len(q.warned), len(q.order)
	q.mu.Unlock()

	if held > maxQuarantineWarnings || queued > maxQuarantineWarnings {
		t.Fatalf("after %d distinct subjects the say-once set holds %d entries and %d queued "+
			"(cap %d) — it is growing with the subjects a producer sends, which is the #892 leak: "+
			"one permanent entry per distinct wire subject, fastest during the storm the autopilot "+
			"exists to handle", subjects, held, queued, maxQuarantineWarnings)
	}
	// NON-VACUITY: a set that is bounded because it retains NOTHING would satisfy
	// the ceiling above while silently deleting the say-once behaviour the test
	// before this one pins.
	if held != maxQuarantineWarnings {
		t.Fatalf("say-once set holds %d entries after %d subjects, want a full %d — the window is "+
			"not the size it claims, so the ceiling above was met by retaining too little rather "+
			"than by evicting", held, subjects, maxQuarantineWarnings)
	}

	// AND THE EVICTOR RAN: sub-0 was pushed out long ago, so it warns again.
	before := len(rec.subjects(t))
	if err := q.Quarantine(context.Background(), "sub-0", "data_gap: storm"); err != nil {
		t.Fatalf("re-quarantine: %v", err)
	}
	if got := len(rec.subjects(t)); got != before+1 {
		t.Errorf("warn lines went %d → %d on a subject evicted %d insertions ago, want one more — "+
			"the ceiling is being met without anything leaving the set", before, got, subjects)
	}
	// While the most recent subject is still inside the window and stays silent,
	// so the line above is eviction rather than the suppression having been lost.
	before = len(rec.subjects(t))
	if err := q.Quarantine(context.Background(), "sub-"+strconv.Itoa(subjects-1), "data_gap: storm"); err != nil {
		t.Fatalf("re-quarantine: %v", err)
	}
	if got := len(rec.subjects(t)); got != before {
		t.Errorf("warn lines went %d → %d on the most recently seen subject, want no change — the "+
			"say-once suppression is gone and every repeat is now a log line", before, got)
	}
}

// LogModelRoller MUST RETAIN NOTHING, AND LogQuarantiner ONLY THE BOUNDED SET
// (#892).
//
// # What this is protecting
//
// Both types are production-wired (services/autopilot/cmd/autopilot/main.go).
// Both used to hold a map[string]string keyed off a wire DataQualityEvent
// subject — no cap, no TTL, no eviction — read by nothing but an accessor whose
// only callers were assertions.
//
// The fields are gone or bounded, so there is no count left to drive past. What
// a future change can still do is put the retention back — a ring "for
// inspection", a last-N buffer, a second per-subject map — and the estate-wide
// guard in test/arch/long_lived_maps_are_evicted_test.go would not see all of
// it: that walk's population is fields guarded by a sync mutex, and
// LogModelRoller no longer HAS a mutex, so a map added back to it is invisible
// there. This is default-deny over each type's own state instead: no field,
// transitively through kanz-owned types, may be a slice, map or channel except
// the two this repair deliberately kept.
func TestDefaultRemediatorsRetainNothingUnbounded(t *testing.T) {
	for _, tc := range []struct {
		ty    reflect.Type
		allow []string
	}{
		{reflect.TypeOf((*LogModelRoller)(nil)).Elem(), nil},
		{
			reflect.TypeOf((*LogQuarantiner)(nil)).Elem(),
			[]string{
				"LogQuarantiner.order []string",
				"LogQuarantiner.warned map[string]struct {}",
			},
		},
	} {
		t.Run(tc.ty.Name(), func(t *testing.T) {
			var containers []string
			fields := walkKanzContainers(tc.ty, tc.ty.Name(), 0, map[reflect.Type]bool{}, &containers)

			// NON-VACUITY: the walk actually saw the type's fields. If it is
			// renamed, emptied or replaced by an alias, every assertion below
			// passes over nothing.
			if fields == 0 {
				t.Fatalf("the field walk visited 0 fields of %s — the type has moved and this "+
					"guard's verdict is empty", tc.ty)
			}
			if _, ok := tc.ty.FieldByName("logger"); !ok {
				t.Fatalf("%s has no `logger` field — the remediator's sink has changed shape, so "+
					"this guard is no longer describing the type it was written about", tc.ty)
			}

			sort.Strings(containers)
			if !reflect.DeepEqual(containers, tc.allow) {
				t.Errorf("%s retains %v, want exactly %v.\n\n"+
					"A map or slice on a production-wired remediator grows once per distinct "+
					"data-quality SUBJECT — a string off the wire with no bounded universe — for "+
					"the life of the pod, and it fills fastest during the degraded period "+
					"autopilot exists to handle (#892). The warn line is the sink production "+
					"actually has; a ledger no production code reads is retention, not a sink.\n\n"+
					"If in-process retention is genuinely wanted, give it an explicit ceiling the "+
					"way LogQuarantiner.warned has one, and state the bound in a test here.",
					tc.ty, containers, tc.allow)
			}
		})
	}
}

// THE ROLLBACK LINE FIRES EVERY TIME, however many times (#892). This is the
// coverage that replaced the removed RolledBack() ledger: it asserts the same
// "a rollback happened, for this model, for this reason" property against the
// artifact production actually keeps.
func TestLogModelRollerLogsEveryRollback(t *testing.T) {
	// Well past anything a "last N for inspection" ring would have held, so a
	// reintroduced bounded buffer could not satisfy this assertion by accident.
	const rollbacks = 1000

	rec := &recorder{}
	r := NewLogModelRoller(rec.logger())

	for i := range rollbacks {
		if err := r.Rollback(context.Background(), "model-"+strconv.Itoa(i), "drift: input distribution"); err != nil {
			t.Fatalf("rollback %d: %v", i, err)
		}
	}

	got := rec.lines(t, rollbackMsg)
	if len(got) != rollbacks {
		t.Fatalf("rollback log lines = %d, want %d — the sink drops or coalesces rollbacks, so a "+
			"model reverted under drift can leave no record at all", len(got), rollbacks)
	}

	last := got[len(got)-1]
	for _, want := range []struct{ key, value string }{
		{"level", "WARN"},
		{"model_id", "model-" + strconv.Itoa(rollbacks-1)},
		{"reason", "drift: input distribution"},
	} {
		if got, ok := last[want.key].(string); !ok || got != want.value {
			t.Errorf("rollback line %s = %v, want %q", want.key, last[want.key], want.value)
		}
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
