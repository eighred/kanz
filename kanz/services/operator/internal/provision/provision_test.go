package provision

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func cfg() Config {
	return Config{Namespace: "kanz-operator", ProvisionerImage: "ghcr.io/kanz-eng/kanz-provisioner:latest",
		K3sServerURL: "https://cp:6443", K3sToken: "tok"}
}

// testHostKey is the target's public host key as an AddNode would carry it —
// authorized_keys format, public material, never secret.
const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestHostKeyForProvisionerJobEnvXX"

func TestAddNodeCreatesJobAndOwnedSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	p := New(cs, cfg())
	id, err := p.AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM"),
		SSHHostKey: testHostKey})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	// The Secret must exist and be OWNED by the Job (GC cascades).
	secList, _ := cs.CoreV1().Secrets("kanz-operator").List(context.Background(), metav1.ListOptions{})
	if len(secList.Items) != 1 {
		t.Fatalf("want 1 secret, got %d", len(secList.Items))
	}
	sec := secList.Items[0]
	if string(sec.Data["ssh_key"]) != "PEM" {
		t.Errorf("secret does not carry the bootstrap key")
	}
	if string(sec.Data["k3s_token"]) != "tok" {
		t.Errorf("k3s token not stored in the Job-owned Secret")
	}
	owned := false
	for _, or := range sec.OwnerReferences {
		if or.Kind == "Job" && or.Name == job.Name {
			owned = true
		}
	}
	if !owned {
		t.Errorf("secret is not owned by the job — it could orphan")
	}
	// The pod TEMPLATE (not just the Job object) must carry the component label, or the
	// node-provisioner-egress NetworkPolicy selects zero pods and the provisioner pod
	// (holds the SSH key + k3s token) gets unrestricted egress.
	if job.Spec.Template.Labels["app.kubernetes.io/component"] != "node-provisioner" {
		t.Errorf("pod template missing the node-provisioner label — the egress NetworkPolicy would not select it: %v", job.Spec.Template.Labels)
	}
	// The key must NOT appear in the Job's pod spec in plaintext (only mounted from the Secret).
	// The k3s token must likewise never appear as a plaintext env Value; it must come from
	// the Job-owned Secret via secretKeyRef.
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Value == "PEM" {
				t.Errorf("bootstrap key leaked into Job env in plaintext")
			}
			if e.Value == "tok" {
				t.Errorf("k3s token leaked into Job env as a plaintext value")
			}
			if e.Name == "K3S_TOKEN" {
				if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
					e.ValueFrom.SecretKeyRef.Key != "k3s_token" ||
					e.ValueFrom.SecretKeyRef.Name != job.Name {
					t.Errorf("K3S_TOKEN must come from a secretKeyRef to the Job-owned secret, got %+v", e)
				}
			}
		}
	}

	// THE HOST KEY MUST REACH THE POD, or the provisioner fails closed on every
	// join and the whole plumbing is decorative. It is deliberately a plaintext
	// env Value and not a secretKeyRef: it is the target's PUBLIC key, and
	// routing it through the Job-owned Secret would tell the next reader it is
	// confidential when the two entries beside it genuinely are.
	var gotHostKey string
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "PROVISION_HOST_KEY" {
				gotHostKey = e.Value
				if e.ValueFrom != nil {
					t.Errorf("PROVISION_HOST_KEY should be a plain value — it is public material: %+v", e)
				}
			}
		}
	}
	if gotHostKey != testHostKey {
		t.Errorf("PROVISION_HOST_KEY = %q, want %q — without it sshRun refuses to dial and the "+
			"join never runs", gotHostKey, testHostKey)
	}
}

func TestAddNodeDeletesJobIfSecretCreateFails(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("secret create boom")
	})
	_, err := New(cs, cfg()).AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err == nil {
		t.Fatal("expected an error when secret create fails")
	}
	jobs, _ := cs.BatchV1().Jobs("kanz-operator").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("job was not cleaned up after secret-create failure: %d jobs remain", len(jobs.Items))
	}
}

func TestListMapsJobStatus(t *testing.T) {
	cs := fake.NewSimpleClientset(
		provJob("p-joined", "london", batchv1.JobStatus{Succeeded: 1}),
		provJob("p-failed", "tokyo", batchv1.JobStatus{Failed: 1}),
		provJob("p-running", "paris", batchv1.JobStatus{Active: 1}),
	)
	got, err := New(cs, cfg()).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]Provision{}
	for _, pr := range got {
		byID[pr.ID] = pr
	}
	if byID["p-joined"].Status != StatusJoined {
		t.Errorf("joined = %v", byID["p-joined"].Status)
	}
	if byID["p-failed"].Status != StatusFailed {
		t.Errorf("failed = %v", byID["p-failed"].Status)
	}
	if byID["p-running"].Status != StatusInstalling {
		t.Errorf("running = %v", byID["p-running"].Status)
	}
	if byID["p-joined"].Hostname != "london" {
		t.Errorf("hostname not surfaced")
	}
}

// TestAddNodeSetsImagePullPolicyWhenConfigured proves the fix for the
// ImagePullBackOff defect: with ImagePullPolicy configured, every provisioner
// container must carry it explicitly rather than relying on Kubernetes'
// :latest-tag default of Always, which fails closed on any node that cannot
// reach ghcr (air-gapped estates, disaster-recovery rebuilds) even when the
// image is already present locally.
func TestAddNodeSetsImagePullPolicyWhenConfigured(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := cfg()
	c.ImagePullPolicy = "IfNotPresent"
	id, err := New(cs, c).AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.ImagePullPolicy != "IfNotPresent" {
			t.Errorf("container %s ImagePullPolicy = %q, want IfNotPresent", c.Name, c.ImagePullPolicy)
		}
	}
}

// TestAddNodeLeavesImagePullPolicyUnsetByDefault proves the production
// contract this fix must not disturb: with ImagePullPolicy left empty (the
// config default), the field must be the zero value so Kubernetes' own
// defaulting decides — never a hardcoded policy chosen in this package.
func TestAddNodeLeavesImagePullPolicyUnsetByDefault(t *testing.T) {
	cs := fake.NewSimpleClientset()
	id, err := New(cs, cfg()).AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.ImagePullPolicy != "" {
			t.Errorf("container %s ImagePullPolicy = %q, want zero value (Kubernetes decides)", c.Name, c.ImagePullPolicy)
		}
	}
}

// TestAddNodeSetsFSGroupToMatchRunAsUser proves the fix for the permission-denied
// defect found on a live cluster: `kanz-provisioner: read bootstrap key
// /etc/provision/ssh_key: permission denied`. Kubernetes owns Secret volume files
// root:fsGroup; with no fsGroup set, that group is root, and the container (which
// must run as a non-root uid per RunAsNonRoot) can never read its own credential.
// FSGroup must equal RunAsUser so the pod's own uid's group can read the mount.
func TestAddNodeSetsFSGroupToMatchRunAsUser(t *testing.T) {
	cs := fake.NewSimpleClientset()
	id, err := New(cs, cfg()).AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || sc.FSGroup == nil {
		t.Fatalf("pod securityContext must set both RunAsUser and FSGroup, got %+v", sc)
	}
	if *sc.FSGroup != *sc.RunAsUser {
		t.Errorf("FSGroup (%d) must equal RunAsUser (%d) — Secret volume files are owned "+
			"root:fsGroup, so a mismatched group locks the non-root container out of its "+
			"own bootstrap-key mount", *sc.FSGroup, *sc.RunAsUser)
	}
	if *sc.RunAsUser != 65532 {
		t.Errorf("RunAsUser = %d, want 65532", *sc.RunAsUser)
	}
}

// TestAddNodeBootstrapKeyModeIsGroupReadable proves the other half of the same
// live-cluster defect: DefaultMode 0400 (owner-only) plus a non-root container
// is unreadable, not strict — the owning uid is root, not the container's uid,
// so 0400 never granted the container read access on any run of this path. This
// is a regression guard: 0440 (root and the pod's fsGroup may read; no world
// access) must not silently regress back to a mode only root can read.
func TestAddNodeBootstrapKeyModeIsGroupReadable(t *testing.T) {
	cs := fake.NewSimpleClientset()
	id, err := New(cs, cfg()).AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	var mode *int32
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "bootstrap-key" && v.Secret != nil {
			mode = v.Secret.DefaultMode
		}
	}
	if mode == nil {
		t.Fatalf("bootstrap-key secret volume not found")
	}
	if *mode != 0o440 {
		t.Errorf("bootstrap-key DefaultMode = %#o, want 0440 (0400 fails at runtime for a "+
			"non-root container: the file is owned root:fsGroup, not root:root, so owner-only "+
			"read excludes the container's own gid)", *mode)
	}
}

func provJob(name, host string, st batchv1.JobStatus) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kanz-operator",
			Labels:      map[string]string{"app.kubernetes.io/component": "node-provisioner"},
			Annotations: map[string]string{"kanz.io/provision-hostname": host},
		},
		Status: st,
	}
}

// TestAddNodeJobRunsUnderProvisionerServiceAccount pins the inheritance that
// OPS-M2f-b relies on. The ghcr-pull credential is attached to the
// kanz-node-provisioner ServiceAccount in infra/deploy/operator-deploy.yaml,
// NOT to this Job's pod spec. So the Job can pull its private image only for
// as long as it keeps naming that ServiceAccount. Renaming it here — or
// dropping it and falling back to the namespace default SA — silently
// reintroduces the ErrImagePull this milestone exists to remove, on a code
// path that has no manifest a reviewer would think to check.
func TestAddNodeJobRunsUnderProvisionerServiceAccount(t *testing.T) {
	cs := fake.NewSimpleClientset()
	id, err := New(cs, cfg()).AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	if got := job.Spec.Template.Spec.ServiceAccountName; got != "kanz-node-provisioner" {
		t.Errorf("provisioning Job ServiceAccountName = %q, want %q — the ghcr-pull "+
			"credential is attached to that ServiceAccount, so any other value means "+
			"this Job cannot pull its own image", got, "kanz-node-provisioner")
	}
}

// TestAddNodeJobIsReclaimedEvenIfTheOperatorDies is the credential half of the
// probe's TestProbeIsNotRetriedAndIsReclaimed.
//
// AddNode deletes this Job explicitly (provision.go), and that covers every path
// where the operator survives. It is the path where the operator does NOT survive
// that this asserts: a rollout, an OOM, or a drain of the control-plane node the
// operator is pinned to, landing between Create and Delete.
//
// ActiveDeadlineSeconds does not cover it. That field bounds how long the POD may
// RUN; it terminates the pod and marks the Job Failed/DeadlineExceeded, and the
// Job OBJECT — with the owner-referenced Secret holding the SSH bootstrap key and
// the K3S_TOKEN cluster-admission token — persists until something deletes it.
// Only TTLSecondsAfterFinished reclaims a finished Job.
//
// The probe Job has carried this since it shipped, and the probe carries no
// directly-mounted credential. This Job does.
func TestAddNodeJobIsReclaimedEvenIfTheOperatorDies(t *testing.T) {
	cs := fake.NewSimpleClientset()
	id, err := New(cs, cfg()).AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("provisioning Job has no TTLSecondsAfterFinished — if the operator dies between " +
			"creating this Job and deleting it, the Job and its owner-referenced Secret persist " +
			"indefinitely, leaving the SSH bootstrap key and the k3s cluster-admission token in " +
			"the cluster with nothing to reclaim them. ActiveDeadlineSeconds does not do this: it " +
			"bounds the pod's RUNTIME, not the Job object's LIFETIME")
	}
	if got := *job.Spec.TTLSecondsAfterFinished; got <= 0 {
		t.Errorf("TTLSecondsAfterFinished = %d, want > 0 — a non-positive TTL is not a bound", got)
	}
}
