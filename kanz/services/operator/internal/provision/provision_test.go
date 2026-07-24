package provision

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func cfg() Config {
	return Config{Namespace: "kanz-operator", ProvisionerImage: "ghcr.io/kanz-eng/kanz-provisioner:latest",
		K3sServerURL: "https://cp:6443", K3sToken: "tok"}
}

func TestAddNodeCreatesJobAndOwnedSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	p := New(cs, cfg())
	id, err := p.AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
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
	owned := false
	for _, or := range sec.OwnerReferences {
		if or.Kind == "Job" && or.Name == job.Name {
			owned = true
		}
	}
	if !owned {
		t.Errorf("secret is not owned by the job — it could orphan")
	}
	// The key must NOT appear in the Job's pod spec in plaintext (only mounted from the Secret).
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Value == "PEM" {
				t.Errorf("bootstrap key leaked into Job env in plaintext")
			}
		}
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
