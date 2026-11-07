"""Trusted-host read-only SSM/Git provenance collector for the retained plan."""
import json
import os
from pathlib import Path
import shutil
import sys

from bounded_process import run
import recovery_provenance as provenance
import recovery_receipts as receipts
import transaction as tx


def main():
    if len(sys.argv) != 2:
        raise tx.Refusal("fixed host repository argument required")
    root = Path(sys.argv[1]).resolve(strict=True)
    raw = sys.stdin.buffer.read(524289)
    if len(raw) > 524288:
        raise tx.Refusal("canonical projection exceeds bound")
    projection = receipts.document(raw)
    if set(projection) != {"policies", "workloads"}:
        raise tx.Refusal("canonical projection shape differs")
    aws, git = shutil.which("aws"), shutil.which("git")
    if not aws or not git:
        raise tx.Refusal("trusted host tools unavailable")
    def command(argv, limit=131072, strip=True):
        return run(argv, cwd=str(root), env=dict(os.environ), timeout=45, stdout_limit=limit, strip=strip)
    def read_git(path):
        allowed = {"tools/Invoke-TokyoGatewayPolicy.ps1", "tools/gateway-policy-proof/transport_codec.py",
                   *("tools/gateway-policy-proof/" + name for name in provenance.FILES)}
        if path not in allowed:
            raise tx.Refusal("old source read outside closed paths")
        value = command([git, "show", receipts.OLD_SOURCE + ":" + path], 65536, strip=False)
        if not value or len(value.encode()) > 65536:
            raise tx.Refusal("old source unavailable")
        return value
    common = [aws, "--profile", "kanz-platform", "--region", "ap-northeast-1"]
    commands = receipts.document(command([*common, "ssm", "list-commands", "--command-id", receipts.COMMAND,
        "--no-paginate", "--output", "json"]).encode())
    if set(commands) != {"Commands"} or not isinstance(commands["Commands"], list) or len(commands["Commands"]) != 1:
        raise tx.Refusal("original SSM command unavailable")
    invocation = receipts.document(command([*common, "ssm", "get-command-invocation", "--command-id", receipts.COMMAND,
        "--instance-id", provenance.INSTANCE, "--output", "json"]).encode())
    result = provenance.verify(commands["Commands"][0], invocation, read_git, projection["policies"], projection["workloads"])
    print(json.dumps(result, separators=(",", ":")))


if __name__ == "__main__":
    main()
