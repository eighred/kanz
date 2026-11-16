"""Install only the reviewed signed public-tool payload; never build a fallback."""
import json
import os
import re
from pathlib import Path
import shutil
import tempfile

import qualify as q

PRODUCER_SOURCE = "a2dbaf41fd77da6dc30380f133e80802488fe7c2"
CONFIG = "sha256:e523902a82786dd3dfb6aa9c5b155b9516c11fd22f86cbb7db6d9bb36f762532"
DIGEST = "sha256:0579c6075afce18b98b97864a056d0e553d0281e6d12c598ab8e3042ae88de71"
IMAGE = q.REPOSITORY + "@" + DIGEST


def signature_claims(raw):
    claims = json.loads(raw, object_pairs_hook=q.strict)
    if not isinstance(claims, list) or not claims:
        raise ValueError("verified producer signature missing")
    for claim in claims:
        if (claim.get("critical", {}).get("type") != "https://sigstore.dev/cosign/sign/v1"
                or claim.get("critical", {}).get("image", {}).get("docker-manifest-digest") != DIGEST
                or claim.get("critical", {}).get("identity", {}).get("docker-reference") != IMAGE
                or claim.get("optional", {}).get("source_sha") != PRODUCER_SOURCE):
            raise ValueError("verified producer signature binding differs")


def install(runner_temp, consumer_source, run, attempt):
    if not all(isinstance(value, str) and re.fullmatch(r"[1-9][0-9]{0,19}", value) for value in (run, attempt)):
        raise ValueError("invalid consumer run identity")
    # Producer identity describes the artifact, not the caller's checkout. Never
    # assign GITHUB_SHA or make the public producer revision a consumer stamp.
    with tempfile.TemporaryDirectory(prefix="rust-audit-readback-", dir=runner_temp) as scratch:
        temp = Path(scratch)
        signatures = temp / "signatures.json"
        with signatures.open("wb") as stream:
            q.run(["cosign", "verify", "--certificate-oidc-issuer", q.ISSUER,
                   "--certificate-identity", q.SIGNER, "-a", "source_sha=" + PRODUCER_SOURCE,
                   IMAGE], stdout=stream)
        signature_claims(q.read_bounded(signatures, 1024 * 1024))
        q.run(["docker", "pull", "--platform", "linux/amd64", IMAGE])
        # This local tag only makes Docker SAVE's exact allowlist deterministic.
        # It is assigned from the already signature-verified immutable reference.
        q.run(["docker", "tag", IMAGE, q.local_tag(PRODUCER_SOURCE)])
        archive = temp / "image.tar"
        q.run(["docker", "save", "--output", str(archive), q.local_tag(PRODUCER_SOURCE)])
        image, files = q.extract_saved(q.read_bounded(archive, q.MAX_PAYLOAD), CONFIG, temp, PRODUCER_SOURCE)
        advisories = q.exercise(temp)
        installed = Path(tempfile.mkdtemp(prefix="kanz-rust-audit-tools-", dir=runner_temp))
        for tool in q.TOOLS:
            shutil.copyfile(temp / tool, installed / tool)
            (installed / tool).chmod(0o755)
        receipt = {"run": run, "attempt": attempt, "consumer_source": consumer_source, "producer_source": PRODUCER_SOURCE,
                   "image": IMAGE, "config": image, "issuer": q.ISSUER, "signer": q.SIGNER,
                   "source_annotation": PRODUCER_SOURCE, "files": files, "advisory_commits": advisories}
        (Path(runner_temp) / "rust-audit-tools-installed.json").write_text(json.dumps(receipt, sort_keys=True) + "\n", encoding="utf8")
        return installed


def main():
    consumer_source = q.source()
    installed = install(Path(os.environ["RUNNER_TEMP"]), consumer_source,
                        os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"])
    with Path(os.environ["GITHUB_PATH"]).open("a", encoding="utf8") as path:
        path.write(str(installed) + "\n")
    print("signed immutable Rust audit tools installed; fresh audits remain mandatory")


if __name__ == "__main__":
    main()
