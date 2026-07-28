package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// The three env vars that decide, between them, WHO this Job is willing to talk to.
// Exactly one must be set; see hostKeyCallback for why "none" is an error rather than
// a default.
const (
	// envKnownHostsFile points at an OpenSSH known_hosts file. Preferred form: it is
	// the format `ssh-keyscan` emits and the format an operator already has tooling
	// for, and it carries the host identity alongside the key so a mismatch names the
	// host it was expecting.
	envKnownHostsFile = "PROVISION_KNOWN_HOSTS_FILE"
	// envHostKey pins ONE public key in authorized_keys format — the exact contents of
	// the target's /etc/ssh/ssh_host_ed25519_key.pub. For the case where the operator
	// has a fingerprint out of band but no file to mount.
	envHostKey = "PROVISION_HOST_KEY"
	// envInsecureSkip is the loud, deliberate escape hatch. It is spelled at this
	// length so that it cannot appear in a manifest by accident and cannot be read by
	// a reviewer as anything other than what it is.
	envInsecureSkip = "KANZ_PROVISIONER_INSECURE_SKIP_HOST_KEY_VERIFY"
)

// warnOut is where the insecure-mode warning goes. A var, not os.Stderr inline, so a
// test can prove the warning is actually emitted — the same reason probe.go makes
// terminationLogPath a var. An escape hatch whose warning is untested is an escape
// hatch that will one day be silent.
var warnOut io.Writer = os.Stderr

// hostKeyCallback resolves this Job's host-key policy from env, or refuses to produce
// one at all.
//
// THIS USED TO BE ssh.InsecureIgnoreHostKey(), and the comment that justified it read:
// "this is FIRST CONTACT with a freshly-provisioned host whose key we do not and cannot
// yet know. The bootstrap trust is the operator-supplied key plus the ephemeral,
// RBAC-gated, NetworkPolicy-scoped Job this runs in — not TOFU host verification." That
// text was copied verbatim from the S2a node-provisioning plan, written before any of
// it ran and never re-derived against the code that shipped. (That plan lived under
// docs/, deleted 2026-07-29; it is in git history, and the fact that a justification
// outlived the document it came from — and was still wrong — is the reason it is
// quoted here in full rather than cited.) Both of its halves are wrong, and they are
// wrong in different ways, so both are recorded here.
//
// THE COMPENSATING CONTROLS IT NAMES ARE ALL CLIENT-SIDE. RBAC decides who may ask for a
// provision. The NetworkPolicy decides which PORT this pod may open — node-provisioner-egress
// grants destination-open :22, which is to say ANY destination on :22; provision.go's own
// Probe error text says so in as many words. The bootstrap key authenticates US TO THE
// SERVER. Not one of the three constrains WHICH HOST ANSWERS THE DIAL. They were offered
// as a substitute for server authentication and they never performed it, so this was
// never a trade — it was a gap with a paragraph in front of it.
//
// WHAT THE ATTACKER GETS, CONCRETELY. The single session this callback guards runs
// k3sInstallCmd (join.go), which pipes `K3S_URL='...' K3S_TOKEN='...'` into a shell on
// the far end. K3S_TOKEN is the cluster-admission token — the one credential that lets
// an arbitrary host join this estate as a worker. Trusting any host key means anyone who
// can answer that TCP connect — an on-path attacker, a stale DNS or DHCP lease, an IP
// reassigned since the operator last looked, or a fat-fingered octet in the Add Node
// form — is handed it in plaintext at the application layer, and then chooses the exit
// status too: they reply 0, the Job succeeds, and ListProvisions reports Joined for a
// node that was never provisioned. The estate believes in a host that does not exist
// while the attacker joins one that does.
//
// That is the same credential provision.go pins both Jobs to the control plane to
// protect — "the credential that admits the fleet onto a member of the fleet it admits.
// The blast radius of one compromised worker stops being that worker." The estate went
// to the trouble of a nodeSelector, a toleration, an fsGroup and a 0o440 mount mode to
// keep K3S_TOKEN off the fleet, and then read it out to whoever picked up the phone.
//
// "WE CANNOT YET KNOW THE KEY" WAS ALSO FALSE. Somebody put the bootstrap key's public
// half into that host's authorized_keys before anyone typed its address into a form.
// Whatever channel carried that — a cloud-init template, an image, a hand — carries a
// host key fingerprint just as well; `ssh-keyscan` on the machine that did it produces
// the file this reads. It was not unknowable. It was un-plumbed.
//
// It is still un-plumbed above this line: AddNodeRequest
// (kanz-schemas/proto/operator/v1/operator.proto:129) has no host-key field, so the
// operator sets none of these vars today and every provision fails closed here until it
// does. That is the intended state. A provisioner that refuses to run is an outage in a
// feature; a provisioner that hands the cluster-admission token to strangers is an
// outage in the estate, and only one of the two announces itself.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	knownHostsFile := os.Getenv(envKnownHostsFile)
	hostKey := os.Getenv(envHostKey)

	skip, err := insecureSkipRequested()
	if err != nil {
		return nil, err
	}

	// EXACTLY ONE, counted before any of them is honoured. Two sources set means the
	// caller holds two different beliefs about who the far end is, and picking one by
	// precedence would resolve that silently — in the direction of whichever branch
	// happened to be written first. Refusing makes the operator say which it meant.
	// This matters most for the pairing that looks harmless: a manifest that carries a
	// real known_hosts AND a left-over insecure flag from a debugging session verifies
	// nothing, and under any precedence rule at all, one of those two is a lie.
	set := make([]string, 0, 3)
	if knownHostsFile != "" {
		set = append(set, envKnownHostsFile)
	}
	if hostKey != "" {
		set = append(set, envHostKey)
	}
	if skip {
		set = append(set, envInsecureSkip)
	}
	switch len(set) {
	case 1:
	case 0:
		return nil, fmt.Errorf("no SSH host-key policy configured: set %s to a known_hosts file, "+
			"or %s to the target's public host key in authorized_keys format. Refusing to connect "+
			"without verifying the host: this session carries K3S_TOKEN, the cluster-admission "+
			"credential, to whatever answers the dial",
			envKnownHostsFile, envHostKey)
	default:
		return nil, fmt.Errorf("conflicting SSH host-key policy: %s are all set — "+
			"set exactly one", strings.Join(set, ", "))
	}

	switch {
	case skip:
		// Deliberately not a one-liner about verification being disabled. This line is
		// the only artefact left in the logs of a run that trusted anyone; it names the
		// credential at stake so that whoever greps it later does not have to come back
		// to this file to learn what it cost.
		fmt.Fprintf(warnOut, "kanz-provisioner: WARNING: %s is set — SSH host key verification is DISABLED. "+
			"This session will hand K3S_TOKEN (cluster admission) to whatever host answers the dial, "+
			"and will report success on whatever it replies. Never set this outside a disposable test rig.\n",
			envInsecureSkip)
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // gated above; see the warning it just printed
	case knownHostsFile != "":
		return knownHostsCallback(knownHostsFile)
	default:
		return fixedHostKeyCallback(hostKey)
	}
}

// insecureSkipRequested reads the escape hatch.
//
// An unparseable value is an ERROR and not a quiet false. Quiet false is the safe
// direction, which is exactly why it is the wrong one to be quiet about: an operator who
// wrote `=yes` in a test rig gets host key verification, a confusing handshake failure,
// and no hint that the var they set was discarded. The same shape as main.go's refusal
// to treat an unknown PROVISION_MODE as "join" — a typo must not silently select a
// behaviour nobody asked for, in either direction.
func insecureSkipRequested() (bool, error) {
	raw := os.Getenv(envInsecureSkip)
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a boolean (want true/false)", envInsecureSkip, raw)
	}
	return v, nil
}

// knownHostsCallback verifies against an OpenSSH known_hosts file.
//
// A MISSING OR UNREADABLE FILE IS A HARD ERROR. The tempting alternative — treat an
// absent file as "nothing pinned yet" and continue — reintroduces the whole defect
// through the one path most likely to occur in production: a Secret that failed to
// mount, a volume renamed, a typo in the path. Those are precisely the conditions under
// which nobody is watching, and the failure would be invisible because the provision
// would succeed.
func knownHostsCallback(path string) (ssh.HostKeyCallback, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("read %s %s: %w (refusing to connect unverified)", envKnownHostsFile, path, err)
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("parse known_hosts %s: %w", path, err)
	}

	// Wrapped to separate the two failures knownhosts returns as one type, because they
	// mean opposite things to whoever reads the Job's logs. "Not in the file" is an
	// estate misconfiguration — go add the entry. "Present and DIFFERENT" is either a
	// rebuilt host or an interception attempt, and the correct response is to stop and
	// find out which, not to update the file until it goes green.
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) {
			if len(ke.Want) > 0 {
				return fmt.Errorf("SSH HOST KEY MISMATCH for %s: it offered a %s key that is NOT the one "+
					"pinned in %s. Either the host was rebuilt, or this connection is not reaching the host "+
					"you think it is. Do not proceed until you know which — this session carries K3S_TOKEN: %w",
					hostname, key.Type(), path, err)
			}
			return fmt.Errorf("host %s is not in known_hosts %s (it offered a %s key). Add its host key "+
				"there; this session will not trust an unverified host: %w", hostname, path, key.Type(), err)
		}
		var re *knownhosts.RevokedError
		if errors.As(err, &re) {
			return fmt.Errorf("host %s offered a key REVOKED in %s: %w", hostname, path, err)
		}
		return fmt.Errorf("verify host key for %s against %s: %w", hostname, path, err)
	}, nil
}

// fixedHostKeyCallback pins the single public key in line, which is authorized_keys
// format — the verbatim contents of the target's /etc/ssh/ssh_host_ed25519_key.pub, or a
// line of `ssh-keyscan` output with the leading host field removed.
func fixedHostKeyCallback(line string) (ssh.HostKeyCallback, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w (want authorized_keys format, e.g. \"ssh-ed25519 AAAA...\")", envHostKey, err)
	}
	fixed := ssh.FixedHostKey(pub)

	// ssh.FixedHostKey's own error is the bare string "ssh: host key mismatch", which in
	// a Job's last log line before it exits non-zero is not enough to act on: it names
	// neither the host nor which of the two keys was which.
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := fixed(hostname, remote, key); err != nil {
			return fmt.Errorf("SSH HOST KEY MISMATCH for %s: it offered a %s key, but %s pins a %s key. "+
				"Either the host was rebuilt, or this connection is not reaching the host you think it is. "+
				"Do not proceed until you know which — this session carries K3S_TOKEN: %w",
				hostname, key.Type(), envHostKey, pub.Type(), err)
		}
		return nil
	}, nil
}
