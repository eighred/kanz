"""Temporary read-only claim-shape diagnostic, not tool qualification."""
import json
import os
from pathlib import Path
import re
import tempfile

import consume as c
import qualify as q

KNOWN_TYPES = {"https://sigstore.dev/cosign/sign/v1", "cosign container image signature"}


def project(raw, source, run, attempt):
    if not re.fullmatch(r"[0-9a-f]{40}", source) or not all(re.fullmatch(r"[1-9][0-9]{0,19}", value) for value in (run, attempt)):
        raise ValueError("invalid diagnostic identity")
    claims = json.loads(raw, object_pairs_hook=q.strict)
    if not isinstance(claims, list) or not 1 <= len(claims) <= 16:
        raise ValueError("unexpected verified claim count")
    result = []
    for claim in claims:
        if not isinstance(claim, dict): raise ValueError("unexpected verified claim shape")
        critical, optional = claim.get("critical"), claim.get("optional")
        if not isinstance(critical, dict) or not isinstance(optional, dict): raise ValueError("unexpected verified claim shape")
        identity, image = critical.get("identity"), critical.get("image")
        if not isinstance(identity, dict) or not isinstance(image, dict): raise ValueError("unexpected verified claim shape")
        kind = critical.get("type")
        if kind not in KNOWN_TYPES: raise ValueError("unknown verified claim type")
        reference = identity.get("docker-reference")
        result.append({"critical_type": kind,
                       "reference": "expected-immutable" if reference == c.IMAGE else "bare-repository" if reference == q.REPOSITORY else "other",
                       "digest_matches": image.get("docker-manifest-digest") == c.DIGEST,
                       "source_matches": optional.get("source_sha") == c.PRODUCER_SOURCE})
    return {"diagnostic_only": True, "consumer_source": source, "run": run, "attempt": attempt, "count": len(result), "claims": result}


def main():
    source = q.source()
    with tempfile.TemporaryDirectory(prefix="verified-claim-probe-", dir=os.environ["RUNNER_TEMP"]) as directory:
        raw = Path(directory) / "verified.json"
        with raw.open("wb") as output:
            q.run(["cosign", "verify", "--certificate-oidc-issuer", q.ISSUER,
                   "--certificate-identity", q.SIGNER, "-a", "source_sha=" + c.PRODUCER_SOURCE,
                   c.IMAGE], stdout=output)
        result = project(q.read_bounded(raw, 1024 * 1024), source, os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"])
        destination = Path(os.environ["RUNNER_TEMP"]) / "verified-tool-claim-projection.json"
        destination.write_text(json.dumps(result, sort_keys=True) + "\n", encoding="utf8")
    print("verified public claim projection retained; diagnostic only, no payload tools executed")


if __name__ == "__main__":
    main()
