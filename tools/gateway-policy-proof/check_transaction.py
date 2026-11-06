"""Bind transaction evidence to this exact checkout and workflow attempt."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

import proof
from transaction_fixture import validate_report
from image_identity import public_identity


def main(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 65536:
        raise ValueError("invalid transaction artifact")
    report = json.loads(path.read_bytes())
    if report.get("imageIdentity") != public_identity():
        raise ValueError("transaction runtime image differs from fixed public graph")
    with tempfile.TemporaryDirectory(prefix="kanz-policy-check-") as directory:
        env = {key: os.environ[key] for key in ("PATH", "SYSTEMROOT") if key in os.environ}
        env.update(HOME=directory, TMPDIR=directory, LANG="C.UTF-8")
        def run(args):
            result = subprocess.run(args, cwd=proof.ROOT, env=env, capture_output=True, text=True, timeout=60, check=True)
            if len(result.stdout.encode()) > 4 << 20:
                raise ValueError("checker output exceeds bound")
            return result.stdout.strip()
        _, _, render_hash = proof.render(run)
        validate_report(report, run(["git", "rev-parse", "HEAD"]), run(["git", "rev-parse", "HEAD^{tree}"]),
                        os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"], render_hash)
    print("All 14 owned-cluster policy transaction/probe cases and cleanup passed")


if __name__ == "__main__":
    main(Path(sys.argv[1]))
