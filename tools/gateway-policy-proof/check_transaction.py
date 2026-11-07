"""Bind transaction evidence to this exact checkout and workflow attempt."""
import json
import os
from pathlib import Path
import sys
import tempfile

import proof
from transaction_fixture import CASES, validate_report
from image_identity import public_identity
from bounded_process import run as bounded_run


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
            return bounded_run(args, cwd=proof.ROOT, env=env, timeout=60)
        _, _, render_hash = proof.render(run)
        validate_report(report, run(["git", "rev-parse", "HEAD"]), run(["git", "rev-parse", "HEAD^{tree}"]),
                        os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"], render_hash)
    print(f"All {len(CASES)} owned-cluster policy transaction/probe cases and cleanup passed")


if __name__ == "__main__":
    main(Path(sys.argv[1]))
