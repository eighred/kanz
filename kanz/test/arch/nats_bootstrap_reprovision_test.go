package arch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"gopkg.in/yaml.v3"
)

// THE RE-PROVISIONING HOOK, EXERCISED RATHER THAN ASSERTED (#92).
//
// infra/nats/bootstrap-job.yaml is an Argo CD PostSync hook. #92's remaining
// clause is that it RE-PROVISIONS a broker that has lost its streams "without a
// hand-run command", and its own words for why that stayed open are exact: "A
// PostSync hook that has never fired is not a re-provisioning path, it is a
// manifest asserting one."
//
// WHAT STILL NEEDS A CLUSTER, AND IS NOT CLAIMED HERE: that ARGO fires it. That
// is an observation of a control plane, it needs #98, and nothing below pretends
// otherwise.
//
// WHAT DID NOT NEED ONE, AND HAD NO TEST: the hook's own contract. CI already
// extracts this script from the ConfigMap and runs it (kanz-ci.yml) — but only
// its EXIT CODE is checked. Nothing asserts what it left behind, nothing re-runs
// it, and nothing has ever taken a stream away and watched it come back. Three
// properties, all reachable on a laptop:
//
//   - every stream it declares is on the broker and is FILE-backed. The arch
//     suite checks the MANIFEST says --storage=file; nothing checked the running
//     stream. A memory-backed stream passes every other test in this repository
//     and loses every FACT the next time the broker restarts, which is the
//     unrecoverable half #92 was filed for;
//   - re-running changes nothing. Argo re-runs this on every sync, so a script
//     that was not idempotent would rewrite the estate's retention on each
//     deploy;
//   - a stream that has been DESTROYED is restored by a re-run, with the same
//     configuration. That is clause 2's mechanism with Argo taken out of it.
//
// Gated on TEST_NATS_URL. The two that RUN the script additionally need the
// `nats` CLI, which the CI runner does not have — CI runs the script inside
// natsio/nats-box for that reason — so they skip there and run locally. Stated
// rather than hidden: on CI this file proves the first property only.

// bootstrapScript returns the shell script the Argo hook runs, read out of the
// ConfigMap that ships it.
//
// FROM THE MANIFEST, NEVER A COPY. The script's own header says why: "A copy of
// the stream list in the CI workflow would drift from this one, and the drift
// would be invisible until a service met a real spine and found its subject
// unbound." A test carrying its own copy would be that same drift wearing a
// different hat.
func bootstrapScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "nats", "bootstrap-job.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var doc struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind == "ConfigMap" {
			if s, ok := doc.Data["bootstrap.sh"]; ok {
				return s
			}
		}
	}
	t.Fatal("no ConfigMap in infra/nats/bootstrap-job.yaml carries bootstrap.sh — this guard is " +
		"reading nothing, which is the failure it exists to prevent one level up")
	return ""
}

// runBootstrap runs the hook's script against url, exactly as the Job does.
func runBootstrap(t *testing.T, url string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "bootstrap.sh")
	if err := os.WriteFile(script, []byte(bootstrapScript(t)), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script)
	// NATS_REPLICAS exists in the script precisely so it can run against a single
	// node; production leaves it at 3. Setting it here is using the seam the
	// script provides, not working around it.
	cmd.Env = append(os.Environ(), "NATS_URL="+url, "NATS_REPLICAS=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the bootstrap hook FAILED against %s: %v\n%s", url, err, tail(string(out)))
	}
}

func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	return strings.Join(lines, "\n")
}

// brokerOrSkip dials TEST_NATS_URL or skips.
func brokerOrSkip(t *testing.T) (jetstream.JetStream, string) {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to exercise the nats-bootstrap hook against a real broker")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js, url
}

// natsCLIOrSkip skips when the `nats` binary the script drives is absent. The CI
// runner has no such binary — it runs the script inside natsio/nats-box — so the
// two tests that RUN the hook are local-only, and say so rather than passing
// vacuously.
func natsCLIOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("nats"); err != nil {
		t.Skip("the `nats` CLI is not on PATH; the bootstrap hook drives it, so this test cannot " +
			"run the real script here (CI runs it inside natsio/nats-box)")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh on PATH to run the hook's script")
	}
}

// EVERY STREAM THE HOOK DECLARES IS ON THE BROKER, AND IS FILE-BACKED.
//
// The arch suite already proves the MANIFEST says --storage=file
// (stateful_storage_durability_test.go). This is the other end of that chain: the
// stream that actually exists. They can differ — a stream created by hand during
// an incident, an older bootstrap, a broker restored from elsewhere — and a
// memory-backed one is invisible to every other check in this repository until
// the day the broker restarts and every FACT on it is gone.
func TestEveryBootstrappedStreamIsFileBackedOnTheBroker(t *testing.T) {
	js, _ := brokerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	declared := provisionedStreams(t)
	if len(declared) < 10 {
		t.Fatalf("parsed only %d streams from the manifest — the scan is broken and this guard is "+
			"asserting almost nothing", len(declared))
	}

	var missing, inMemory []string
	for name := range declared {
		s, err := js.Stream(ctx, name)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatalf("stream info %s: %v", name, err)
		}
		if info.Config.Storage != jetstream.FileStorage {
			inMemory = append(inMemory, name+" ("+info.Config.Storage.String()+")")
		}
	}
	sort.Strings(missing)
	sort.Strings(inMemory)

	if len(missing) > 0 {
		t.Errorf("the bootstrap hook declares %d stream(s) this broker does not carry: %s\n"+
			"  Every subject bound to them is unpublishable — a JetStream publish to an unbound "+
			"subject is a hard error — and a consumer on one 404s at startup, taking its service's "+
			"whole consumer group down.", len(missing), strings.Join(missing, ", "))
	}
	if len(inMemory) > 0 {
		t.Errorf("%d stream(s) on this broker are NOT file-backed: %s\n"+
			"  Every FACT on them is lost the next time the broker process dies, and nothing else "+
			"in this repository can see it: the manifest guard checks --storage=file in the YAML, "+
			"not the stream that exists (#92).", len(inMemory), strings.Join(inMemory, ", "))
	}
}

// RUNNING THE HOOK TWICE CHANGES NOTHING.
//
// Argo re-runs a PostSync hook on EVERY sync. A script that recreated rather than
// edited would reset retention on every deploy — and the estate's retentions are
// not decoration: MANDATE is compacted with no max age precisely because a
// mandate that ages off the stream is a portfolio that has quietly become
// ungoverned.
func TestRunningTheBootstrapHookTwiceChangesNothing(t *testing.T) {
	natsCLIOrSkip(t)
	js, url := brokerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	runBootstrap(t, url)
	before := topologyOf(t, ctx, js, provisionedStreams(t))
	runBootstrap(t, url)
	after := topologyOf(t, ctx, js, provisionedStreams(t))

	for name, b := range before {
		a, ok := after[name]
		if !ok {
			t.Errorf("%s disappeared when the hook ran a second time", name)
			continue
		}
		if a != b {
			t.Errorf("%s was RECONFIGURED by a second run of the hook:\n  before: %s\n  after:  %s\n"+
				"  Argo re-runs this on every sync, so that is the estate's retention changing "+
				"under it on each deploy.", name, b, a)
		}
	}
}

// A STREAM THE BROKER HAS LOST IS RESTORED BY THE HOOK, WITH ITS CONFIGURATION.
//
// This is #92's clause 2 with Argo taken out of it: the mechanism, not the
// observation. What remains cluster-gated is that Argo fires it; what is proven
// here is that when it fires, it restores.
//
// OBSERVABILITY IS THE ONE DELETED, deliberately: a 1-hour retention makes it the
// stream whose contents are least likely to be something another test is holding.
// The cleanup re-runs the hook whatever happens, so a failure mid-test cannot
// leave a shared broker short a stream.
func TestTheBootstrapHookRestoresAStreamTheBrokerHasLost(t *testing.T) {
	natsCLIOrSkip(t)
	js, url := brokerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const victim = "OBSERVABILITY"
	declared := provisionedStreams(t)
	if _, ok := declared[victim]; !ok {
		t.Fatalf("%s is no longer declared by the bootstrap hook — pick another stream for this "+
			"test rather than deleting the assertion", victim)
	}

	runBootstrap(t, url)
	want := topologyOf(t, ctx, js, map[string][]string{victim: declared[victim]})[victim]
	if want == "" {
		t.Fatalf("%s is not on the broker even after the hook ran; nothing can be proven about "+
			"restoring it", victim)
	}

	// WHATEVER HAPPENS BELOW, THE STREAM COMES BACK. Registered before the delete.
	t.Cleanup(func() { runBootstrap(t, url) })

	if err := js.DeleteStream(ctx, victim); err != nil {
		t.Fatalf("delete %s: %v", victim, err)
	}
	if _, err := js.Stream(ctx, victim); err == nil {
		t.Fatalf("%s still exists after being deleted — the rest of this test would prove nothing", victim)
	}

	runBootstrap(t, url)

	got := topologyOf(t, ctx, js, map[string][]string{victim: declared[victim]})[victim]
	if got == "" {
		t.Fatalf("the hook did NOT restore %s. #92's clause is that a broker which has lost its "+
			"streams is re-provisioned without a hand-run command; this is that clause failing at "+
			"the mechanism, before Argo is even involved.", victim)
	}
	if got != want {
		t.Errorf("%s came back DIFFERENT:\n  was:      %s\n  restored: %s\n"+
			"  A re-provisioning path that restores a stream with other retention or other "+
			"subjects has not restored it.", victim, want, got)
	}
}

// topologyOf renders each stream's configuration into one comparable line: the
// fields whose drift would change what the estate keeps.
func topologyOf(t *testing.T, ctx context.Context, js jetstream.JetStream, streams map[string][]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name := range streams {
		s, err := js.Stream(ctx, name)
		if err != nil {
			continue // absent: reported by the caller that cares
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatalf("stream info %s: %v", name, err)
		}
		subs := append([]string(nil), info.Config.Subjects...)
		sort.Strings(subs)
		out[name] = strings.Join([]string{
			"subjects=" + strings.Join(subs, ","),
			"max_age=" + info.Config.MaxAge.String(),
			"storage=" + info.Config.Storage.String(),
			"retention=" + info.Config.Retention.String(),
			"max_msgs_per_subject=" + strconv.FormatInt(info.Config.MaxMsgsPerSubject, 10),
		}, " ")
	}
	return out
}
