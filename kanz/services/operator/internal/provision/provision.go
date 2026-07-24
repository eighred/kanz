// Package provision is the operator service's node-provisioning orchestrator: it
// turns an AddNode request into a short-lived Secret (the bootstrap SSH key) and a
// one-shot kanz-provisioner Job that owns it, and it derives provisioning status
// from the Jobs' own status. It holds no SSH itself — crypto/ssh lives only in the
// provisioner Job (cmd/kanz-provisioner).
package provision

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
)

const (
	componentLabel = "app.kubernetes.io/component"
	componentValue = "node-provisioner"
	hostnameAnnot  = "kanz.io/provision-hostname"
	keySecretKey   = "ssh_key"
	keyMountPath   = "/etc/provision"
)

// Config is the orchestrator's deploy-time configuration.
type Config struct {
	Namespace        string
	ProvisionerImage string
	K3sServerURL     string
	K3sToken         string
}

// Request is one AddNode call, decoded from the RPC.
type Request struct {
	Hostname string
	IP       string
	SSHPort  int32
	SSHUser  string
	SSHKey   []byte
}

// Status is the coarse provisioning status derived from a Job.
type Status int

const (
	StatusPending Status = iota
	StatusInstalling
	StatusJoined
	StatusFailed
)

// Provision is one node provisioning, surfaced to ListProvisions.
type Provision struct {
	ID       string
	Hostname string
	Status   Status
	Message  string
}

// Provisioner creates the Secret+Job and reads status back.
type Provisioner struct {
	cs  kubernetes.Interface
	cfg Config
}

func New(cs kubernetes.Interface, cfg Config) *Provisioner { return &Provisioner{cs: cs, cfg: cfg} }

// AddNode creates the one-shot Job first, then creates the bootstrap-key Secret already
// owner-referenced to it — there is never a moment when a credential-bearing Secret
// exists un-owned. Returns the Job name as the provision id.
func (p *Provisioner) AddNode(ctx context.Context, r Request) (string, error) {
	if r.IP == "" || r.SSHUser == "" || len(r.SSHKey) == 0 {
		return "", fmt.Errorf("ip, ssh_user and ssh_private_key are required")
	}
	port := r.SSHPort
	if port == 0 {
		port = 22
	}
	name := "provision-" + sanitize(r.Hostname) + "-" + rand.String(5)

	// 1. Create the Job FIRST. Its UID is assigned synchronously on Create, so the
	//    Secret can then be created ALREADY owner-referenced to it — there is never a
	//    moment when a credential-bearing Secret exists un-owned. A crash after this but
	//    before the Secret leaves NO key material to orphan; the pod merely fails to
	//    mount the not-yet-created Secret and the Job self-expires at ActiveDeadlineSeconds.
	job, err := p.cs.BatchV1().Jobs(p.cfg.Namespace).Create(ctx, p.jobSpec(name, r, port), metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create provisioning job: %w", err)
	}

	// 2. Create the bootstrap-key Secret already owned by the Job (Kubernetes GCs it with
	//    the Job; it can never be orphaned). If this fails, best-effort delete the Job so a
	//    doomed mount-pending pod does not linger for the full deadline.
	_, err = p.cs.CoreV1().Secrets(p.cfg.Namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: p.cfg.Namespace,
			Labels:          map[string]string{componentLabel: componentValue},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
		},
		Data: map[string][]byte{keySecretKey: r.SSHKey},
	}, metav1.CreateOptions{})
	if err != nil {
		_ = p.cs.BatchV1().Jobs(p.cfg.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{})
		return "", fmt.Errorf("create bootstrap secret: %w", err)
	}
	return job.Name, nil
}

func (p *Provisioner) jobSpec(name string, r Request, port int32) *batchv1.Job {
	var backoff int32 = 0    // one attempt; a retry would re-SSH, and the operator re-issues AddNode
	var deadline int64 = 900 // 15m hard cap on a provisioning attempt — a belt to sshRun's ctx, so a
	//                          wedged Job cannot linger indefinitely holding the bootstrap-key Secret.
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: p.cfg.Namespace,
			Labels:      map[string]string{componentLabel: componentValue},
			Annotations: map[string]string{hostnameAnnot: r.Hostname},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{componentLabel: componentValue},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "kanz-node-provisioner",
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr64(65532),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "provisioner",
						Image: p.cfg.ProvisionerImage,
						Env: []corev1.EnvVar{
							{Name: "PROVISION_TARGET_ADDR", Value: fmt.Sprintf("%s:%d", r.IP, port)},
							{Name: "PROVISION_SSH_USER", Value: r.SSHUser},
							{Name: "K3S_SERVER_URL", Value: p.cfg.K3sServerURL},
							{Name: "K3S_TOKEN", Value: p.cfg.K3sToken},
							{Name: "PROVISION_SSH_KEY_FILE", Value: keyMountPath + "/" + keySecretKey},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "bootstrap-key", MountPath: keyMountPath, ReadOnly: true}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
							RunAsNonRoot: ptr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "bootstrap-key",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: name, DefaultMode: ptr32(0o400)}},
					}},
				},
			},
		},
	}
}

// List returns every provisioning Job's status.
func (p *Provisioner) List(ctx context.Context) ([]Provision, error) {
	jobs, err := p.cs.BatchV1().Jobs(p.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: componentLabel + "=" + componentValue})
	if err != nil {
		return nil, fmt.Errorf("list provisioning jobs: %w", err)
	}
	out := make([]Provision, 0, len(jobs.Items))
	for i := range jobs.Items {
		j := &jobs.Items[i]
		out = append(out, Provision{
			ID:       j.Name,
			Hostname: j.Annotations[hostnameAnnot],
			Status:   statusOf(j),
			Message:  failureMessage(j),
		})
	}
	return out, nil
}

func statusOf(j *batchv1.Job) Status {
	switch {
	case j.Status.Succeeded > 0:
		return StatusJoined
	case j.Status.Failed > 0:
		return StatusFailed
	case j.Status.Active > 0:
		return StatusInstalling
	default:
		return StatusPending
	}
}

func failureMessage(j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.Message
		}
	}
	return ""
}

// sanitize lowercases and keeps DNS-label-safe chars so the Job name is valid.
func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "node"
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func ptr(b bool) *bool     { return &b }
func ptr32(i int32) *int32 { return &i }
func ptr64(i int64) *int64 { return &i }
