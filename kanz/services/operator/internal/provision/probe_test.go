package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// jobRecorder keeps the Jobs Probe created and the names it deleted, since Probe
// deletes on its way out and the objects are gone by the time it returns.
type jobRecorder struct {
	created []*batchv1.Job
	deleted []string
}

// probeCluster returns a fake clientset that stands in for the one behaviour Probe
// depends on and a bare fake does not have: the Job controller creating the Job's pod.
// The pod appears already terminated with msg as its termination message, which is the
// exact seam Probe reads — without it Probe would poll an empty namespace until it
// timed out, and every test would prove nothing but the timeout path.
func probeCluster(msg string) (*fake.Clientset, *jobRecorder) {
	cs := fake.NewSimpleClientset()
	rec := &jobRecorder{}

	cs.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		job, ok := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		if !ok {
			return false, nil, nil
		}
		rec.created = append(rec.created, job.DeepCopy())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: job.Name + "-pod", Namespace: job.Namespace,
				// The Job controller copies the pod template's labels onto the pod; that copy
				// is what makes awaitProbe's label selector find it.
				Labels: job.Spec.Template.Labels,
			},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "probe",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}},
			}}},
		}
		// Tracker().Add, NOT the typed client: Invokes holds the Fake's lock while it runs
		// reactors, so calling the clientset from inside one deadlocks.
		if err := cs.Tracker().Add(pod); err != nil {
			return true, nil, err
		}
		return false, nil, nil // fall through so the Job itself is still stored
	})
	cs.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		rec.deleted = append(rec.deleted, a.(k8stesting.DeleteAction).GetName())
		return false, nil, nil
	})
	return cs, rec
}

// TestProbePodCarriesComponentLabel is the guard on the property this whole design
// rests on. node-provisioner-egress selects pods by app.kubernetes.io/component, and
// it is the ONLY policy in the namespace that permits :22 — the operator's own egress
// has no such rule by design. Without this label on the POD TEMPLATE (the Job object's
// labels are irrelevant to a NetworkPolicy) the probe pod gets deny-by-default egress,
// still exits cleanly, and reports every target on earth unreachable.
func TestProbePodCarriesComponentLabel(t *testing.T) {
	cs, rec := probeCluster("reachable 3")
	if _, err := New(cs, cfg()).Probe(context.Background(), "10.0.0.5", 22); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(rec.created) != 1 {
		t.Fatalf("want exactly 1 probe job created, got %d", len(rec.created))
	}
	tmpl := rec.created[0].Spec.Template
	if tmpl.Labels[componentLabel] != componentValue {
		t.Errorf("probe pod template labels = %v, want %s=%s — node-provisioner-egress selects on "+
			"this label and is the only policy granting :22, so without it every probe reports "+
			"unreachable no matter what the target is doing",
			tmpl.Labels, componentLabel, componentValue)
	}
}

// TestProbeReturnsLatencyFromTerminationMessage proves the happy path end to end
// through the one channel the operator reads: the terminated container's message.
func TestProbeReturnsLatencyFromTerminationMessage(t *testing.T) {
	cs, _ := probeCluster("reachable 7\n")
	got, err := New(cs, cfg()).Probe(context.Background(), "10.0.0.5", 2222)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !got.Reachable || got.LatencyMS != 7 || got.Message != "" {
		t.Errorf("Probe = %+v, want {Reachable:true LatencyMS:7 Message:\"\"}", got)
	}
}

func TestProbeReturnsTrimmedReasonWhenUnreachable(t *testing.T) {
	cs, _ := probeCluster("unreachable connection refused\n")
	got, err := New(cs, cfg()).Probe(context.Background(), "10.0.0.5", 22)
	if err != nil {
		t.Fatalf("an unreachable host is a RESULT, not an error: %v", err)
	}
	if got.Reachable {
		t.Error("want Reachable:false")
	}
	if got.Message != "connection refused" {
		t.Errorf("Message = %q, want %q", got.Message, "connection refused")
	}
}

// TestProbeTargetsTheRequestedAddress covers the default-port contract the RPC relies
// on and proves the address reaches the container as env rather than as a mount or arg.
func TestProbeTargetsTheRequestedAddress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ip       string
		port     int32
		wantAddr string
	}{
		{"explicit port", "10.0.0.5", 2222, "10.0.0.5:2222"},
		{"zero means 22", "10.0.0.6", 0, "10.0.0.6:22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, rec := probeCluster("reachable 1")
			if _, err := New(cs, cfg()).Probe(context.Background(), tc.ip, tc.port); err != nil {
				t.Fatalf("Probe: %v", err)
			}
			env := map[string]string{}
			for _, c := range rec.created[0].Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					env[e.Name] = e.Value
				}
			}
			if env["PROVISION_TARGET_ADDR"] != tc.wantAddr {
				t.Errorf("PROVISION_TARGET_ADDR = %q, want %q", env["PROVISION_TARGET_ADDR"], tc.wantAddr)
			}
			if env["PROVISION_MODE"] != "probe" {
				t.Errorf("PROVISION_MODE = %q, want probe — anything else joins the host instead of "+
					"pinging it", env["PROVISION_MODE"])
			}
		})
	}
}

// TestProbeMountsNoCredential is the security property that justifies a second Job
// shape existing at all: the probe pod must carry no bootstrap key, no k3s token, and
// no Secret to mount one from. A probe is triggerable by any caller who can reach the
// RPC; a probe that provisioned a key into a pod would make that a credential-minting
// endpoint.
func TestProbeMountsNoCredential(t *testing.T) {
	cs, rec := probeCluster("reachable 1")
	if _, err := New(cs, cfg()).Probe(context.Background(), "10.0.0.5", 22); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	spec := rec.created[0].Spec.Template.Spec
	if len(spec.Volumes) != 0 {
		t.Errorf("probe pod has %d volume(s) %+v, want none", len(spec.Volumes), spec.Volumes)
	}
	for _, c := range spec.Containers {
		if len(c.VolumeMounts) != 0 {
			t.Errorf("probe container %s mounts %+v, want nothing", c.Name, c.VolumeMounts)
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				t.Errorf("probe container reads env %s from a Secret — it needs no credential", e.Name)
			}
			if e.Name == "K3S_TOKEN" || e.Name == "PROVISION_SSH_KEY_FILE" || e.Name == "PROVISION_SSH_USER" {
				t.Errorf("probe container carries %s — the probe has no use for a credential", e.Name)
			}
		}
	}
	secs, _ := cs.CoreV1().Secrets("kanz-operator").List(context.Background(), metav1.ListOptions{})
	if len(secs.Items) != 0 {
		t.Errorf("Probe created %d Secret(s) — it must create none", len(secs.Items))
	}
}

// TestProbeIsNotRetriedAndIsReclaimed: BackoffLimit 0 because a retry would answer a
// different dial than the one asked about, and both reclaim paths present because a
// probe is triggerable at will — a leak per attempt fills the namespace with pods that
// hold :22 egress.
func TestProbeIsNotRetriedAndIsReclaimed(t *testing.T) {
	cs, rec := probeCluster("reachable 1")
	if _, err := New(cs, cfg()).Probe(context.Background(), "10.0.0.5", 22); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	job := rec.created[0]
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %v, want 0 — one dial, one answer", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Error("TTLSecondsAfterFinished is unset — a probe whose operator died would leak its Job")
	}
	if len(rec.deleted) != 1 || rec.deleted[0] != job.Name {
		t.Errorf("deleted jobs = %v, want exactly [%s]", rec.deleted, job.Name)
	}
	jobs, _ := cs.BatchV1().Jobs("kanz-operator").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("%d probe job(s) left behind", len(jobs.Items))
	}
}

// TestProbeJobIsInvisibleToListProvisions: List selects Jobs by componentLabel, so
// putting that label on the probe JOB (as opposed to its pod template, where it is
// required) would surface every probe in the TUI's provisioning list as a phantom node.
func TestProbeJobIsInvisibleToListProvisions(t *testing.T) {
	cs, rec := probeCluster("reachable 1")
	p := New(cs, cfg())
	if _, err := p.Probe(context.Background(), "10.0.0.5", 22); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := rec.created[0].Labels[componentLabel]; got != "" {
		t.Errorf("probe Job object carries %s=%q — ListProvisions selects on that label and would "+
			"report this probe as a node being provisioned", componentLabel, got)
	}
	// Put the Job back exactly as Probe built it (Probe deleted its own on the way out)
	// and prove List ignores it. Straight onto the tracker, since going through the
	// clientset would re-fire the create reactor and its pod.
	if err := cs.Tracker().Add(rec.created[0]); err != nil {
		t.Fatal(err)
	}
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %+v, want nothing — a probe is not a provisioning", got)
	}
}

func TestProbeRejectsEmptyIP(t *testing.T) {
	cs, rec := probeCluster("reachable 1")
	if _, err := New(cs, cfg()).Probe(context.Background(), "", 22); err == nil {
		t.Fatal("expected an error for an empty ip")
	}
	if len(rec.created) != 0 {
		t.Errorf("a rejected request still created %d job(s)", len(rec.created))
	}
}

func TestProbeSurfacesJobCreateFailure(t *testing.T) {
	cs, _ := probeCluster("reachable 1")
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})
	if _, err := New(cs, cfg()).Probe(context.Background(), "10.0.0.5", 22); err == nil {
		t.Fatal("expected the Job-create failure to surface")
	}
}

// TestParseProbeMessageDistinguishesBrokenFromUnreachable is the assertion that keeps
// the two failure modes apart. A probe that could not run must NEVER decode to
// Reachable:false: that would accuse a healthy target of being down, and the operator
// would go fix the wrong thing.
func TestParseProbeMessageDistinguishesBrokenFromUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		msg       string
		wantErr   bool
		want      ProbeResult
		errSubstr string
	}{
		{name: "reachable", msg: "reachable 12", want: ProbeResult{Reachable: true, LatencyMS: 12}},
		{name: "reachable with newline", msg: "reachable 0\n", want: ProbeResult{Reachable: true}},
		{name: "unreachable", msg: "unreachable i/o timeout\n",
			want: ProbeResult{Message: "i/o timeout"}},
		{name: "empty means broken", msg: "", wantErr: true, errSubstr: "without a termination message"},
		{name: "whitespace means broken", msg: " \n", wantErr: true, errSubstr: "without a termination message"},
		{name: "reachable without latency", msg: "reachable", wantErr: true, errSubstr: "without a latency"},
		{name: "unparseable latency", msg: "reachable soon", wantErr: true, errSubstr: "unparseable latency"},
		{name: "unknown verdict", msg: "OOMKilled", wantErr: true, errSubstr: "unrecognised"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProbeMessage(tc.msg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseProbeMessage(%q) = %+v, want an error — a broken probe must not "+
						"decode to an unreachable host", tc.msg, got)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("err = %v, want it to mention %q", err, tc.errSubstr)
				}
				if got.Reachable {
					t.Error("an error result must not also claim Reachable")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProbeMessage(%q): %v", tc.msg, err)
			}
			if got != tc.want {
				t.Errorf("parseProbeMessage(%q) = %+v, want %+v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestProbeErrorsWhenPodNeverTerminates: a pod that never answers must produce an error
// naming the timeout, not a silent Reachable:false. awaitProbe takes the timeout as a
// parameter precisely so this path is testable in milliseconds.
func TestProbeErrorsWhenPodNeverTerminates(t *testing.T) {
	cs := fake.NewSimpleClientset() // no pod at all: nothing ever terminates
	_, err := New(cs, cfg()).awaitProbe(context.Background(), "probe-abcdefgh", 30*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "did not complete in time") {
		t.Errorf("err = %v, want it to say the probe Job did not complete in time", err)
	}
}

// TestProbeIgnoresAnotherProbesPod proves the pod lookup is scoped to this Job. Two
// concurrent probes must not read each other's verdicts.
func TestProbeIgnoresAnotherProbesPod(t *testing.T) {
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "probe-other-pod", Namespace: "kanz-operator",
			Labels: map[string]string{componentLabel: componentValue, probeJobLabel: "probe-other"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "probe",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "reachable 999"}},
		}}},
	}
	cs := fake.NewSimpleClientset(other)
	_, err := New(cs, cfg()).awaitProbe(context.Background(), "probe-mine", 30*time.Millisecond)
	if err == nil {
		t.Fatal("awaitProbe read a different probe's pod")
	}
}

func TestProbeNamesDoNotCollide(t *testing.T) {
	cs, rec := probeCluster("reachable 1")
	p := New(cs, cfg())
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		if _, err := p.Probe(context.Background(), "10.0.0.5", 22); err != nil {
			t.Fatalf("Probe %d: %v", i, err)
		}
		name := rec.created[i].Name
		if seen[name] {
			t.Fatalf("probe job name %q reused — concurrent probes would collide", name)
		}
		seen[name] = true
	}
}
