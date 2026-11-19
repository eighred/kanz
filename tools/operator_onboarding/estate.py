"""Two owned Ubuntu VMs for the GHCR onboarding qualification."""

from contextlib import contextmanager
import ipaddress
import json
import os
from pathlib import Path
import subprocess
import time
import uuid


IMAGE = "a80e9c9fd0d484dec024da3e9e264bbad4ce159b5312d6e149ef6ad532e18e43"
K3S_VERSION = "v1.36.4+k3s1"
K3S_SHA = "835873f37245fc615f547a2fe2af9402a347875f13fa64a1f136de644955ea3f"


def run(args, *, data=None, timeout=120, environment=None, public_error=None):
    # Arguments, streams and environment may contain fixture credentials.
    # Report only executable and return code; never include a TimeoutExpired
    # traceback containing the original command line.
    try:
        result = subprocess.run(args, input=data, text=True, capture_output=True,
                                env=child_env() if environment is None else environment, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise RuntimeError(f"{Path(args[0]).name} timed out after {timeout}s") from None
    if result.returncode:
        if public_error:
            raise RuntimeError(public_error(result.stderr))
        raise RuntimeError(f"{Path(args[0]).name} exited {result.returncode}")
    return result.stdout.strip()


def child_env():
    # Registry identity belongs only to the Kubernetes Secret creation.
    return {k: v for k, v in os.environ.items() if k not in
            {"GHCR_JOB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN"}}


def guest(vm, *args, data=None, timeout=120):
    return run(["lxc", "exec", vm, "--", *args], data=data, timeout=timeout)


def inventory():
    return {vm["name"]: vm for vm in json.loads(run(["lxc", "list", "--format=json"]))}


def machine_address(vm):
    listing = json.loads(guest(vm, "ip", "-json", "-4", "address", "show", "scope", "global"))
    addresses = [a["local"] for link in listing for a in link["addr_info"]
                 if a["family"] == "inet" and link["ifname"] != "lo"]
    if len(addresses) != 1 or not ipaddress.ip_address(addresses[0]).is_private:
        raise RuntimeError("expected one private VM address before cluster startup")
    return addresses[0]


@contextmanager
def estate():
    if (os.environ.get("GITHUB_ACTIONS") != "true" or
            os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted" or
            os.environ.get("GITHUB_REPOSITORY") != "eighred/kanz" or
            not Path("/dev/kvm").is_char_device()):
        raise RuntimeError("onboarding requires the repository's disposable hosted KVM runner")
    lease = uuid.uuid4().hex[:12]
    names = [f"kanz77-{lease}-{role}" for role in ("control", "worker")]
    if inventory():
        raise RuntimeError("refusing a nonempty LXD estate")
    try:
        for name, memory in zip(names, ("5GiB", "3GiB"), strict=True):
            run(["lxc", "launch", "ubuntu:" + IMAGE, name, "--vm",
                 "-c", f"user.kanz_lease={lease}", "-c", f"limits.memory={memory}",
                 "-c", "limits.cpu=2"], timeout=600)
            deadline = time.monotonic() + 180
            while True:
                try:
                    guest(name, "true", timeout=10)
                    break
                except RuntimeError:
                    if time.monotonic() >= deadline:
                        raise RuntimeError(f"VM guest agent unavailable on {name}") from None
                    time.sleep(2)
            guest(name, "cloud-init", "status", "--wait", timeout=180)
            if guest(name, "hostname", "-s") != name:
                raise RuntimeError("guest hostname disagrees with the leased instance")
        yield tuple(names)
    finally:
        current = inventory()
        for name in reversed(names):
            if name in current:
                if current[name]["config"].get("user.kanz_lease") != lease:
                    raise RuntimeError(f"refusing to delete a foreign VM named {name}")
                run(["lxc", "delete", "--force", name], timeout=120)
        if any(name in inventory() for name in names):
            raise RuntimeError("leased VMs survived cleanup")
        print("Released both VM names; no leased instance remains.", flush=True)


def start_server(control):
    # Credentials, datastore and SVIDs remain inside the disposable guest.
    # Mount before starting k3s. Leave container images on the VM's own disk.
    guest(control, "sh", "-eu", "-c", """
        swapoff -a
        mkdir -p /var/lib/rancher/k3s/server
        mount -t tmpfs -o mode=0700,size=512m tmpfs /var/lib/rancher/k3s/server
        umask 077
        head -c 32 /dev/urandom | base64 > /run/kanz77-agent-token
        curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
            -o /usr/local/bin/k3s https://github.com/k3s-io/k3s/releases/download/v1.36.4%2Bk3s1/k3s
        echo "$1  /usr/local/bin/k3s" | sha256sum --check --status
        chmod 0755 /usr/local/bin/k3s
        systemd-run --unit=kanz77-server --property=Delegate=yes \
            /usr/local/bin/k3s server --secrets-encryption --agent-token-file /run/kanz77-agent-token \
            --disable=traefik --disable=servicelb
    """, "fixture", K3S_SHA, timeout=360)
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        try:
            guest(control, "k3s", "kubectl", "get", "node", control, timeout=8)
            break
        except RuntimeError:
            time.sleep(2)
    guest(control, "k3s", "kubectl", "wait", "--for=condition=Ready",
          "node/" + control, "--timeout=120s", timeout=130)
    if "Encryption Status: Enabled" not in guest(control, "k3s", "secrets-encrypt", "status"):
        raise RuntimeError("control-plane secret encryption is unavailable")


def prepare_worker(worker):
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

    private = Ed25519PrivateKey.generate()
    public = private.public_key().public_bytes(serialization.Encoding.OpenSSH,
                                               serialization.PublicFormat.OpenSSH).decode("ascii")
    guest(worker, "sh", "-eu", "-c", """
        if command -v k3s >/dev/null 2>&1; then exit 1; fi
        test ! -e /var/lib/rancher/k3s
        test ! -e /etc/rancher/k3s/registries.yaml
        swapoff -a
        install -d -m 0700 -o ubuntu -g ubuntu /home/ubuntu/.ssh
        test ! -s /home/ubuntu/.ssh/authorized_keys
        cat > /home/ubuntu/.ssh/authorized_keys
        chown ubuntu:ubuntu /home/ubuntu/.ssh/authorized_keys
        chmod 0600 /home/ubuntu/.ssh/authorized_keys
        printf 'INSTALL_K3S_VERSION=%s\n' "$1" >> /etc/environment
        systemctl start ssh
    """, "fixture", K3S_VERSION, data=public + "\n")
    key = private.private_bytes(serialization.Encoding.PEM,
                                serialization.PrivateFormat.OpenSSH,
                                serialization.NoEncryption()).decode("ascii")
    host_key = guest(worker, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
    return host_key, key
