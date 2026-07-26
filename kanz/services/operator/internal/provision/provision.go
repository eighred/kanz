// Package provision is the operator service's node-provisioning orchestrator: it
// turns an AddNode request into a short-lived Secret (the bootstrap SSH key) and a
// one-shot kanz-provisioner Job that owns it, and it derives provisioning status
// from the Jobs' own status. It holds no SSH itself — crypto/ssh lives only in the
// provisioner Job (cmd/kanz-provisioner).
//
// Probe follows the same rule one step further: the operator holds no :22 egress
// either. A pre-flight reachability dial at a caller-supplied address runs in an
// ephemeral Job carrying the provisioner's pod identity, which is the only identity
// in this namespace permitted to reach an arbitrary host's SSH port.
package provision

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	k3sTokenKey    = "k3s_token"
	keyMountPath   = "/etc/provision"
	// probeJobLabel carries the probe Job's own name onto its pod, so Probe can find
	// that pod's status by label. Deliberately not the `job-name` label the Job
	// controller injects: which of its two spellings a cluster sets depends on the
	// Kubernetes version, and a probe that silently finds no pod is indistinguishable
	// from a probe that timed out.
	probeJobLabel = "kanz.io/probe-job"
	// controlPlaneRoleLabel/Value is the node label marking a control-plane node. The
	// pair is COPIED FROM infra/deploy/operator-deploy.yaml's nodeSelector on the
	// operator Deployment and must stay identical to it: a nodeSelector is an exact
	// string match, k3s labels server nodes with the value "true" (this estate) while
	// kubeadm and kind use an EMPTY value, so a mismatched value does not schedule
	// somewhere wrong — it leaves the pod Pending forever. test/arch asserts the two
	// spellings agree.
	controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"
	controlPlaneRoleValue = "true"
)

// controlPlaneOnly and tolerateControlPlane are the placement BOTH Jobs below carry.
// Declared once and shared by jobSpec and probeJobSpec deliberately: this is a
// security constraint, and two copies of a security constraint drift apart — one gets
// tuned, the other is forgotten, and the forgotten one is the hole.
//
// THE FIRST REASON IS CREDENTIAL CONFINEMENT. The provisioning Job mounts a Job-owned
// Secret carrying the SSH bootstrap key AND K3S_TOKEN — the cluster-admission token,
// the single credential that lets any host join this cluster. Unpinned, that pod is
// scheduled onto an arbitrary worker, including a worker someone provisioned minutes
// earlier by typing an address into the Add Node form. Landing there copies the
// credential that admits the fleet onto a member of the fleet it admits. The blast
// radius of one compromised worker stops being that worker.
//
// THE SECOND REASON IS RETIRED, AND IS RECORDED HERE BECAUSE ITS ABSENCE IS THE
// POINT. Until OPS-M2f-b this Job also had to be pinned because the estate held no
// registry credentials at all: ghcr is private, nothing in infra/ carried
// imagePullSecrets, and a Job landing on a node that had not pre-loaded the
// kanz-provisioner image died in ErrImagePull. Observed live — a probe Job landed on
// a freshly joined worker, 403'd, and reported a healthy host unreachable after the
// full 60s wait, a false verdict about someone's node caused entirely by where the
// pod ran. That reason is gone: the kanz-node-provisioner ServiceAccount now carries
// the ghcr-pull credential, and this Job inherits it (pinned by
// TestAddNodeJobRunsUnderProvisionerServiceAccount). So credential confinement above
// is now the ONLY thing holding this pin — which is exactly what OPS-M2f-a has to
// replace, and it should not have to rediscover that the other half already expired.
//
// The toleration grants nothing on THIS cluster: the k3s control-plane node carries no
// taints today, so the selector alone places both pods. It is carried anyway for the
// same reason operator-deploy.yaml carries it — on a cluster that taints its control
// plane NoSchedule (kubeadm's default, and any estate that later reserves its control
// plane) the selector says "only here" while the taint says "not here", and the pin
// silently becomes an unschedulable Job. Hardening the cluster must not break
// provisioning.
var (
	controlPlaneOnly = map[string]string{controlPlaneRoleLabel: controlPlaneRoleValue}

	tolerateControlPlane = []corev1.Toleration{{
		Key:      controlPlaneRoleLabel,
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
)

// Config is the orchestrator's deploy-time configuration.
type Config struct {
	Namespace        string
	ProvisionerImage string
	K3sServerURL     string
	K3sToken         string
	// ImagePullPolicy is set on the provisioner container ONLY when non-empty
	// (see jobSpec). Left empty, Kubernetes applies its own default — Always,
	// for the :latest tag ProvisionerImage normally carries — which is correct
	// in production but strands a node that cannot reach the registry even
	// when the image is already present locally (ImagePullBackOff on a real
	// image: the OPS-M2e incident). Never hardcode a policy here; the operator
	// must not decide production's default.
	ImagePullPolicy string
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

// ProbeResult is one reachability answer. Message is a short, client-safe reason when
// unreachable — a trimmed dial error, never bytes read from the peer, which the probe
// does not read at all.
type ProbeResult struct {
	Reachable bool
	LatencyMS int64
	Message   string
}

// Provisioner creates the Secret+Job and reads status back.
type Provisioner struct {
	cs  kubernetes.Interface
	cfg Config
	// probeTimeout is how long Probe waits for its Job, defaulted from the const of
	// the same name. A field rather than the const read directly so a test can drive
	// Probe — not just awaitProbe — down the timeout path in milliseconds, and so
	// exercise the deferred Job delete that path depends on. Not exported and not
	// configuration: production has exactly one correct value for it.
	probeTimeout time.Duration
}

func New(cs kubernetes.Interface, cfg Config) *Provisioner {
	return &Provisioner{cs: cs, cfg: cfg, probeTimeout: probeTimeout}
}

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
		Data: map[string][]byte{keySecretKey: r.SSHKey, k3sTokenKey: []byte(p.cfg.K3sToken)},
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

	container := corev1.Container{
		Name:  "provisioner",
		Image: p.cfg.ProvisionerImage,
		Env: []corev1.EnvVar{
			{Name: "PROVISION_TARGET_ADDR", Value: fmt.Sprintf("%s:%d", r.IP, port)},
			{Name: "PROVISION_SSH_USER", Value: r.SSHUser},
			{Name: "K3S_SERVER_URL", Value: p.cfg.K3sServerURL},
			{Name: "K3S_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
					Key:                  k3sTokenKey,
				},
			}},
			{Name: "PROVISION_SSH_KEY_FILE", Value: keyMountPath + "/" + keySecretKey},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "bootstrap-key", MountPath: keyMountPath, ReadOnly: true}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
			RunAsNonRoot: ptr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	// Only set when configured — an empty ImagePullPolicy field (the zero value)
	// leaves Kubernetes to apply its own default. Setting corev1.PullPolicy("")
	// explicitly would be indistinguishable from never having set it in the API,
	// but the `if` here keeps that equivalence obvious at the call site rather
	// than relying on the zero-value coincidence.
	if p.cfg.ImagePullPolicy != "" {
		container.ImagePullPolicy = corev1.PullPolicy(p.cfg.ImagePullPolicy)
	}

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
					// This pod mounts the bootstrap key and the k3s join token. See
					// controlPlaneOnly: the pin keeps cluster-admission credentials off
					// the fleet they admit.
					NodeSelector: controlPlaneOnly,
					Tolerations:  tolerateControlPlane,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr64(65532),
						// FSGroup must match RunAsUser. Kubernetes owns every Secret volume
						// file root:fsGroup, never root:<container-uid>; leave FSGroup unset
						// and the group is root, so a non-root container (required by
						// RunAsNonRoot above) can never read its own mounted credential no
						// matter how the file mode is set below. This was the live-cluster
						// defect: "read bootstrap key /etc/provision/ssh_key: permission denied".
						FSGroup:        ptr64(65532),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes: []corev1.Volume{{
						Name: "bootstrap-key",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: name,
							// 0o440 (r--r-----): root and the pod's fsGroup (set above) may
							// read; no world access. 0o400 looks stricter — and a reviewer
							// with no cluster reads it as good hygiene — but paired with a
							// non-root container it is unreadable, not strict: the owning
							// uid is root, not 65532, so owner-only read excludes the very
							// process meant to read it. This path had never actually been
							// exercised before the incident that found it, which is how the
							// defect survived review. Do NOT widen this to 0o444
							// (world-readable) or drop RunAsNonRoot/FSGroup instead — those
							// trade away a real security property this pairing keeps.
							DefaultMode: ptr32(0o440)}},
					}},
				},
			},
		},
	}
}

// probeTimeout bounds how long Probe waits for its Job to answer. Generous next to the
// binary's own 10s dial, because the wait also covers scheduling and possibly an image
// pull; the caller is a human waiting on a form, not a control loop.
//
// This is the INNERMOST of the three bounds on a Test Connection, and it must stay the
// smallest: the two layers above it (control.testConnectionTimeout, then the TUI's
// testConnTimeout) exist to let this one fire first, because only this layer can say
// "the probe job did not answer" rather than "something upstream timed out". test/arch
// asserts the ordering across the three packages.
const probeTimeout = 60 * time.Second

// probePollInterval is how often the probe pod's status is re-read. A poll, not a
// watch, deliberately: one pod living a few seconds does not justify an informer, and
// List with a label selector needs no RBAC the operator does not already have.
const probePollInterval = 250 * time.Millisecond

// probeTTL reclaims a finished probe Job even if this process dies before its own
// delete runs. The explicit delete in Probe is the primary reclaim; this is the belt.
const probeTTL int32 = 60

// probeDeleteTimeout bounds Probe's deferred cleanup delete. Short because the delete
// runs after the answer is already known and nothing waits on it, and because probeTTL
// reclaims the Job anyway if this call is the thing that fails.
const probeDeleteTimeout = 10 * time.Second

// Probe runs a one-shot reachability probe as an ephemeral Job and returns its result.
//
// The dial happens in the Job's pod, NOT in this process, and that placement is the
// whole point. The pod carries componentLabel=componentValue, so node-provisioner-egress
// grants it destination-open :22; the operator Deployment's own policy has no :22 rule
// at all. A caller-supplied probe target is by definition an arbitrary-destination SSH
// dial, so it lives for one dial in a throwaway pod instead of permanently in the
// always-running, gateway-reachable operator.
func (p *Provisioner) Probe(ctx context.Context, ip string, sshPort int32) (ProbeResult, error) {
	if ip == "" {
		return ProbeResult{}, fmt.Errorf("ip is required")
	}
	port := sshPort
	if port == 0 {
		port = 22
	}
	// A non-22 port CANNOT be probed, and saying so is the whole point of this branch.
	// node-provisioner-egress pins TCP:22 (infra/deploy/operator-deploy.yaml) — it is the
	// only port any pod in this namespace may open to an arbitrary destination — so a probe
	// of :2222 is dropped by policy before it leaves the node and comes back as a dial
	// error indistinguishable from a firewalled host. Reporting that as Reachable:false
	// would tell an operator their healthy node is down because of a NetworkPolicy they
	// cannot see from the Add Node form. This is an ERROR, not a result, for the same
	// reason parseProbeMessage refuses to decode a broken probe as an unreachable host.
	if port != 22 {
		return ProbeResult{}, fmt.Errorf("cannot probe port %d: only port 22 can be probed, because "+
			"cluster egress policy pins the provisioner's outbound SSH to :22 — provisioning itself "+
			"has the same constraint, so a node whose sshd is elsewhere cannot be added from here", port)
	}
	// Random name with no hostname in it: probes are concurrent (any operator at any
	// form can fire one) and, unlike AddNode, carry no per-host identity worth
	// preserving, so a collision must be impossible rather than merely unlikely.
	name := "probe-" + rand.String(8)

	job, err := p.cs.BatchV1().Jobs(p.cfg.Namespace).Create(ctx, p.probeJobSpec(name, ip, port), metav1.CreateOptions{})
	if err != nil {
		return ProbeResult{}, fmt.Errorf("create probe job: %w", err)
	}
	// Delete on EVERY path, including timeout and cancellation: a probe that leaked a
	// Job per attempt would fill the namespace with pods holding :22 egress. Background
	// propagation so the pod goes with the Job rather than being orphaned to the TTL,
	// and WithoutCancel so an abandoned RPC still cleans up after itself.
	//
	// WithoutCancel alone would strip the DEADLINE as well as the cancellation, and the
	// operator's client sets no Timeout of its own (cmd/operator/main.go builds it from
	// rest.InClusterConfig), so a wedged API server would park this gRPC handler
	// goroutine in `defer` forever. WithTimeout puts the bound back without reinstating
	// the caller's cancellation, which is the pairing this needs: cleanup must outlive
	// the RPC, but not the process.
	defer func() {
		bg := metav1.DeletePropagationBackground
		delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), probeDeleteTimeout)
		defer delCancel()
		_ = p.cs.BatchV1().Jobs(p.cfg.Namespace).Delete(delCtx, job.Name,
			metav1.DeleteOptions{PropagationPolicy: &bg})
	}()
	return p.awaitProbe(ctx, job.Name, p.probeTimeout)
}

// awaitProbe polls the probe pod until its container terminates, then reads the answer
// out of the termination message. timeout is a parameter rather than the const so a
// test can exercise the deadline path without spending a minute on it.
func (p *Provisioner) awaitProbe(ctx context.Context, jobName string, timeout time.Duration) (ProbeResult, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sel := probeJobLabel + "=" + jobName
	for {
		// waitCtx, NOT ctx: the select below only bounds the GAPS between polls, so a
		// List issued on the parent context is outside the deadline this function
		// advertises. The operator's client has no Timeout of its own (it comes from
		// rest.InClusterConfig), so one hung List on an unreachable API server would
		// park this gRPC handler goroutine indefinitely — with the bound in the
		// signature and in the error message, both lying.
		pods, err := p.cs.CoreV1().Pods(p.cfg.Namespace).List(waitCtx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return ProbeResult{}, fmt.Errorf("list probe pods for %s: %w", jobName, err)
		}
		for i := range pods.Items {
			// The probe pod runs exactly one container, so index 0 IS the probe.
			css := pods.Items[i].Status.ContainerStatuses
			if len(css) > 0 && css[0].State.Terminated != nil {
				return parseProbeMessage(css[0].State.Terminated.Message)
			}
		}
		select {
		case <-waitCtx.Done():
			// Separate the two ways the wait can end: the caller gave up, or the probe
			// did not answer. They mean different things to whoever reads the error.
			if ctx.Err() != nil {
				return ProbeResult{}, fmt.Errorf("probe job %s: %w", jobName, ctx.Err())
			}
			return ProbeResult{}, fmt.Errorf("probe job %s did not complete in time (waited %s)", jobName, timeout)
		case <-time.After(probePollInterval):
		}
	}
}

// parseProbeMessage decodes the single line cmd/kanz-provisioner writes to
// /dev/termination-log.
//
// An absent or unparseable message is an ERROR, never Reachable:false. "The probe could
// not run" and "the host is down" lead a human to completely different next actions —
// fix the estate versus fix the target — so reporting them identically would turn every
// broken probe into a false accusation against the target.
func parseProbeMessage(msg string) (ProbeResult, error) {
	fields := strings.Fields(msg)
	if len(fields) == 0 {
		return ProbeResult{}, fmt.Errorf("probe container exited without a termination message — " +
			"the probe did not run, which is not the same as an unreachable host")
	}
	switch fields[0] {
	case "reachable":
		if len(fields) < 2 {
			return ProbeResult{}, fmt.Errorf("probe reported reachable without a latency (%q)", msg)
		}
		ms, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("probe reported an unparseable latency %q", fields[1])
		}
		return ProbeResult{Reachable: true, LatencyMS: ms}, nil
	case "unreachable":
		// Held to the same strictness as the reachable branch, for the same reason: the
		// probe always writes a trimmed dial reason (cmd/kanz-provisioner/probe.go), so a
		// bare "unreachable" is a probe that malfunctioned, not a verdict. Defaulting the
		// message to "unreachable" would launder that into a confident accusation against
		// the target — the exact substitution this function exists to refuse.
		if len(fields) < 2 {
			return ProbeResult{}, fmt.Errorf("probe reported unreachable without a reason (%q)", msg)
		}
		return ProbeResult{Reachable: false, Message: strings.Join(fields[1:], " ")}, nil
	default:
		return ProbeResult{}, fmt.Errorf("probe wrote an unrecognised termination message %q", msg)
	}
}

// probeJobSpec builds the ephemeral probe Job. It mirrors jobSpec's pod shape — same
// ServiceAccount, same security context, same component label — because the label is
// what earns the :22 egress and the rest is what makes that pod acceptable to run. It
// differs in exactly two ways, both deliberate: no credential of any kind, and one
// attempt only.
func (p *Provisioner) probeJobSpec(name, ip string, port int32) *batchv1.Job {
	var backoff int32 = 0 // ONE dial, one answer. A retry would report a second dial's luck as
	//                         the first one's result; the caller asked a question, not for a Job
	//                         to eventually succeed.
	var deadline int64 = 120 // belt to the binary's 10s dial and Probe's 60s wait: a pod that
	//                          never gets scheduled must not outlive the operator process that
	//                          was going to delete it.
	ttl := probeTTL

	container := corev1.Container{
		Name:  "probe",
		Image: p.cfg.ProvisionerImage,
		// NO bootstrap-key Secret is created and NO volume is mounted. The probe opens a
		// TCP connection and closes it, so it needs no credential — and creating one "for
		// symmetry with jobSpec" would place key material in a pod that cannot use it, on
		// a path any caller can trigger at will.
		Env: []corev1.EnvVar{
			{Name: "PROVISION_MODE", Value: "probe"},
			{Name: "PROVISION_TARGET_ADDR", Value: fmt.Sprintf("%s:%d", ip, port)},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
			RunAsNonRoot: ptr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	// Same rule as jobSpec: set only when configured, so Kubernetes' own defaulting
	// decides otherwise. See the comment on Config.ImagePullPolicy for why the operator
	// must not choose production's default itself.
	if p.cfg.ImagePullPolicy != "" {
		container.ImagePullPolicy = corev1.PullPolicy(p.cfg.ImagePullPolicy)
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: p.cfg.Namespace,
			// The JOB object deliberately does NOT carry componentLabel: List() selects
			// provisioning Jobs by it, and a probe is not a provisioning — labelling the Job
			// would make every probe appear in ListProvisions as a phantom node.
			Labels: map[string]string{probeJobLabel: name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					// componentLabel IS THE ENTIRE MECHANISM by which this pod may reach :22 —
					// node-provisioner-egress selects on it, and nothing else in this namespace
					// grants that port. Remove it and the probe still runs, still succeeds, and
					// reports every target unreachable with a dial error indistinguishable from
					// a firewalled host. probeJobLabel is how awaitProbe finds this pod.
					Labels: map[string]string{componentLabel: componentValue, probeJobLabel: name},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "kanz-node-provisioner",
					RestartPolicy:      corev1.RestartPolicyNever,
					// Identical placement to jobSpec, from the same shared values. This pod
					// carries no credential, so the reason here is the second one in
					// controlPlaneOnly's comment: a probe that lands on a node which cannot
					// pull the image reports a healthy host unreachable.
					NodeSelector: controlPlaneOnly,
					Tolerations:  tolerateControlPlane,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr64(65532),
						// FSGroup matches RunAsUser as in jobSpec. Nothing here depends on it today
						// (no volume is mounted); it is kept identical so the two pod shapes cannot
						// drift, and so adding a mount later cannot resurrect the permission-denied
						// defect jobSpec's FSGroup comment describes.
						FSGroup:        ptr64(65532),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
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
